// Package service : VPC d'un projet (ADR 0019, lot 1). Rattacher un VPS au
// réseau privé de son projet, le détacher, l'inscrire, et montrer le
// réseau. La mise en œuvre (VPCBackend) est choisie par le placement du VPS
// et du cluster du projet ; elle n'est jamais nommée au client.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	netDomain "github.com/thekrauss/kubemanager/internal/modules/network/domain"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

// Store : VPC, rattachements et jetons (repository.Store).
type Store interface {
	Network(ctx context.Context, project uuid.UUID) (*domain.Network, error)
	Attachment(ctx context.Context, vps uuid.UUID) (*domain.Attachment, error)
	Attachments(ctx context.Context, project uuid.UUID) ([]domain.Attachment, error)
	Attach(ctx context.Context, project uuid.UUID, cluster string, a *domain.Attachment) (*domain.Network, error)
	SetState(ctx context.Context, vps uuid.UUID, state, publicKey string, at time.Time) error
	Detach(ctx context.Context, vps, project uuid.UUID) (int64, error)
	NewEnrollment(ctx context.Context, e *domain.Enrollment) error
	ConsumeEnrollment(ctx context.Context, hash []byte, now time.Time) (*domain.Enrollment, error)
	Reset(ctx context.Context, vps uuid.UUID) error
	SetWanted(ctx context.Context, vps uuid.UUID, wanted bool) error
	Networks(ctx context.Context) ([]domain.Network, error)
	// IfEmpty : fn sous le verrou des rattachements du projet, s'il n'en a
	// aucun.
	IfEmpty(ctx context.Context, project uuid.UUID, fn func() error) error
	// RecordTraffic : compteurs relevés par VPS (cumul mensuel du projet).
	RecordTraffic(ctx context.Context, project uuid.UUID, at time.Time, counters map[uuid.UUID]domain.Traffic) error
	Traffic(ctx context.Context, month time.Time) ([]domain.ProjectTraffic, error)
}

// Machines : VPS de la plateforme (module compute).
type Machines interface {
	VPS(ctx context.Context, id uuid.UUID) (*computeDomain.VPS, error)
}

// Cluster : ce que le VPC doit savoir du cluster d'un projet.
type Cluster struct {
	ID       string
	Provider string
	Region   string
}

// Clusters : cluster d'un projet (module clusters).
type Clusters interface {
	ProjectCluster(ctx context.Context, project uuid.UUID) (Cluster, error)
}

