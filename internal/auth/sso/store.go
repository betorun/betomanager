package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/iam"
)

// ChallengePrefix : enregistrement TXT qui prouve un domaine
// (_betorun-sso.<domaine> = betorun-sso=<jeton>).
const (
	ChallengePrefix = "_betorun-sso."
	challengeValue  = "betorun-sso="
	maxDomains      = 20
)

// Rôles qu'une première connexion peut donner sur l'organisation.
var jitRoles = []string{"", "roles/viewer", "roles/editor"}

var (
	ErrNotConfigured   = errors.New("company sign-in is not configured for this organization")
	ErrNoSSO           = errors.New("no company sign-in is configured for this email domain")
	ErrSSORequired     = errors.New("your organization requires company sign-in (SSO) for this address")
	ErrClientID        = errors.New("the client ID is required (255 characters at most)")
	ErrClientSecret    = errors.New("the client secret is required (1024 characters at most)")
	ErrJITRole         = errors.New("the role given at first sign-in must be viewer, editor or none")
	ErrEnforceUntested = errors.New("before requiring company sign-in, verify a domain and sign in once with it")
	ErrDomainTaken     = errors.New("this domain is already verified by another organization")
	ErrDomainNotFound  = errors.New("domain not found")
	ErrDomainUnproven  = errors.New("the DNS record was not found yet (it can take a few minutes to appear)")
	ErrTooManyDomains  = errors.New("too many domains (20 at most)")
	ErrEmailDomain     = errors.New("the identity provider returned an address outside the organization's verified domains")
)

// Resolver : lecture DNS (remplaçable dans les tests).
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type configRow struct {
	OrganizationID  uuid.UUID `gorm:"primaryKey;type:uuid"`
	Issuer          string
	ClientID        string
	ClientSecretEnc []byte
	Enabled         bool
	Enforced        bool
	JITRole         string `gorm:"column:jit_role"`
	TestedAt        *time.Time
	UpdatedBy       string
	UpdatedAt       time.Time
}

func (configRow) TableName() string { return "org_sso" }

type domainRow struct {
	OrganizationID uuid.UUID `gorm:"primaryKey;type:uuid"`
	Domain         string    `gorm:"primaryKey"`
	Token          string
	VerifiedAt     *time.Time
	CreatedAt      time.Time
}

func (domainRow) TableName() string { return "org_sso_domains" }

// Config : vue API (jamais le secret).
type Config struct {
	Organization iam.ResourceName `json:"organization"`
	Configured   bool             `json:"configured"`
	Issuer       string           `json:"issuer"`
	ClientID     string           `json:"client_id"`
	HasSecret    bool             `json:"has_secret"`
	Enabled      bool             `json:"enabled"`
	Enforced     bool             `json:"enforced"`
	JITRole      string           `json:"jit_role"`
	// TestedAt : dernière connexion réussie par ce fournisseur.
	TestedAt  *time.Time `json:"tested_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	Domains   []Domain   `json:"domains"`
	// RedirectURI : adresse de retour à déclarer chez le fournisseur.
	RedirectURI string `json:"redirect_uri"`
}

// Domain : domaine revendiqué ; vérifié par un enregistrement TXT.
type Domain struct {
	Domain      string     `json:"domain"`
	Verified    bool       `json:"verified"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	RecordName  string     `json:"record_name"`
	RecordValue string     `json:"record_value"`
}

// Update : réglages envoyés par l'organisation (ClientSecret nil : inchangé).
type Update struct {
	Issuer       string
	ClientID     string
	ClientSecret *string
	Enabled      bool
	Enforced     bool
	JITRole      string
}

// Login : de quoi lancer une connexion pour une adresse.
type Login struct {
	Organization uuid.UUID
	Client       Client
	JITRole      string
	Domains      []string
}

// Store : configuration des organisations.
type Store struct {
	DB       *gorm.DB
	Sealer   *auth.Sealer
	OIDC     *OIDC
	Resolver Resolver
	// RedirectURI : page de retour de la console (/auth/sso/callback).
	RedirectURI string
}

