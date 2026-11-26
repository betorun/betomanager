// Package vpctest : suites de contrat que toute mise en œuvre du VPC passe
// (tunnel, réseau natif Proxmox…). Le produit ne dépend que de ce contrat.
package vpctest

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

// samplePublicKey : forme d'une clé publique d'inscription.
const samplePublicKey = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="

func vpcAt(cidr string) domain.VPC {
	p := netip.MustParsePrefix(cidr)
	return domain.VPC{ProjectID: uuid.New(), ClusterID: "contract", CIDR: p, Gateway: domain.GatewayOf(p)}
}

func member(vpc domain.VPC, host byte, name string) domain.Member {
	b := vpc.CIDR.Addr().As4()
	b[3] = host
	return domain.Member{VPSID: uuid.New(), Name: name, Address: netip.AddrFrom4(b), PublicIPv4: fmt.Sprintf("203.0.113.%d", host)}
}

func statusOf(t *testing.T, b domain.VPCBackend, vpc domain.VPC) map[uuid.UUID]domain.MemberStatus {
	t.Helper()
	list, err := b.Status(context.Background(), vpc)
	require.NoError(t, err)
	out := make(map[uuid.UUID]domain.MemberStatus, len(list))
	for _, s := range list {
		out[s.VPSID] = s
	}
	return out
}

// RunBackendContract : ce que toute VPCBackend garantit. newBackend rend une
// mise en œuvre neuve, prête pour un placement qu'elle sait servir.
func RunBackendContract(t *testing.T, newBackend func(t *testing.T) domain.VPCBackend) {
	ctx := context.Background()

	t.Run("nom stable, jamais vide", func(t *testing.T) {
		b := newBackend(t)
		require.NotEmpty(t, b.Name())
		assert.Equal(t, b.Name(), b.Name())
	})

	t.Run("attacher et préparer sont idempotents", func(t *testing.T) {
		b := newBackend(t)
		vpc := vpcAt("10.248.1.0/24")
		m := member(vpc, 2, "web")
		require.NoError(t, b.Ensure(ctx, vpc, nil))
		require.NoError(t, b.Ensure(ctx, vpc, nil))
		p1, err := b.Attach(ctx, vpc, m)
		require.NoError(t, err)
		p2, err := b.Attach(ctx, vpc, m)
		require.NoError(t, err)
		assert.Equal(t, p1, p2)
		assert.Equal(t, b.Capabilities().NeedsEnrollment, p1.NeedsEnrollment, "le plan suit les capacités")
		require.NoError(t, b.Ensure(ctx, vpc, []domain.Member{m}))
		assert.Contains(t, statusOf(t, b, vpc), m.VPSID)
	})

	t.Run("inscription", func(t *testing.T) {
		b := newBackend(t)
		vpc := vpcAt("10.248.2.0/24")
		m := member(vpc, 2, "api")
		require.NoError(t, b.Ensure(ctx, vpc, nil))
		_, err := b.Attach(ctx, vpc, m)
		require.NoError(t, err)
		if !b.Capabilities().NeedsEnrollment {
			_, err := b.Enroll(ctx, domain.EnrollRequest{VPC: vpc, Member: m, PublicKey: samplePublicKey})
			assert.ErrorIs(t, err, domain.ErrNotEnrollable)
			return
		}
		_, err = b.Enroll(ctx, domain.EnrollRequest{VPC: vpc, Member: m, PublicKey: "pas une clé"})
		assert.ErrorIs(t, err, domain.ErrInvalidPublicKey)
		res, err := b.Enroll(ctx, domain.EnrollRequest{VPC: vpc, Member: m, PublicKey: samplePublicKey})
		require.NoError(t, err)
		require.NotEmpty(t, res.Config)
		for k := range res.Config {
			assert.NotContains(t, strings.ToLower(k), "private", "aucun secret dans la configuration rendue au VPS")
		}
	})

	t.Run("détacher : idempotent, même inconnu, et effectif", func(t *testing.T) {
		b := newBackend(t)
		vpc := vpcAt("10.248.3.0/24")
		m := member(vpc, 2, "db-client")
		require.NoError(t, b.Ensure(ctx, vpc, nil))
		_, err := b.Attach(ctx, vpc, m)
		require.NoError(t, err)
		require.NoError(t, b.Detach(ctx, vpc, m.VPSID))
		require.NoError(t, b.Detach(ctx, vpc, m.VPSID))
		require.NoError(t, b.Detach(ctx, vpc, uuid.New()))
		assert.NotContains(t, statusOf(t, b, vpc), m.VPSID)
	})

	t.Run("deux VPC ne se voient pas", func(t *testing.T) {
		b := newBackend(t)
		a, c := vpcAt("10.248.4.0/24"), vpcAt("10.248.5.0/24")
		ma, mc := member(a, 2, "a"), member(c, 2, "c")
		for _, x := range []struct {
			v domain.VPC
			m domain.Member
		}{{a, ma}, {c, mc}} {
			require.NoError(t, b.Ensure(ctx, x.v, nil))
			_, err := b.Attach(ctx, x.v, x.m)
			require.NoError(t, err)
		}
		sa, sc := statusOf(t, b, a), statusOf(t, b, c)
		assert.Contains(t, sa, ma.VPSID)
		assert.NotContains(t, sa, mc.VPSID)
		assert.NotContains(t, sc, ma.VPSID)
		// Même plage dans un autre cluster : toujours isolés.
		d := a
		d.ProjectID, d.ClusterID = uuid.New(), "autre"
		require.NoError(t, b.Ensure(ctx, d, nil))
		assert.NotContains(t, statusOf(t, b, d), ma.VPSID)
	})

	t.Run("démonter : idempotent, plus aucun membre", func(t *testing.T) {
		b := newBackend(t)
		vpc := vpcAt("10.248.6.0/24")
		m := member(vpc, 2, "x")
		require.NoError(t, b.Ensure(ctx, vpc, nil))
		_, err := b.Attach(ctx, vpc, m)
		require.NoError(t, err)
		require.NoError(t, b.Teardown(ctx, vpc))
		require.NoError(t, b.Teardown(ctx, vpc))
		assert.Empty(t, statusOf(t, b, vpc))
	})
}