// Service : VPC des projets.
type Service struct {
	Store    Store
	Machines Machines
	Clusters Clusters
	// Backends : mises en œuvre disponibles, essayées dans l'ordre
	// (la première qui sait servir le placement est retenue).
	Backends []domain.VPCBackend
	// RegionOf : région d'un VPS d'après son emplacement ("" : inconnue).
	RegionOf func(v *computeDomain.VPS) string
	// APIURL : URL publique de l'API (…/kmanager/v1), appelée par les VPS
	// pour s'inscrire.
	APIURL string
	// Logger : erreurs des synchronisations en tâche de fond (nil : muettes).
	Logger interface {
		Warnw(msg string, kv ...any)
	}
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// ── Vue ────────────────────────────────────────────────────────────────

// MemberView : un VPS du réseau privé, vu par le client.
type MemberView struct {
	VPSID uuid.UUID `json:"vps_id"`
	Name  string    `json:"name"`
	// Address : adresse privée fixe ; InternalName : nom à utiliser.
	Address      string `json:"address"`
	InternalName string `json:"internal_name"`
	// State : pending (en attente du VPS) ou attached (relié).
	State string `json:"state"`
	// Connected : état vu par le réseau (nil : inconnu pour le moment).
	Connected     *bool      `json:"connected,omitempty"`
	AttachedAt    *time.Time `json:"attached_at,omitempty"`
	LastContactAt *time.Time `json:"last_contact_at,omitempty"`
}

// View : réseau privé (VPC) d'un projet.
type View struct {
	// Enabled : un VPC existe (au moins un VPS a été rattaché).
	Enabled bool `json:"enabled"`
	// Range : plage privée des VPS ("" tant qu'il n'y a pas de VPC).
	Range   string       `json:"range"`
	Suffix  string       `json:"name_suffix"`
	Members []MemberView `json:"members"`
}

// Get : VPC du projet et ses VPS. Une mise en œuvre muette n'empêche pas la
// lecture : l'état vu par le réseau est alors omis.
func (s *Service) Get(ctx context.Context, project uuid.UUID) (*View, error) {
	out := &View{Suffix: domain.NameSuffix, Members: []MemberView{}}
	n, err := s.Store.Network(ctx, project)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return out, nil
	}
	out.Enabled, out.Range = true, n.CIDR
	atts, err := s.Store.Attachments(ctx, project)
	if err != nil {
		return nil, err
	}
	live := s.liveStatus(ctx, n, atts)
	for i := range atts {
		a := &atts[i]
		v, err := s.Machines.VPS(ctx, a.VPSID)
		if err != nil {
			continue // VPS illisible : omis de la vue, pas d'échec
		}
		m := MemberView{VPSID: a.VPSID, Name: v.Name, Address: a.Address, InternalName: domain.InternalName(v.Name),
			State: a.State, AttachedAt: a.AttachedAt, LastContactAt: a.LastContactAt}
		if st, ok := live[a.VPSID]; ok {
			c := st.Connected
			m.Connected = &c
			if st.LastContact != nil {
				m.LastContactAt = st.LastContact
			}
		}
		out.Members = append(out.Members, m)
	}
	sort.SliceStable(out.Members, func(i, j int) bool { return out.Members[i].Name < out.Members[j].Name })
	return out, nil
}

func (s *Service) liveStatus(ctx context.Context, n *domain.Network, atts []domain.Attachment) map[uuid.UUID]domain.MemberStatus {
	out := map[uuid.UUID]domain.MemberStatus{}
	vpc, err := toVPC(n)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, a := range atts {
		if seen[a.Backend] {
			continue
		}
		seen[a.Backend] = true
		b := s.backendNamed(a.Backend)
		if b == nil {
			continue
		}
		list, err := b.Status(ctx, vpc)
		if err != nil {
			continue
		}
		for _, st := range list {
			out[st.VPSID] = st
		}
	}
	return out
}

// NetworkMembers : VPS rattachés, comme membres du réseau du projet (vue
// réseau du module network). Status : pending ou attached.
func (s *Service) NetworkMembers(ctx context.Context, project uuid.UUID) ([]netDomain.Member, error) {
	atts, err := s.Store.Attachments(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]netDomain.Member, 0, len(atts))
	for _, a := range atts {
		v, err := s.Machines.VPS(ctx, a.VPSID)
		if err != nil {
			continue
		}
		name := domain.InternalName(v.Name)
		out = append(out, netDomain.Member{ID: a.VPSID.String(), Kind: netDomain.KindVPS, Name: v.Name, Status: a.State,
			Internal: &netDomain.Address{Host: name, URL: name}, Public: []netDomain.PublicEndpoint{}})
	}
	return out, nil
}

// ── Rattacher ──────────────────────────────────────────────────────────

// Enrollment : jeton d'inscription, montré une seule fois.
type Enrollment struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// Command : à lancer en root sur le VPS (contient le jeton).
	Command string `json:"command"`
}

// AttachResult : rattachement d'un VPS ; Enrollment présent si le VPS doit
// s'inscrire lui-même.
type AttachResult struct {
	Member     MemberView  `json:"member"`
	Enrollment *Enrollment `json:"enrollment,omitempty"`
}

// attachable : états d'un VPS qui peut être rattaché.
func attachable(v *computeDomain.VPS) bool {
	return v.State == computeDomain.StateRunning || v.State == computeDomain.StateStopped
}