func secretAD(org uuid.UUID) string { return "org-sso:" + org.String() }

func (s *Store) Get(ctx context.Context, org uuid.UUID) (*Config, error) {
	out := &Config{Organization: iam.OrganizationName(org), Domains: []Domain{}, RedirectURI: s.RedirectURI}
	var row configRow
	err := s.DB.WithContext(ctx).Where("organization_id = ?", org).Take(&row).Error
	switch {
	case err == nil:
		out.Configured, out.Issuer, out.ClientID = true, row.Issuer, row.ClientID
		out.HasSecret, out.Enabled, out.Enforced = len(row.ClientSecretEnc) > 0, row.Enabled, row.Enforced
		out.JITRole, out.TestedAt, out.UpdatedAt = row.JITRole, row.TestedAt, &row.UpdatedAt
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, err
	}
	var domains []domainRow
	if err := s.DB.WithContext(ctx).Where("organization_id = ?", org).Order("domain").Find(&domains).Error; err != nil {
		return nil, err
	}
	for _, d := range domains {
		out.Domains = append(out.Domains, domainView(d))
	}
	return out, nil
}

func domainView(d domainRow) Domain {
	return Domain{Domain: d.Domain, Verified: d.VerifiedAt != nil, VerifiedAt: d.VerifiedAt,
		RecordName: ChallengePrefix + d.Domain, RecordValue: challengeValue + d.Token}
}

// Save : réglages du fournisseur. Activer suppose une découverte OpenID
// réussie ; exiger le SSO, un domaine vérifié et une connexion réussie
// (sinon l'organisation s'enferme dehors).
func (s *Store) Save(ctx context.Context, caller iam.Principal, org uuid.UUID, in Update) (*Config, error) {
	issuer, err := s.OIDC.NormalizeIssuer(in.Issuer)
	if err != nil {
		return nil, err
	}
	clientID := strings.TrimSpace(in.ClientID)
	if clientID == "" || len(clientID) > 255 {
		return nil, ErrClientID
	}
	if !slices.Contains(jitRoles, in.JITRole) {
		return nil, ErrJITRole
	}
	var row configRow
	err = s.DB.WithContext(ctx).Where("organization_id = ?", org).Take(&row).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if row.Issuer != issuer || row.ClientID != clientID {
		row.TestedAt = nil // autre fournisseur : la preuve de connexion ne vaut plus
	}
	row.OrganizationID, row.Issuer, row.ClientID, row.JITRole = org, issuer, clientID, in.JITRole
	if in.ClientSecret != nil {
		secret := strings.TrimSpace(*in.ClientSecret)
		if secret == "" || len(secret) > 1024 {
			return nil, ErrClientSecret
		}
		if row.ClientSecretEnc, err = s.Sealer.Seal([]byte(secret), secretAD(org)); err != nil {
			return nil, err
		}
	}
	if len(row.ClientSecretEnc) == 0 {
		return nil, ErrClientSecret
	}
	if in.Enabled {
		if _, err := s.OIDC.Discover(ctx, issuer); err != nil {
			return nil, ErrDiscovery
		}
	}
	if in.Enforced {
		verified, err := s.verifiedDomains(ctx, org)
		if err != nil {
			return nil, err
		}
		if !in.Enabled || row.TestedAt == nil || len(verified) == 0 {
			return nil, ErrEnforceUntested
		}
	}
	row.Enabled, row.Enforced = in.Enabled, in.Enforced && in.Enabled
	row.UpdatedBy, row.UpdatedAt = string(caller), time.Now().UTC()
	if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "sso_issuer", issuer)
	audit.Annotate(ctx, "sso_enabled", row.Enabled)
	audit.Annotate(ctx, "sso_enforced", row.Enforced)
	if in.ClientSecret != nil {
		audit.Annotate(ctx, "sso_secret", "replaced")
	}
	return s.Get(ctx, org)
}

