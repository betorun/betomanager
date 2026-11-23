package workflows

import (
	"context"
	"encoding/json"
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

type fakeNetwork struct {
	reverses  map[string]string
	unblocked []string
	stopped   []string
}

func (f *fakeNetwork) SetReverse(_ context.Context, ip, r string) error {
	f.reverses[ip] = r
	return nil
}
func (f *fakeNetwork) SpamBlocked(context.Context, string) (bool, time.Time, error) {
	return false, time.Time{}, nil
}
func (f *fakeNetwork) UnblockSpam(_ context.Context, ip string) error {
	f.unblocked = append(f.unblocked, ip)
	return nil
}
func (f *fakeNetwork) StopRenewal(_ context.Context, sn string) error {
	f.stopped = append(f.stopped, sn)
	return nil
}

func runNet(t *testing.T, store *vmStore, cloud *fakeCloud, net *fakeNetwork, rid uuid.UUID, kind string) error {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store, Provider: cloud, Network: net})
	env.ExecuteWorkflow(RequestWorkflow, service.RequestInput{RequestID: rid.String(), Kind: kind})
	require.True(t, env.IsWorkflowCompleted())
	return env.GetWorkflowError()
}

func TestReverseAndTerminate(t *testing.T) {
	store, vid, rid := setup(domain.StateRunning, domain.KindReverse)
	store.vps[vid].ProviderID, store.vps[vid].IPv4 = "vps-a.vps.ovh.net", "51.68.1.2"
	raw, _ := json.Marshal(domain.ReversePayload{Family: "v4", Reverse: "mail.exemple.cg"})
	store.reqs[rid].Payload = raw
	net := &fakeNetwork{reverses: map[string]string{}}
	require.NoError(t, runNet(t, store, &fakeCloud{state: domain.StateRunning}, net, rid, domain.KindReverse))
	assert.Equal(t, "mail.exemple.cg", net.reverses["51.68.1.2"])

	tstore, tvid, trid := setup(domain.StateDeleted, domain.KindTerminate)
	tstore.vps[tvid].ProviderID = "vps-b.vps.ovh.net"
	cloud := &fakeCloud{state: domain.StateRunning}
	require.NoError(t, runNet(t, tstore, cloud, net, trid, domain.KindTerminate))
	assert.Equal(t, []string{"stop"}, cloud.powered, "coupé tout de suite")
	assert.Equal(t, []string{"vps-b.vps.ovh.net"}, net.stopped, "plus renouvelé chez le fournisseur")
}

func TestCreateInstallsSMTPBlockUnlessAllowed(t *testing.T) {
	store, _, rid := setup(domain.StatePending, KindCreate)
	cloud := &fakeCloud{orderState: service.OrderDelivered, state: domain.StateRunning}
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{refunds: map[string]billingDomain.Milli{}}, rid, KindCreate))
	assert.Equal(t, domain.SMTPBlockScript, cloud.script, "port 25 fermé par défaut")

	store, vid, rid := setup(domain.StatePending, KindCreate)
	store.vps[vid].SMTPAllowed = true
	cloud = &fakeCloud{orderState: service.OrderDelivered, state: domain.StateRunning}
	require.NoError(t, runVM(t, store, cloud, &fakeWallet{refunds: map[string]billingDomain.Milli{}}, rid, KindCreate))
	assert.Empty(t, cloud.script, "ouvert par l'équipe : aucune règle")
}