// Attach : VPS rattaché au VPC de son projet (créé au premier VPS).
// Idempotent : un VPS déjà rattaché garde son adresse ; s'il attend encore
// son inscription, un nouveau jeton remplace l'ancien.
func (s *Service) Attach(ctx context.Context, org, vpsID uuid.UUID, by string) (*AttachResult, error) {
	v, err := s.vps(ctx, org, vpsID)
	if err != nil {
		return nil, err
	}
	if !attachable(v) {
		return nil, domain.ErrVPSNotReady
	}
	out, err := s.attach(ctx, v, by, domain.EnrollmentTTL, true)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetWanted(ctx, v.ID, true); err != nil {
		return nil, err
	}
	return out, nil
}

// attach : rattachement (client, ou installation d'un VPS qui le demande) ;
// ttl : durée du jeton d'inscription éventuel. network faux : en base
// seulement (mode compute, sans accès aux clusters) ; le réseau suit à
// l'inscription ou à la synchronisation suivante.
func (s *Service) attach(ctx context.Context, v *computeDomain.VPS, by string, ttl time.Duration, network bool) (*AttachResult, error) {
	if existing, err := s.Store.Attachment(ctx, v.ID); err != nil {
		return nil, err
	} else if existing != nil {
		return s.again(ctx, v, existing, by, ttl)
	}
	cl, err := s.Clusters.ProjectCluster(ctx, v.ProjectID)
	if err != nil {
		return nil, err
	}
	if s.RegionOf != nil {
		if r := s.RegionOf(v); r != "" && cl.Region != "" && r != cl.Region {
			return nil, domain.ErrRegionMismatch
		}
	}
	b := s.pick(domain.Placement{VPSProvider: v.Provider, ClusterID: cl.ID, ClusterProvider: cl.Provider})
	if b == nil {
		return nil, domain.ErrUnavailable
	}
	a := &domain.Attachment{VPSID: v.ID, Backend: b.Name(), State: domain.StatePending, CreatedBy: by, CreatedAt: s.now()}
	n, err := s.Store.Attach(ctx, v.ProjectID, cl.ID, a)
	if errors.Is(err, domain.ErrAlreadyAttached) {
		existing, gerr := s.Store.Attachment(ctx, v.ID)
		if gerr != nil || existing == nil {
			return nil, err
		}
		return s.again(ctx, v, existing, by, ttl)
	}
	if err != nil {
		return nil, err
	}
	if !network {
		return s.finish(ctx, v, a, b.Capabilities().NeedsEnrollment, by, ttl)
	}
	vpc, m, err := s.member(n, a, v)
	if err == nil {
		err = b.Ensure(ctx, vpc, nil)
	}
	var plan *domain.AttachPlan
	if err == nil {
		plan, err = b.Attach(ctx, vpc, m)
	}
	if err != nil {
		// Rien n'est laissé à moitié : le rattachement est retiré.
		_ = b.Detach(context.WithoutCancel(ctx), vpc, v.ID)
		_, _ = s.Store.Detach(context.WithoutCancel(ctx), v.ID, v.ProjectID)
		return nil, err
	}
	return s.finish(ctx, v, a, plan.NeedsEnrollment, by, ttl)
}

// again : VPS déjà rattaché (requête répétée).
func (s *Service) again(ctx context.Context, v *computeDomain.VPS, a *domain.Attachment, by string, ttl time.Duration) (*AttachResult, error) {
	b := s.backendNamed(a.Backend)
	if b == nil {
		return nil, domain.ErrUnavailable
	}
	needs := a.State == domain.StatePending && b.Capabilities().NeedsEnrollment
	return s.finish(ctx, v, a, needs, by, ttl)
}

func (s *Service) finish(ctx context.Context, v *computeDomain.VPS, a *domain.Attachment, needsEnrollment bool, by string, ttl time.Duration) (*AttachResult, error) {
	out := &AttachResult{}
	if needsEnrollment {
		e, err := s.newEnrollment(ctx, v.ID, by, ttl)
		if err != nil {
			return nil, err
		}
		out.Enrollment = e
	} else if a.State != domain.StateAttached {
		at := s.now()
		if err := s.Store.SetState(ctx, v.ID, domain.StateAttached, "", at); err != nil {
			return nil, err
		}
		a.State, a.AttachedAt, a.LastContactAt = domain.StateAttached, &at, &at
	}
	out.Member = MemberView{VPSID: v.ID, Name: v.Name, Address: a.Address, InternalName: domain.InternalName(v.Name),
		State: a.State, AttachedAt: a.AttachedAt, LastContactAt: a.LastContactAt}
	return out, nil
}

