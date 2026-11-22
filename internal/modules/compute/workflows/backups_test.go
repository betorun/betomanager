package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type backupStore struct {
	*vmStore
	saved map[uuid.UUID]*domain.Backups
}

func (s *backupStore) SaveBackups(_ context.Context, b *domain.Backups) error {
	s.saved[b.VPSID] = b
	return nil
}
func (s *backupStore) Backups(_ context.Context, id uuid.UUID) (*domain.Backups, error) {
	return s.saved[id], nil
}

type fakeBackups struct {
	service.BackupProvider
	calls     []string
	optionOn  bool
	orderErr  error
	restoreTo []bool
}

func (f *fakeBackups) Backups(context.Context, string) (*service.ProviderBackups, error) {
	return &service.ProviderBackups{Automated: true, SnapshotOption: f.optionOn,
		RestorePoints: []time.Time{time.Date(2026, 11, 22, 2, 0, 0, 0, time.UTC)}}, nil
}
func (f *fakeBackups) Restore(_ context.Context, _ string, _ time.Time, full bool) (string, error) {
	f.restoreTo = append(f.restoreTo, full)
	return "80", nil
}
func (f *fakeBackups) OrderSnapshotOption(context.Context, string, string, int64) (string, error) {
	f.calls = append(f.calls, "order")
	if f.orderErr != nil {
		return "", f.orderErr
	}
	f.optionOn = true
	return "777", nil
}
func (f *fakeBackups) ReleaseSnapshotOption(context.Context, string, string) error {
	f.calls = append(f.calls, "release")
	f.optionOn = false
	return nil
}

func runBackup(t *testing.T, store *backupStore, cloud *fakeCloud, b *fakeBackups, wallet *fakeWallet, rid uuid.UUID, kind string) error {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store, Provider: cloud, Orderer: cloud, Backups: b, Wallet: wallet})
	env.ExecuteWorkflow(RequestWorkflow, service.RequestInput{RequestID: rid.String(), Kind: kind})
	require.True(t, env.IsWorkflowCompleted())
	return env.GetWorkflowError()
}

func backupSetup(kind string, payload any) (*backupStore, uuid.UUID, uuid.UUID) {
	vs, vid, rid := setup(domain.StateRunning, kind)
	vs.vps[vid].ProviderID = "vps-a.vps.ovh.net"
	raw, _ := json.Marshal(payload)
	vs.reqs[rid].Payload = raw
	return &backupStore{vmStore: vs, saved: map[uuid.UUID]*domain.Backups{}}, vid, rid
}

func TestRestoreMount(t *testing.T) {
	store, vid, rid := backupSetup(domain.KindRestore, domain.RestorePayload{RestorePoint: time.Date(2026, 11, 22, 2, 0, 0, 0, time.UTC), Mode: domain.RestoreMount})
	b := &fakeBackups{}
	require.NoError(t, runBackup(t, store, &fakeCloud{state: domain.StateRunning}, b, &fakeWallet{}, rid, domain.KindRestore))
	assert.Equal(t, []bool{false}, b.restoreTo, "montage, pas de remplacement du disque")
	assert.NotNil(t, store.saved[vid], "relevé refait")
	assert.Equal(t, domain.StateRunning, store.vps[vid].State)
}

func TestSubscribeSnapshot(t *testing.T) {
	store, vid, rid := backupSetup(domain.KindSnapshotOrder, domain.SubscribePayload{Period: "p:snapshot", AmountMilli: 1_000_000})
	store.vps[vid].SnapshotOption = domain.SnapshotOrdering
	b := &fakeBackups{}
	wallet := &fakeWallet{refunds: map[string]billingDomain.Milli{}}
	require.NoError(t, runBackup(t, store, &fakeCloud{orderState: service.OrderDelivered, state: domain.StateRunning}, b, wallet, rid, domain.KindSnapshotOrder))
	assert.Equal(t, []string{"order"}, b.calls)
	assert.Empty(t, wallet.refunds)
}

func TestSubscribeSnapshot_FailsRefunded(t *testing.T) {
	store, _, rid := backupSetup(domain.KindSnapshotOrder, domain.SubscribePayload{Period: "p:snapshot", AmountMilli: 1_000_000})
	b := &fakeBackups{orderErr: errors.New("refused")}
	wallet := &fakeWallet{refunds: map[string]billingDomain.Milli{}}
	require.Error(t, runBackup(t, store, &fakeCloud{state: domain.StateRunning}, b, wallet, rid, domain.KindSnapshotOrder))
	assert.Equal(t, billingDomain.Milli(1_000_000), wallet.refunds["p:snapshot:refund"])
	assert.Equal(t, []string{"order"}, b.calls, "jamais rejoué")
}
