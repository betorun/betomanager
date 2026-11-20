package iam

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/audit"
)

// Politiques d'organisation (iam.md §13) : contraintes posées sur une
// organisation et valables pour tous ses projets.
const (
	// ConstraintRequireMFA : toute personne qui agit sur l'organisation ou ses
	// projets doit s'être authentifiée avec double authentification. Les
	// comptes de service ne sont pas concernés (leur preuve est leur clé).
	ConstraintRequireMFA = "iam.requireMfa"
	// ConstraintAllowedMemberDomains : seules les personnes dont l'e-mail est
	// sur l'un de ces domaines peuvent recevoir un accès (organisation,
	// projets, groupes). Les accès déjà donnés restent.
	ConstraintAllowedMemberDomains = "iam.allowedMemberDomains"
	// ConstraintDisableKeyCreation : plus de nouvelle clé de compte de
	// service (clé API ou clé de signature) ; la fédération (STS) reste.
	ConstraintDisableKeyCreation = "iam.disableServiceAccountKeyCreation"
	// ConstraintDataAccessLogs : les lectures de données (logs, variables…)
	// entrent au journal d'audit.
	ConstraintDataAccessLogs = "logging.dataAccessLogs"
)

// MaxAllowedDomains : nombre maximal de domaines autorisés.
const MaxAllowedDomains = 20

// orgPolicyTTL : délai maximal de prise d'effet d'un changement sur les autres réplicas.
const orgPolicyTTL = 30 * time.Second

var (
	ErrMFANotConfigured       = errors.New("two-factor authentication is not configured on this platform")
	ErrInvalidDomain          = errors.New("invalid domain name")
	ErrTooManyDomains         = errors.New("too many allowed domains (20 at most)")
	ErrMemberDomainNotAllowed = errors.New("the organization only allows members from its own domains")
	ErrPublicMemberNotAllowed = errors.New("the organization only allows members from its own domains: public access is refused")
	ErrKeyCreationDisabled    = errors.New("the organization does not allow new service account keys: use workload identity federation instead")
)

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

type orgPolicyRow struct {
	ResourceName   string   `gorm:"primaryKey"`
	ConstraintName string   `gorm:"primaryKey"`
	Enforced       bool     `gorm:"not null"`
	ListValues     []string `gorm:"serializer:json;type:jsonb;not null"`
	UpdatedBy      string
	UpdatedAt      time.Time
}

func (orgPolicyRow) TableName() string { return "iam_org_policies" }

