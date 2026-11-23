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
	enqueued []string
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
		case "image":
			v.Image = val.(string)
		case "ssh_key_fingerprint":
			v.SSHKeyFingerprint = val.(string)
		case "snapshot_option":
			v.SnapshotOption = val.(string)
		case "rescue":
			v.Rescue = val.(string)
		case "rescue_secret":
			v.RescueSecret = val.(string)
		}
	}
	v.Version++
	return nil
}
func (s *vmStore) Offer(context.Context, string) (*domain.Offer, error) {
	return &domain.Offer{Code: "VPS-1", ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649, SnapshotPlanCode: "option-snapshot", SnapshotPriceCents: 120,
		Images: json.RawMessage(`{"ubuntu-24.04":"Ubuntu 24.04"}`)}, nil
}
func (s *vmStore) EnqueueTx(_ *gorm.DB, r *domain.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued = append(s.enqueued, r.Kind)
	return nil
}
func (s *vmStore) Transaction(_ context.Context, fn func(*gorm.DB) error) error { return fn(nil) }

type fakeCloud struct {
	service.ComputeProvider
	mu         sync.Mutex
	orderErr   error
	orderState string
	orders     int
	rebuilt    string
	script     string
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
func (f *fakeCloud) Rebuild(_ context.Context, _, image, key, script string) (string, error) {
	f.rebuilt = image + "|" + key
	f.script = script
	return "55", nil
}
func (f *fakeCloud) Task(context.Context, string, string) (*service.ProviderTask, error) {
	return &service.ProviderTask{State: service.TaskDone}, nil
}
func (f *fakeCloud) Reboot(context.Context, string) (string, error) {
	f.powered = append(f.powered, "reboot")
	return "58", nil
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

func TestRebootVM(t *testing.T) {
	store, vid, rid := setup(domain.StateRunning, KindReboot)
	store.vps[vid].ProviderID = "vps-a.vps.ovh.net"
	cloud := &fakeCloud{state: domain.StateRunning}
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{}, rid, KindReboot))
	assert.Equal(t, []string{"reboot"}, cloud.powered)
	assert.Equal(t, domain.StateRunning, store.vps[vid].State)
}

func TestReinstallVM(t *testing.T) {
	store, vid, rid := setup(domain.StateStopped, KindReinstall)
	store.vps[vid].ProviderID = "vps-a.vps.ovh.net"
	payload, _ := json.Marshal(domain.ReinstallPayload{Image: "ubuntu-24.04", SSHPublicKey: "ssh-ed25519 BBBB", Fingerprint: "SHA256:new"})
	store.reqs[rid].Payload = payload
	cloud := &fakeCloud{state: domain.StateRunning}
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{}, rid, KindReinstall))
	assert.Equal(t, "Ubuntu 24.04|ssh-ed25519 BBBB", cloud.rebuilt)
	v := store.vps[vid]
	assert.Equal(t, "SHA256:new", v.SSHKeyFingerprint)
	assert.Equal(t, domain.StateRunning, v.State, "un VPS réinstallé redémarre")
}

func TestSuspendedDuringOperationGetsStopped(t *testing.T) {
	store, vid, rid := setup(domain.StateRunning, KindReboot)
	v := store.vps[vid]
	v.ProviderID = "vps-a.vps.ovh.net"
	cloud := &fakeCloud{state: domain.StateRunning}
	// L'organisation est suspendue pendant le redémarrage.
	v.State, v.FailureCode = domain.StateSuspended, domain.FailureOrgSuspended
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{}, rid, KindReboot))
	assert.Equal(t, domain.StateSuspended, store.vps[vid].State)
	assert.Equal(t, []string{KindStop}, store.enqueued, "arrêt déposé à la fin de l'opération")
}

type listCloud struct {
	*fakeCloud
	ids          []string
	plans, zones map[string]string
}

func (l *listCloud) List(context.Context) ([]service.ProviderVM, error) {
	out := []service.ProviderVM{}
	for _, id := range l.ids {
		vm := service.ProviderVM{ProviderID: id, Plan: "vps-2025-model1", Zone: "Region OpenStack: os-gra7"}
		if l.plans[id] != "" {
			vm.Plan = l.plans[id]
		}
		if l.zones[id] != "" {
			vm.Zone = l.zones[id]
		}
		out = append(out, vm)
	}
	return out, nil
}
func (l *listCloud) Name() string { return domain.ProviderOVH }

type resolveStore struct {
	*vmStore
	known    []domain.VPS
	machines []domain.ProviderMachine
}

func (s *resolveStore) ProviderVPS(context.Context, string) ([]domain.VPS, error) {
	return s.known, nil
}
func (s *resolveStore) Machines(context.Context) ([]domain.ProviderMachine, error) {
	return s.machines, nil
}

func TestResolveDelivered(t *testing.T) {
	vs, vid, _ := setup(domain.StateCreating, KindCreate)
	created := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	vs.vps[vid].CreatedAt = created
	old := created.Add(-48 * time.Hour)
	store := &resolveStore{vmStore: vs,
		known:    []domain.VPS{{ProviderID: "vps-client.vps.ovh.net"}},
		machines: []domain.ProviderMachine{{ProviderID: "vps-old-orphan.vps.ovh.net", Drift: domain.DriftOrphan, FirstSeenAt: &old}}}
	a := &Activities{Store: store, Provider: &listCloud{fakeCloud: &fakeCloud{}, ids: []string{"vps-client.vps.ovh.net", "vps-old-orphan.vps.ovh.net", "vps-new.vps.ovh.net"}}}
	pid, err := a.ResolveDelivered(context.Background(), vid.String())
	require.NoError(t, err)
	assert.Equal(t, "vps-new.vps.ovh.net", pid, "seul VPS inconnu apparu après la commande")

	a.Provider = &listCloud{fakeCloud: &fakeCloud{}, ids: []string{"vps-new.vps.ovh.net", "vps-other.vps.ovh.net"}}
	pid, err = a.ResolveDelivered(context.Background(), vid.String())
	require.NoError(t, err)
	assert.Equal(t, "", pid, "deux candidats : l'équipe tranche")

	// Un seul inconnu, mais d'un autre plan ou d'un autre datacenter : jamais rattaché.
	a.Provider = &listCloud{fakeCloud: &fakeCloud{}, ids: []string{"vps-other.vps.ovh.net"}, plans: map[string]string{"vps-other.vps.ovh.net": "vps-2025-model3"}}
	pid, _ = a.ResolveDelivered(context.Background(), vid.String())
	assert.Equal(t, "", pid, "autre plan : commande d'un autre client")
	a.Provider = &listCloud{fakeCloud: &fakeCloud{}, ids: []string{"vps-other.vps.ovh.net"}, zones: map[string]string{"vps-other.vps.ovh.net": "Region OpenStack: os-sbg5"}}
	pid, _ = a.ResolveDelivered(context.Background(), vid.String())
	assert.Equal(t, "", pid, "autre datacenter")

	vs.vps[vid].ProviderID = "vps-attached.vps.ovh.net"
	pid, _ = a.ResolveDelivered(context.Background(), vid.String())
	assert.Equal(t, "vps-attached.vps.ovh.net", pid, "rattaché par l'équipe")
}