// newEnrollment : 32 octets aléatoires ; seule l'empreinte SHA-256 est gardée.
func (s *Service) newEnrollment(ctx context.Context, vps uuid.UUID, by string, ttl time.Duration) (*Enrollment, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	now := s.now()
	e := &domain.Enrollment{ID: uuid.New(), VPSID: vps, TokenHash: sum[:], ExpiresAt: now.Add(ttl), CreatedBy: by, CreatedAt: now}
	if err := s.Store.NewEnrollment(ctx, e); err != nil {
		return nil, err
	}
	return &Enrollment{Token: token, ExpiresAt: e.ExpiresAt, Command: s.command(token)}, nil
}

// tokenPrefix : reconnaissable par les détecteurs de secrets.
const tokenPrefix = "bvpc_"

// ── Détacher ───────────────────────────────────────────────────────────

// Detach : le VPS n'a plus accès au réseau privé, tout de suite, même
// injoignable. Le dernier VPS parti, le réseau est démonté (sa plage reste
// au projet : un VPS rattaché plus tard retrouve la même).
func (s *Service) Detach(ctx context.Context, org, vpsID uuid.UUID) error {
	v, err := s.vps(ctx, org, vpsID)
	if err != nil {
		return err
	}
	a, err := s.Store.Attachment(ctx, v.ID)
	if err != nil {
		return err
	}
	if a == nil {
		return domain.ErrNotAttached
	}
	if err := s.release(ctx, a); err != nil {
		return err
	}
	return s.Store.SetWanted(ctx, v.ID, false)
}

// release : rattachement retiré, côté réseau d'abord (accès coupé), puis en
// base ; le dernier VPS parti, le réseau est démonté.
func (s *Service) release(ctx context.Context, a *domain.Attachment) error {
	n, err := s.Store.Network(ctx, a.ProjectID)
	if err != nil {
		return err
	}
	if n == nil {
		return nil
	}
	b := s.backendNamed(a.Backend)
	if b == nil {
		return domain.ErrUnavailable
	}
	vpc, err := toVPC(n)
	if err != nil {
		return err
	}
	if err := b.Detach(ctx, vpc, a.VPSID); err != nil {
		return err
	}
	left, err := s.Store.Detach(ctx, a.VPSID, a.ProjectID)
	if err != nil {
		return err
	}
	if left == 0 {
		return s.teardown(ctx, a.ProjectID, vpc, b)
	}
	return nil
}

// teardown : réseau démonté seulement s'il n'a toujours aucun VPS, sous le
// verrou du rattachement (un VPS rattaché entre-temps garde sa passerelle).
func (s *Service) teardown(ctx context.Context, project uuid.UUID, vpc domain.VPC, backends ...domain.VPCBackend) error {
	return s.Store.IfEmpty(ctx, project, func() error {
		var first error
		for _, b := range backends {
			if err := b.Teardown(ctx, vpc); err != nil && first == nil {
				first = err
			}
		}
		return first
	})
}

// ── Inscrire ───────────────────────────────────────────────────────────

// EnrollInput : envoyé par le VPS (clé publique générée sur lui).
type EnrollInput struct {
	Token     string `json:"token" binding:"required" description:"Jeton d'inscription (usage unique, 30 min)"`
	PublicKey string `json:"public_key" binding:"required" description:"Clé publique générée sur le VPS"`
	// SourceIP : adresse d'où vient la requête (posée par le contrôleur).
	SourceIP string `json:"-"`
}

// EnrollOutput : configuration à appliquer sur le VPS.
type EnrollOutput struct {
	Address string            `json:"address"`
	Range   string            `json:"range"`
	Suffix  string            `json:"name_suffix"`
	Config  map[string]string `json:"config"`
}