// Delete : retire la configuration (les domaines restent prouvés).
func (s *Store) Delete(ctx context.Context, org uuid.UUID) error {
	return s.DB.WithContext(ctx).Where("organization_id = ?", org).Delete(&configRow{}).Error
}

func (s *Store) verifiedDomains(ctx context.Context, org uuid.UUID) ([]string, error) {
	var out []string
	err := s.DB.WithContext(ctx).Model(&domainRow{}).Where("organization_id = ? AND verified_at IS NOT NULL", org).
		Order("domain").Pluck("domain", &out).Error
	return out, err
}

// AddDomain : revendique un domaine ; renvoie l'enregistrement TXT à poser.
func (s *Store) AddDomain(ctx context.Context, org uuid.UUID, raw string) (*Domain, error) {
	list, err := iam.NormalizeDomains([]string{raw})
	if err != nil || len(list) != 1 {
		return nil, iam.ErrInvalidDomain
	}
	d := list[0]
	var taken int64
	if err := s.DB.WithContext(ctx).Model(&domainRow{}).Where("domain = ? AND verified_at IS NOT NULL AND organization_id <> ?", d, org).
		Count(&taken).Error; err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrDomainTaken
	}
	var count int64
	if err := s.DB.WithContext(ctx).Model(&domainRow{}).Where("organization_id = ?", org).Count(&count).Error; err != nil {
		return nil, err
	}
	var existing domainRow
	if err := s.DB.WithContext(ctx).Where("organization_id = ? AND domain = ?", org, d).Take(&existing).Error; err == nil {
		v := domainView(existing)
		return &v, nil
	}
	if count >= maxDomains {
		return nil, ErrTooManyDomains
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	row := domainRow{OrganizationID: org, Domain: d, Token: hex.EncodeToString(b), CreatedAt: time.Now().UTC()}
	if err := s.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "sso_domain", d)
	v := domainView(row)
	return &v, nil
}

// VerifyDomain : cherche l'enregistrement TXT ; le premier qui le prouve garde le domaine.
func (s *Store) VerifyDomain(ctx context.Context, org uuid.UUID, d string) (*Domain, error) {
	d = strings.ToLower(strings.TrimSpace(d))
	var row domainRow
	if err := s.DB.WithContext(ctx).Where("organization_id = ? AND domain = ?", org, d).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrDomainNotFound
		}
		return nil, err
	}
	if row.VerifiedAt == nil {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		txt, _ := s.Resolver.LookupTXT(lctx, ChallengePrefix+d)
		found := false
		for _, t := range txt {
			if strings.TrimSpace(t) == challengeValue+row.Token {
				found = true
			}
		}
		if !found {
			return nil, ErrDomainUnproven
		}
		now := time.Now().UTC()
		err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var taken int64
			if err := tx.Model(&domainRow{}).Where("domain = ? AND verified_at IS NOT NULL AND organization_id <> ?", d, org).
				Count(&taken).Error; err != nil {
				return err
			}
			if taken > 0 {
				return ErrDomainTaken
			}
			return tx.Model(&domainRow{}).Where("organization_id = ? AND domain = ?", org, d).Update("verified_at", now).Error
		})
		if err != nil {
			return nil, err
		}
		row.VerifiedAt = &now
		audit.Annotate(ctx, "sso_domain_verified", d)
	}
	v := domainView(row)
	return &v, nil
}

// RemoveDomain : plus de connexion d'entreprise pour ce domaine. Le dernier
// domaine vérifié ne part pas tant que le SSO est exigé.
func (s *Store) RemoveDomain(ctx context.Context, org uuid.UUID, d string) error {
	d = strings.ToLower(strings.TrimSpace(d))
	var cfg configRow
	if err := s.DB.WithContext(ctx).Where("organization_id = ?", org).Take(&cfg).Error; err == nil && cfg.Enforced {
		verified, err := s.verifiedDomains(ctx, org)
		if err != nil {
			return err
		}
		if len(verified) == 1 && verified[0] == d {
			return ErrEnforceUntested
		}
	}
	res := s.DB.WithContext(ctx).Where("organization_id = ? AND domain = ?", org, d).Delete(&domainRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrDomainNotFound
	}
	audit.Annotate(ctx, "sso_domain_removed", d)
	return nil
}

