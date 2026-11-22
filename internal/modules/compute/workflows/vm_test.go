package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"gorm.io/gorm"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type vmStore struct {
	service.Store
	mu       sync.Mutex
	reqs     map[uuid.UUID]*domain.Request
	vps      map[uuid.UUID]*domain.VPS
	finished map[uuid.UUID]bool
}

func (s *vmStore) Request(_ context.Context, id uuid.UUID) (*domain.Request, error) {
	return s.reqs[id], nil
}
func (s *vmStore) FinishRequest(_ context.Context, id uuid.UUID, ok bool, _ string, _ time.Time) error {
	s.finished[id] = ok
	return nil
}
func (s *vmStore) VPS(_ context.Context, id uuid.UUID) (*domain.VPS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vps[id]
	if !ok {
		return nil, domain.ErrVPSNotFound
	}
	c := *v
	return &c, nil
}
func (s *vmStore) UpdateVPS(_ context.Context, id uuid.UUID, f map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.vps[id]
	for k, val := range f {
		switch k {
		case "state":
			v.State = val.(string)
		case "provider_id":
			v.ProviderID = val.(string)
		case "provider_order_id":
			v.ProviderOrderID = val.(string)
		case "failure_code":
			v.FailureCode = val.(string)
		case "ipv4":
			v.IPv4 = val.(string)
		case "paid_until":
			v.PaidUntil = nil
		}
	}
	v.Version++
	return nil
}
func (s *vmStore) Offer(context.Context, string) (*domain.Offer, error) {
	return &domain.Offer{Code: "VPS-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649,
		Images: json.RawMessage(`{"ubuntu-24.04":"Ubuntu 24.04"}`)}, nil
}
func (s *vmStore) EnqueueTx(*gorm.DB, *domain.Request) error { return nil }

type fakeCloud struct {
	service.ComputeProvider
	mu         sync.Mutex
	orderErr   error
	orderState string
	orders     int
	rebuilt    string
	state      string
	powered    []string
}

func (f *fakeCloud) Order(_ context.Context, s service.OrderSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders++
	if f.orderErr != nil {
		return "", f.orderErr
	}
	return "9001", nil
}
func (f *fakeCloud) OrderState(context.Context, string) (string, string, error) {
	return f.orderState, "vps-a.vps.ovh.net", nil
}
func (f *fakeCloud) Get(context.Context, string) (*service.ProviderVM, error) {
	return &service.ProviderVM{ProviderID: "vps-a.vps.ovh.net", State: f.state, IPv4: "51.68.1.2"}, nil
}
func (f *fakeCloud) Rebuild(_ context.Context, _, image, key string) (string, error) {
	f.rebuilt = image + "|" + key
	return "55", nil
}
func (f *fakeCloud) Task(context.Context, string, string) (*service.ProviderTask, error) {
	return &service.ProviderTask{State: service.TaskDone}, nil
}
func (f *fakeCloud) Stop(context.Context, string) (string, error) {
	f.powered = append(f.powered, "stop")
	return "56", nil
}
func (f *fakeCloud) Start(context.Context, string) (string, error) {
	f.powered = append(f.powered, "start")
	return "57", nil
}

type fakeWallet struct {
	service.Wallet
	refunds map[string]billingDomain.Milli
}

func (w *fakeWallet) RefundCompute(_ context.Context, _ uuid.UUID, amount billingDomain.Milli, ref, _ string) error {
	w.refunds[ref] = amount
	return nil
}

func setup(state, kind string) (*vmStore, uuid.UUID, uuid.UUID) {
	vid, rid := uuid.New(), uuid.New()
	payload, _ := json.Marshal(domain.CreatePayload{SSHPublicKey: "ssh-ed25519 AAAA", Period: "compute:vps:x:2026-11-22", AmountMilli: 9_000_000})
	s := &vmStore{
		reqs:     map[uuid.UUID]*domain.Request{rid: {ID: rid, VPSID: &vid, Kind: kind, Payload: payload}},
		vps:      map[uuid.UUID]*domain.VPS{vid: {ID: vid, Name: "web", Offer: "VPS-1", Location: "gra", Image: "ubuntu-24.04", State: state}},
		finished: map[uuid.UUID]bool{},
	}
	return s, vid, rid
}