// Enroll : le VPS s'inscrit avec son jeton (consommé, même en cas d'échec
// de la mise en œuvre : un nouveau jeton se demande depuis la console).
func (s *Service) Enroll(ctx context.Context, in EnrollInput) (*EnrollOutput, error) {
	token := strings.TrimSpace(in.Token)
	key := strings.TrimSpace(in.PublicKey)
	if !strings.HasPrefix(token, tokenPrefix) {
		return nil, domain.ErrInvalidEnrollment
	}
	if !domain.ValidPublicKey(key) {
		return nil, domain.ErrInvalidPublicKey
	}
	sum := sha256.Sum256([]byte(token))
	e, err := s.Store.ConsumeEnrollment(ctx, sum[:], s.now())
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, domain.ErrInvalidEnrollment
	}
	a, err := s.Store.Attachment(ctx, e.VPSID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, domain.ErrInvalidEnrollment
	}
	v, err := s.Machines.VPS(ctx, e.VPSID)
	if err != nil || gone(v) {
		return nil, domain.ErrInvalidEnrollment
	}
	if !active(v) {
		return nil, domain.ErrVPSNotReady // suspendu : pas d'accès avant la reprise
	}
	src, public := publicSource(in.SourceIP)
	if public && v.IPv4 != "" && !sameAddr(src, v.IPv4) && !sameAddr(src, v.IPv6) {
		return nil, domain.ErrInvalidEnrollment // jeton présenté par une autre machine
	}
	n, err := s.Store.Network(ctx, a.ProjectID)
	if err != nil {
		return nil, err
	}
	b := s.backendNamed(a.Backend)
	if b == nil {
		return nil, domain.ErrUnavailable
	}
	vpc, m, err := s.member(n, a, v)
	if err != nil {
		return nil, err
	}
	if m.PublicIPv4 == "" && public && src.Is4() {
		m.PublicIPv4 = src.String() // IP pas encore relevée chez le fournisseur
	}
	// Le réseau connaît le VPS (rattaché en base seulement à sa création).
	if err := b.Ensure(ctx, vpc, []domain.Member{m}); err != nil {
		return nil, err
	}
	res, err := b.Enroll(ctx, domain.EnrollRequest{VPC: vpc, Member: m, PublicKey: key})
	if err != nil {
		return nil, err
	}
	if err := s.Store.SetState(ctx, v.ID, domain.StateAttached, key, s.now()); err != nil {
		return nil, err
	}
	return &EnrollOutput{Address: a.Address, Range: n.CIDR, Suffix: domain.NameSuffix, Config: res.Config}, nil
}

// ── Outils ─────────────────────────────────────────────────────────────

// vps : VPS de l'organisation (404 sinon, résilié compris).
func (s *Service) vps(ctx context.Context, org, id uuid.UUID) (*computeDomain.VPS, error) {
	v, err := s.Machines.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.OrganizationID != org || v.State == computeDomain.StateDeleted {
		return nil, computeDomain.ErrVPSNotFound
	}
	return v, nil
}

func (s *Service) pick(p domain.Placement) domain.VPCBackend {
	for _, b := range s.Backends {
		if b.Handles(p) {
			return b
		}
	}
	return nil
}

func (s *Service) backendNamed(name string) domain.VPCBackend {
	for _, b := range s.Backends {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

func toVPC(n *domain.Network) (domain.VPC, error) {
	p, err := netip.ParsePrefix(n.CIDR)
	if err != nil {
		return domain.VPC{}, err
	}
	return domain.VPC{ProjectID: n.ProjectID, ClusterID: n.ClusterID, CIDR: p, Gateway: domain.GatewayOf(p)}, nil
}

func (s *Service) member(n *domain.Network, a *domain.Attachment, v *computeDomain.VPS) (domain.VPC, domain.Member, error) {
	vpc, err := toVPC(n)
	if err != nil {
		return vpc, domain.Member{}, err
	}
	addr, err := netip.ParseAddr(a.Address)
	if err != nil {
		return vpc, domain.Member{}, err
	}
	return vpc, domain.Member{VPSID: v.ID, Name: v.Name, Address: addr, PublicKey: a.PublicKey, PublicIPv4: v.IPv4}, nil
}