// OrgSecurityPolicy : vue API des contraintes d'une organisation.
type OrgSecurityPolicy struct {
	Organization ResourceName `json:"organization"`
	RequireMFA   bool         `json:"require_mfa"`
	// MFAAvailable : la plateforme sait vérifier la double authentification.
	MFAAvailable bool `json:"mfa_available"`
	// AllowedMemberDomains : vide = tout le monde peut être invité.
	AllowedMemberDomains []string   `json:"allowed_member_domains"`
	DisableKeyCreation   bool       `json:"disable_key_creation"`
	DataAccessLogs       bool       `json:"data_access_logs"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

// OrgPolicyUpdate : champs à changer (nil : inchangé).
type OrgPolicyUpdate struct {
	RequireMFA           *bool
	AllowedMemberDomains *[]string
	DisableKeyCreation   *bool
	DataAccessLogs       *bool
}

// OrgPolicyService : lecture (avec cache court) et écriture des contraintes.
type OrgPolicyService struct {
	DB     *gorm.DB
	Engine *Engine
	// MFAAvailable : faux si la plateforme n'a pas de niveau MFA configuré.
	MFAAvailable bool
	now          func() time.Time

	mu    sync.Mutex
	cache map[uuid.UUID]orgPolicyEntry
}

type orgPolicyEntry struct {
	policy OrgSecurityPolicy
	at     time.Time
}

func NewOrgPolicyService(db *gorm.DB, engine *Engine, mfaAvailable bool) *OrgPolicyService {
	return &OrgPolicyService{DB: db, Engine: engine, MFAAvailable: mfaAvailable, now: time.Now, cache: map[uuid.UUID]orgPolicyEntry{}}
}

// forResource : contraintes de l'organisation de r (nil hors organisation).
func (s *OrgPolicyService) forResource(ctx context.Context, r ResourceName) (*OrgSecurityPolicy, error) {
	if s == nil {
		return nil, nil
	}
	if org, ok := r.Organization(); ok {
		return s.cached(ctx, org)
	}
	ancestry, err := s.Engine.Ancestry(ctx, r)
	if err != nil {
		return nil, err
	}
	for _, a := range ancestry {
		if org, ok := a.Organization(); ok {
			return s.cached(ctx, org)
		}
	}
	return nil, nil
}

func (s *OrgPolicyService) cached(ctx context.Context, org uuid.UUID) (*OrgSecurityPolicy, error) {
	now := s.now()
	s.mu.Lock()
	e, ok := s.cache[org]
	s.mu.Unlock()
	if ok && now.Sub(e.at) < orgPolicyTTL {
		p := e.policy
		return &p, nil
	}
	p, err := s.Get(ctx, org)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[org] = orgPolicyEntry{policy: *p, at: now}
	s.mu.Unlock()
	return p, nil
}

// RequiresMFA : l'organisation de r exige-t-elle la double authentification ?
func (s *OrgPolicyService) RequiresMFA(ctx context.Context, r ResourceName) (bool, error) {
	if s == nil || !s.MFAAvailable {
		return false, nil
	}
	p, err := s.forResource(ctx, r)
	return p != nil && p.RequireMFA, err
}

// DataAccessLogged : les lectures de données sur r entrent-elles au journal ?
func (s *OrgPolicyService) DataAccessLogged(ctx context.Context, r ResourceName) (bool, error) {
	p, err := s.forResource(ctx, r)
	return p != nil && p.DataAccessLogs, err
}

// KeyCreationDisabled : la création de clés de compte de service est-elle interdite sur r ?
func (s *OrgPolicyService) KeyCreationDisabled(ctx context.Context, r ResourceName) (bool, error) {
	p, err := s.forResource(ctx, r)
	return p != nil && p.DisableKeyCreation, err
}

// CheckMembers : les nouveaux membres respectent-ils les domaines autorisés
// de l'organisation de r ? Les comptes de service et groupes passent (ils
// appartiennent à l'organisation) ; l'accès public est refusé.
func (s *OrgPolicyService) CheckMembers(ctx context.Context, r ResourceName, added []Principal) error {
	if len(added) == 0 {
		return nil
	}
	p, err := s.forResource(ctx, r)
	if err != nil || p == nil || len(p.AllowedMemberDomains) == 0 {
		return err
	}
	var users []uuid.UUID
	for _, m := range added {
		if m == AllUsers || m == AllAuthenticatedUsers {
			return ErrPublicMemberNotAllowed
		}
		if kind, id, err := m.Parse(); err == nil && kind == KindUser {
			users = append(users, id)
		}
	}
	if len(users) == 0 {
		return nil
	}
	var emails []string
	if err := s.DB.WithContext(ctx).Raw(`SELECT email FROM users WHERE id IN ?`, users).Scan(&emails).Error; err != nil {
		return err
	}
	for _, e := range emails {
		if !DomainAllowed(e, p.AllowedMemberDomains) {
			return fmt.Errorf("%w: %s", ErrMemberDomainNotAllowed, e)
		}
	}
	return nil
}

// DomainAllowed : l'e-mail est-il sur l'un des domaines (ou un sous-domaine) ?
func DomainAllowed(email string, domains []string) bool {
	_, d, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return false
	}
	for _, allowed := range domains {
		if d == allowed || strings.HasSuffix(d, "."+allowed) {
			return true
		}
	}
	return false
}

// NormalizeDomains : minuscules, sans doublon, triés ; refuse un nom invalide.
func NormalizeDomains(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		d = strings.TrimPrefix(d, "@")
		if d == "" {
			continue
		}
		if len(d) > 253 || !domainRe.MatchString(d) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidDomain, d)
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	if len(out) > MaxAllowedDomains {
		return nil, ErrTooManyDomains
	}
	slices.Sort(out)
	return out, nil
}

func (s *OrgPolicyService) Get(ctx context.Context, org uuid.UUID) (*OrgSecurityPolicy, error) {
	var rows []orgPolicyRow
	if err := s.DB.WithContext(ctx).Where("resource_name = ?", string(OrganizationName(org))).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := &OrgSecurityPolicy{Organization: OrganizationName(org), MFAAvailable: s.MFAAvailable, AllowedMemberDomains: []string{}}
	for _, r := range rows {
		switch r.ConstraintName {
		case ConstraintRequireMFA:
			out.RequireMFA = r.Enforced && s.MFAAvailable
		case ConstraintAllowedMemberDomains:
			if r.Enforced && len(r.ListValues) > 0 {
				out.AllowedMemberDomains = r.ListValues
			}
		case ConstraintDisableKeyCreation:
			out.DisableKeyCreation = r.Enforced
		case ConstraintDataAccessLogs:
			out.DataAccessLogs = r.Enforced
		}
		if at := r.UpdatedAt; out.UpdatedAt == nil || at.After(*out.UpdatedAt) {
			out.UpdatedAt = &at
		}
	}
	return out, nil
}

// SetRequireMFA : raccourci d'Update pour la seule double authentification.
func (s *OrgPolicyService) SetRequireMFA(ctx context.Context, caller Principal, org uuid.UUID, enforced bool) (*OrgSecurityPolicy, error) {
	return s.Update(ctx, caller, org, OrgPolicyUpdate{RequireMFA: &enforced})
}

// Update : l'appelant passe par le step-up (permission sensible) : il ne
// peut donc pas s'exclure lui-même en exigeant la double authentification.
func (s *OrgPolicyService) Update(ctx context.Context, caller Principal, org uuid.UUID, in OrgPolicyUpdate) (*OrgSecurityPolicy, error) {
	now := s.now()
	var rows []orgPolicyRow
	row := func(name string, enforced bool, values []string) {
		if values == nil {
			values = []string{}
		}
		rows = append(rows, orgPolicyRow{ResourceName: string(OrganizationName(org)), ConstraintName: name,
			Enforced: enforced, ListValues: values, UpdatedBy: string(caller), UpdatedAt: now})
		audit.Annotate(ctx, name, enforcedOrValues(enforced, values))
	}
	if in.RequireMFA != nil {
		if *in.RequireMFA && !s.MFAAvailable {
			return nil, ErrMFANotConfigured
		}
		row(ConstraintRequireMFA, *in.RequireMFA, nil)
	}
	if in.AllowedMemberDomains != nil {
		domains, err := NormalizeDomains(*in.AllowedMemberDomains)
		if err != nil {
			return nil, err
		}
		row(ConstraintAllowedMemberDomains, len(domains) > 0, domains)
	}
	if in.DisableKeyCreation != nil {
		row(ConstraintDisableKeyCreation, *in.DisableKeyCreation, nil)
	}
	if in.DataAccessLogs != nil {
		row(ConstraintDataAccessLogs, *in.DataAccessLogs, nil)
	}
	if len(rows) > 0 {
		if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows).Error; err != nil {
			return nil, err
		}
		s.mu.Lock()
		delete(s.cache, org)
		s.mu.Unlock()
	}
	return s.Get(ctx, org)
}

func enforcedOrValues(enforced bool, values []string) any {
	if len(values) > 0 {
		return values
	}
	return enforced
}