// RunFirewallContract : ce que tout FirewallProvider garantit.
func RunFirewallContract(t *testing.T, newProvider func(t *testing.T) domain.FirewallProvider) {
	ctx := context.Background()
	m := domain.Member{VPSID: uuid.New(), Name: "web", PublicIPv4: "203.0.113.10"}
	ssh := domain.FirewallRule{Action: domain.ActionAllow, Protocol: domain.ProtocolTCP, PortFrom: 22}
	web := domain.FirewallRule{Action: domain.ActionAllow, Protocol: domain.ProtocolTCP, PortFrom: 80, PortTo: 443}
	deny := domain.FirewallRule{Action: domain.ActionDeny, Protocol: domain.ProtocolAll}

	client := func(rs []domain.FirewallRule) []domain.FirewallRule {
		var out []domain.FirewallRule
		for _, r := range rs {
			if !r.Reserved {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("capacités cohérentes", func(t *testing.T) {
		p := newProvider(t)
		require.NotEmpty(t, p.Name())
		assert.Positive(t, p.Capabilities().MaxRules)
	})

	t.Run("appliquer remplace, dans l'ordre, réservées à part", func(t *testing.T) {
		p := newProvider(t)
		require.NoError(t, p.Apply(ctx, m, []domain.FirewallRule{ssh, web, deny}))
		got, err := p.Get(ctx, m)
		require.NoError(t, err)
		assert.Equal(t, []domain.FirewallRule{ssh, web, deny}, client(got))
		require.NoError(t, p.Apply(ctx, m, []domain.FirewallRule{deny}))
		got, err = p.Get(ctx, m)
		require.NoError(t, err)
		assert.Equal(t, []domain.FirewallRule{deny}, client(got))
	})

	t.Run("limites et règles invalides refusées, rien n'est changé", func(t *testing.T) {
		p := newProvider(t)
		require.NoError(t, p.Apply(ctx, m, []domain.FirewallRule{ssh}))
		tooMany := make([]domain.FirewallRule, p.Capabilities().MaxRules+1)
		for i := range tooMany {
			tooMany[i] = ssh
		}
		assert.ErrorIs(t, p.Apply(ctx, m, tooMany), domain.ErrTooManyRules)
		assert.ErrorIs(t, p.Apply(ctx, m, []domain.FirewallRule{{Action: "x", Protocol: domain.ProtocolTCP}}), domain.ErrInvalidRule)
		reserved := ssh
		reserved.Reserved = true
		assert.ErrorIs(t, p.Apply(ctx, m, []domain.FirewallRule{reserved}), domain.ErrInvalidRule, "le client ne pose pas de règle réservée")
		got, err := p.Get(ctx, m)
		require.NoError(t, err)
		assert.Equal(t, []domain.FirewallRule{ssh}, client(got))
	})
}