// candidates : le domaine de l'adresse et ses parents (equipe.exemple.sn → exemple.sn).
func candidates(email string) []string {
	_, d, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok || d == "" {
		return nil
	}
	var out []string
	for {
		out = append(out, d)
		_, rest, ok := strings.Cut(d, ".")
		if !ok || !strings.Contains(rest, ".") {
			return out
		}
		d = rest
	}
}

// lookup : organisation qui a prouvé le domaine de l'adresse, et sa configuration.
func (s *Store) lookup(ctx context.Context, email string) (*configRow, error) {
	names := candidates(email)
	if len(names) == 0 {
		return nil, ErrNoSSO
	}
	var row configRow
	err := s.DB.WithContext(ctx).Raw(`SELECT c.* FROM org_sso c JOIN org_sso_domains d ON d.organization_id = c.organization_id
		WHERE d.domain IN ? AND d.verified_at IS NOT NULL AND c.enabled ORDER BY length(d.domain) DESC LIMIT 1`, names).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.OrganizationID == uuid.Nil {
		return nil, ErrNoSSO
	}
	return &row, nil
}

// ForEmail : connexion d'entreprise de l'adresse (ErrNoSSO sinon).
func (s *Store) ForEmail(ctx context.Context, email string) (*Login, error) {
	row, err := s.lookup(ctx, email)
	if err != nil {
		return nil, err
	}
	return s.login(ctx, row)
}

// ForOrganization : connexion d'entreprise d'une organisation.
func (s *Store) ForOrganization(ctx context.Context, org uuid.UUID) (*Login, error) {
	var row configRow
	if err := s.DB.WithContext(ctx).Where("organization_id = ? AND enabled", org).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotConfigured
		}
		return nil, err
	}
	return s.login(ctx, &row)
}

func (s *Store) login(ctx context.Context, row *configRow) (*Login, error) {
	secret, err := s.Sealer.Open(row.ClientSecretEnc, secretAD(row.OrganizationID))
	if err != nil {
		return nil, err
	}
	domains, err := s.verifiedDomains(ctx, row.OrganizationID)
	if err != nil {
		return nil, err
	}
	return &Login{Organization: row.OrganizationID, JITRole: row.JITRole, Domains: domains,
		Client: Client{Issuer: row.Issuer, ClientID: row.ClientID, ClientSecret: string(secret)}}, nil
}

// Required : l'adresse doit-elle passer par la connexion d'entreprise ?
func (s *Store) Required(ctx context.Context, email string) (bool, error) {
	if s == nil {
		return false, nil
	}
	row, err := s.lookup(ctx, email)
	if errors.Is(err, ErrNoSSO) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.Enforced, nil
}

// MarkTested : une connexion a réussi par ce fournisseur.
func (s *Store) MarkTested(ctx context.Context, org uuid.UUID) {
	_ = s.DB.WithContext(ctx).Model(&configRow{}).Where("organization_id = ?", org).Update("tested_at", time.Now().UTC()).Error
}

// ActorSSO : auteur des rôles donnés à la première connexion.
const ActorSSO = "system:sso"

// GrantFirstAccess : rôle de première connexion sur l'organisation, si la
// personne n'en a encore aucun.
func (s *Store) GrantFirstAccess(ctx context.Context, l *Login, user uuid.UUID) error {
	if l.JITRole == "" {
		return nil
	}
	granted, err := iam.GrantIfAbsent(ctx, s.DB, iam.OrganizationName(l.Organization), iam.UserPrincipal(user), l.JITRole, ActorSSO)
	if granted {
		audit.Annotate(ctx, "sso_role_granted", l.JITRole)
	}
	return err
}