func runVM(t *testing.T, store *vmStore, cloud *fakeCloud, wallet *fakeWallet, rid uuid.UUID, kind string) error {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store, Provider: cloud, Orderer: cloud, Wallet: wallet})
	env.ExecuteWorkflow(RequestWorkflow, service.RequestInput{RequestID: rid.String(), Kind: kind})
	require.True(t, env.IsWorkflowCompleted())
	return env.GetWorkflowError()
}

func TestCreateVM(t *testing.T) {
	store, vid, rid := setup(domain.StatePending, KindCreate)
	cloud := &fakeCloud{orderState: service.OrderDelivered, state: domain.StateRunning}
	wallet := &fakeWallet{refunds: map[string]billingDomain.Milli{}}
	require.NoError(t, runVM(t, store, cloud, wallet, rid, KindCreate))

	v := store.vps[vid]
	assert.Equal(t, domain.StateRunning, v.State)
	assert.Equal(t, "vps-a.vps.ovh.net", v.ProviderID)
	assert.Equal(t, "9001", v.ProviderOrderID)
	assert.Equal(t, "51.68.1.2", v.IPv4)
	assert.Equal(t, "Ubuntu 24.04|ssh-ed25519 AAAA", cloud.rebuilt, "système de l'offre, clé du client")
	assert.Equal(t, 1, cloud.orders)
	assert.Empty(t, wallet.refunds)
	assert.True(t, store.finished[rid])
}

func TestCreateVM_OrderFailsRefunded(t *testing.T) {
	store, vid, rid := setup(domain.StatePending, KindCreate)
	cloud := &fakeCloud{orderState: service.OrderFailed}
	wallet := &fakeWallet{refunds: map[string]billingDomain.Milli{}}
	require.Error(t, runVM(t, store, cloud, wallet, rid, KindCreate))

	v := store.vps[vid]
	assert.Equal(t, domain.StateFailed, v.State)
	assert.Equal(t, domain.FailureProvider, v.FailureCode)
	assert.Nil(t, v.PaidUntil)
	assert.Equal(t, billingDomain.Milli(9_000_000), wallet.refunds["compute:vps:x:2026-11-22:refund"])
	ok, closed := store.finished[rid]
	assert.True(t, closed)
	assert.False(t, ok)
}

func TestCreateVM_PriceAboveNeverRetried(t *testing.T) {
	store, vid, rid := setup(domain.StatePending, KindCreate)
	cloud := &fakeCloud{orderErr: errors.Join(service.ErrProviderPriceAbove, errors.New("720 > 649"))}
	wallet := &fakeWallet{refunds: map[string]billingDomain.Milli{}}
	require.Error(t, runVM(t, store, cloud, wallet, rid, KindCreate))
	assert.Equal(t, 1, cloud.orders, "commande jamais rejouée")
	assert.Equal(t, domain.FailurePriceChanged, store.vps[vid].FailureCode)
	assert.Len(t, wallet.refunds, 1)
}

func TestCreateVM_DeliveryTimeout(t *testing.T) {
	store, vid, rid := setup(domain.StatePending, KindCreate)
	cloud := &fakeCloud{orderState: service.OrderPending}
	wallet := &fakeWallet{refunds: map[string]billingDomain.Milli{}}
	require.Error(t, runVM(t, store, cloud, wallet, rid, KindCreate))
	assert.Equal(t, domain.FailureTimeout, store.vps[vid].FailureCode)
	assert.Len(t, wallet.refunds, 1)
}

func TestStopKeepsSuspended(t *testing.T) {
	store, vid, rid := setup(domain.StateSuspended, KindStop)
	store.vps[vid].ProviderID = "vps-a.vps.ovh.net"
	store.vps[vid].FailureCode = domain.FailureUnpaid
	cloud := &fakeCloud{state: domain.StateRunning}
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{}, rid, KindStop))
	assert.Equal(t, []string{"stop"}, cloud.powered)
	assert.Equal(t, domain.StateSuspended, store.vps[vid].State, "reste suspendu une fois arrêté")
}

func TestStartAfterPayment(t *testing.T) {
	store, vid, rid := setup(domain.StateSuspended, KindStart)
	store.vps[vid].ProviderID = "vps-a.vps.ovh.net"
	store.vps[vid].FailureCode = domain.FailureUnpaid
	cloud := &fakeCloud{state: domain.StateStopped}
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{}, rid, KindStart))
	assert.Equal(t, []string{"start"}, cloud.powered)
	assert.Equal(t, domain.StateRunning, store.vps[vid].State)
	assert.Equal(t, "", store.vps[vid].FailureCode)
}
