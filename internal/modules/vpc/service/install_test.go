package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/backend/memory"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

const enrollKey = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="

// scripted : mise en œuvre avec inscription par script (comme tunnel).
type scripted struct{ *memory.Backend }

func (scripted) EnrollScript(apiURL, token string) string {
	return "#!/bin/sh\nAPI=" + apiURL + "\nTOKEN=${1:-" + token + "}\n"
}

func newScriptedFixture(t *testing.T) *fixture {
	f := newFixture(t, true)
	f.svc.Backends = []domain.VPCBackend{scripted{f.backend}}
	f.svc.APIURL = "https://api.example.com/kmanager/v1/"
	return f
}

func members(t *testing.T, f *fixture) map[uuid.UUID]domain.MemberStatus {
	t.Helper()
	n, err := f.svc.Store.Network(context.Background(), f.project)
	require.NoError(t, err)
	if n == nil {
		return map[uuid.UUID]domain.MemberStatus{}
	}
	vpc, err := toVPC(n)
	require.NoError(t, err)
	list, err := f.backend.Status(context.Background(), vpc)
	require.NoError(t, err)
	out := map[uuid.UUID]domain.MemberStatus{}
	for _, s := range list {
		out[s.VPSID] = s
	}
	return out
}

func TestAttachGivesCommand_AndRemembersChoice(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	require.NotNil(t, r.Enrollment)
	assert.Equal(t, "curl -fsS https://api.example.com/kmanager/v1/vpc/enroll.sh | sudo sh -s -- "+r.Enrollment.Token, r.Enrollment.Command)
	assert.True(t, v.PrivateNetwork, "choix retenu pour les réinstallations")
	assert.Contains(t, f.svc.EnrollScript(), "TOKEN=${1:-}", "script servi sans jeton")

	require.NoError(t, f.svc.Detach(ctx, f.org, v.ID))
	assert.False(t, v.PrivateNetwork)
}

func TestPrepareInstall_Creation(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()

	plain := f.vps("plain", computeDomain.StateCreating)
	script, err := f.svc.PrepareInstall(ctx, plain.ID)
	require.NoError(t, err)
	assert.Empty(t, script, "réseau privé non demandé")

	v := f.vps("web", computeDomain.StateCreating)
	v.PrivateNetwork = true
	script, err = f.svc.PrepareInstall(ctx, v.ID)
	require.NoError(t, err)
	assert.Contains(t, script, "API=https://api.example.com/kmanager/v1\n")
	i := strings.Index(script, "bvpc_")
	require.Positive(t, i, "jeton glissé dans le script")
	token := strings.TrimSuffix(strings.Split(script[i:], "}")[0], "\n")

	a, err := f.svc.Store.Attachment(ctx, v.ID)
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, domain.StatePending, a.State)

	// Le jeton du premier démarrage vaut deux heures.
	f.now = f.now.Add(90 * 60 * 1e9)
	out, err := f.svc.Enroll(ctx, EnrollInput{Token: token, PublicKey: enrollKey})
	require.NoError(t, err)
	assert.Equal(t, a.Address, out.Address)
}

func TestPrepareInstall_ReinstallKeepsAddress(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r.Enrollment.Token, PublicKey: enrollKey})
	require.NoError(t, err)
	assert.True(t, members(t, f)[v.ID].Connected)

	script, err := f.svc.PrepareInstall(ctx, v.ID)
	require.NoError(t, err)
	assert.Contains(t, script, "bvpc_")
	a, _ := f.svc.Store.Attachment(ctx, v.ID)
	assert.Equal(t, r.Member.Address, a.Address, "même adresse après réinstallation")
	assert.Equal(t, domain.StatePending, a.State)
	assert.Empty(t, a.PublicKey, "ancienne clé retirée (disque effacé)")
}

func TestPrepareInstall_UnavailableNeverBlocks(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateCreating)
	v.PrivateNetwork, v.Location = true, "bhs" // autre région que le cluster
	script, err := f.svc.PrepareInstall(ctx, v.ID)
	require.NoError(t, err)
	assert.Empty(t, script)
	assert.False(t, v.PrivateNetwork, "choix retiré : le client le refera")
}

func TestSync_SuspendResumeTerminate(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v, other := f.vps("web", computeDomain.StateRunning), f.vps("db", computeDomain.StateRunning)
	for _, x := range []*computeDomain.VPS{v, other} {
		r, err := f.svc.Attach(ctx, f.org, x.ID, "u1")
		require.NoError(t, err)
		_, err = f.svc.Enroll(ctx, EnrollInput{Token: r.Enrollment.Token, PublicKey: enrollKey})
		require.NoError(t, err)
	}

	v.State = computeDomain.StateSuspended
	f.svc.OnVPSChanged(ctx, v)
	assert.NotContains(t, members(t, f), v.ID, "suspendu : accès coupé")
	a, _ := f.svc.Store.Attachment(ctx, v.ID)
	require.NotNil(t, a, "rattachement gardé pendant la suspension")

	v.State = computeDomain.StateRunning
	f.svc.OnVPSChanged(ctx, v)
	assert.Contains(t, members(t, f), v.ID, "reprise : accès rendu")

	v.State = computeDomain.StateDeleted
	f.svc.OnVPSChanged(ctx, v)
	a, _ = f.svc.Store.Attachment(ctx, v.ID)
	assert.Nil(t, a, "résilié : rattachement retiré")
	assert.NotContains(t, members(t, f), v.ID)
	assert.False(t, v.PrivateNetwork)
	assert.Contains(t, members(t, f), other.ID, "les autres VPS restent")

	other.State = computeDomain.StateFailed
	require.NoError(t, f.svc.Reconcile(ctx))
	assert.Empty(t, members(t, f), "dernier VPS parti : réseau démonté")
}

