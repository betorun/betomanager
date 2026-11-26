// Package memory : mise en œuvre du VPC en mémoire, pour les tests et le
// développement (lot 1 de l'ADR 0019). Elle ne relie rien : elle tient
// l'état qu'une vraie mise en œuvre tiendrait, et passe les suites de
// contrat de vpctest.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

// Name : identifiant de la mise en œuvre.
const Name = "memory"

type netKey struct {
	cluster string
	project uuid.UUID
}

type entry struct {
	member   domain.Member
	enrolled *time.Time
}

// Backend : VPCBackend en mémoire. Enrollment : le VPS doit s'inscrire
// (comme un tunnel) ; faux : il naît relié (comme un réseau natif).
type Backend struct {
	Enrollment bool
	Now        func() time.Time

	mu   sync.Mutex
	nets map[netKey]map[uuid.UUID]*entry
}

var _ domain.VPCBackend = (*Backend)(nil)

func (b *Backend) Name() string                  { return Name }
func (b *Backend) Handles(domain.Placement) bool { return true }
func (b *Backend) Capabilities() domain.Capabilities {
	return domain.Capabilities{NeedsEnrollment: b.Enrollment}
}

func (b *Backend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

func (b *Backend) net(v domain.VPC) map[uuid.UUID]*entry {
	if b.nets == nil {
		b.nets = map[netKey]map[uuid.UUID]*entry{}
	}
	k := netKey{v.ClusterID, v.ProjectID}
	if b.nets[k] == nil {
		b.nets[k] = map[uuid.UUID]*entry{}
	}
	return b.nets[k]
}

func (b *Backend) Ensure(_ context.Context, v domain.VPC, members []domain.Member) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.net(v)
	for _, m := range members {
		if e, ok := n[m.VPSID]; ok {
			e.member = m
			continue
		}
		n[m.VPSID] = &entry{member: m}
	}
	return nil
}

func (b *Backend) Attach(_ context.Context, v domain.VPC, m domain.Member) (*domain.AttachPlan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.net(v)
	if _, ok := n[m.VPSID]; !ok {
		n[m.VPSID] = &entry{member: m}
	}
	return &domain.AttachPlan{NeedsEnrollment: b.Enrollment}, nil
}

func (b *Backend) Enroll(_ context.Context, req domain.EnrollRequest) (*domain.EnrollResult, error) {
	if !b.Enrollment {
		return nil, domain.ErrNotEnrollable
	}
	if !domain.ValidPublicKey(req.PublicKey) {
		return nil, domain.ErrInvalidPublicKey
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.net(req.VPC)
	e, ok := n[req.Member.VPSID]
	if !ok {
		return nil, domain.ErrNotAttached
	}
	at := b.now()
	e.member.PublicKey, e.enrolled = req.PublicKey, &at
	return &domain.EnrollResult{Config: map[string]string{
		"address": req.Member.Address.String(),
		"gateway": req.VPC.Gateway.String(),
		"range":   req.VPC.CIDR.String(),
	}}, nil
}

func (b *Backend) Detach(_ context.Context, v domain.VPC, id uuid.UUID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.net(v), id)
	return nil
}

func (b *Backend) Status(_ context.Context, v domain.VPC) ([]domain.MemberStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.net(v)
	out := make([]domain.MemberStatus, 0, len(n))
	for id, e := range n {
		connected := !b.Enrollment || e.enrolled != nil
		out = append(out, domain.MemberStatus{VPSID: id, Connected: connected, LastContact: e.enrolled})
	}
	return out, nil
}

func (b *Backend) Teardown(_ context.Context, v domain.VPC) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.nets, netKey{v.ClusterID, v.ProjectID})
	return nil
}

// Firewall : FirewallProvider en mémoire. Une règle réservée (entrée du
// réseau privé) précède celles du client, comme sur un vrai pare-feu.
type Firewall struct {
	MaxRules int

	mu    sync.Mutex
	rules map[uuid.UUID][]domain.FirewallRule
}

var _ domain.FirewallProvider = (*Firewall)(nil)

func (f *Firewall) Name() string { return Name }

func (f *Firewall) Capabilities() domain.FirewallCapabilities {
	max := f.MaxRules
	if max <= 0 {
		max = 16
	}
	return domain.FirewallCapabilities{MaxRules: max, Stateful: true}
}

// reserved : règle posée par la plateforme.
var reserved = domain.FirewallRule{Action: domain.ActionAllow, Protocol: domain.ProtocolUDP, PortFrom: 51820, Reserved: true}

func (f *Firewall) Apply(_ context.Context, m domain.Member, rules []domain.FirewallRule) error {
	c := f.Capabilities()
	if err := domain.ValidateRules(rules, c.MaxRules, c.IPv6); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rules == nil {
		f.rules = map[uuid.UUID][]domain.FirewallRule{}
	}
	f.rules[m.VPSID] = append([]domain.FirewallRule(nil), rules...)
	return nil
}

func (f *Firewall) Get(_ context.Context, m domain.Member) ([]domain.FirewallRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.FirewallRule{reserved}, f.rules[m.VPSID]...), nil
}