func TestSync_RemovesUnknownMembers(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	_, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	n, _ := f.svc.Store.Network(ctx, f.project)
	vpc, _ := toVPC(n)
	stray := uuid.New()
	require.NoError(t, f.backend.Ensure(ctx, vpc, []domain.Member{{VPSID: stray, Name: "x"}}))
	require.NoError(t, f.svc.Sync(ctx, f.project))
	assert.NotContains(t, members(t, f), stray)
	assert.Contains(t, members(t, f), v.ID)
}

func TestEnroll_SourceAddress(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	v.IPv6 = "2001:db8::5"
	attach := func() string {
		r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
		require.NoError(t, err)
		return r.Enrollment.Token
	}
	_, err := f.svc.Enroll(ctx, EnrollInput{Token: attach(), PublicKey: enrollKey, SourceIP: "198.51.100.77"})
	assert.ErrorIs(t, err, domain.ErrInvalidEnrollment, "jeton présenté par une autre machine")
	for _, src := range []string{"203.0.113.5", "2001:db8::5", "10.42.0.9", ""} {
		_, err = f.svc.Enroll(ctx, EnrollInput{Token: attach(), PublicKey: enrollKey, SourceIP: src})
		assert.NoError(t, err, "source %q", src)
		require.NoError(t, f.svc.Store.Reset(ctx, v.ID))
	}
}

func TestCheckOrder(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	assert.NoError(t, f.svc.CheckOrder(ctx, f.project, "ovh", "gra"))
	assert.ErrorIs(t, f.svc.CheckOrder(ctx, f.project, "ovh", "bhs"), domain.ErrRegionMismatch)
	f.svc.APIURL = ""
	assert.ErrorIs(t, f.svc.CheckOrder(ctx, f.project, "ovh", "gra"), domain.ErrUnavailable, "inscription impossible sans URL publique")
	f.svc.Backends = nil
	assert.ErrorIs(t, f.svc.CheckOrder(ctx, f.project, "ovh", "gra"), domain.ErrUnavailable)
}

func TestBeforeReinstall_CutsAccessNow(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r.Enrollment.Token, PublicKey: enrollKey})
	require.NoError(t, err)
	require.NoError(t, f.svc.BeforeReinstall(ctx, v.ID))
	a, _ := f.svc.Store.Attachment(ctx, v.ID)
	assert.Equal(t, domain.StatePending, a.State)
	assert.Empty(t, a.PublicKey)
	assert.Equal(t, r.Member.Address, a.Address)
	require.NoError(t, f.svc.BeforeReinstall(ctx, uuid.New()), "VPS non rattaché : rien à faire")
}

func TestPrepareInstall_NativeAttachedAtSync(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateCreating)
	v.PrivateNetwork = true
	script, err := f.svc.PrepareInstall(ctx, v.ID)
	require.NoError(t, err)
	assert.Empty(t, script, "réseau natif : rien à lancer sur le VPS")
	require.NoError(t, f.svc.Sync(ctx, f.project))
	a, _ := f.svc.Store.Attachment(ctx, v.ID)
	assert.Equal(t, domain.StateAttached, a.State)
}

func TestSync_EmptyNetworkIsTornDown(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	_, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	require.NoError(t, f.svc.Detach(ctx, f.org, v.ID))
	n, _ := f.svc.Store.Network(ctx, f.project)
	vpc, _ := toVPC(n)
	// Recréé entre-temps par une synchronisation concurrente.
	require.NoError(t, f.backend.Ensure(ctx, vpc, []domain.Member{{VPSID: uuid.New(), Name: "x"}}))
	require.NoError(t, f.svc.Reconcile(ctx))
	assert.Empty(t, members(t, f))
}

func TestEnroll_SuspendedRefused(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	v := f.vps("web", computeDomain.StateRunning)
	r, err := f.svc.Attach(ctx, f.org, v.ID, "u1")
	require.NoError(t, err)
	v.State = computeDomain.StateSuspended
	_, err = f.svc.Enroll(ctx, EnrollInput{Token: r.Enrollment.Token, PublicKey: enrollKey})
	assert.ErrorIs(t, err, domain.ErrVPSNotReady)
	assert.False(t, members(t, f)[v.ID].Connected, "aucun accès pendant la suspension")
}
