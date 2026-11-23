// Package service : lecture des documents juridiques, acceptation par les
// organisations, rédaction et publication par l'équipe (docs/modules/legal.md).
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/legal/domain"
)

// Store : stockage des versions et des acceptations.
type Store interface {
	// Current : dernière version publiée ; domain.ErrDocumentNotFound sans version publiée.
	Current(ctx context.Context, kind string) (*domain.Document, error)
	// Document : une version (brouillon compris) ; domain.ErrDocumentNotFound.
	Document(ctx context.Context, kind string, version int) (*domain.Document, error)
	// Versions : toutes les versions d'un document, la plus récente d'abord.
	Versions(ctx context.Context, kind string) ([]domain.Document, error)
	// CurrentVersions : numéro de la version en vigueur de chaque document publié.
	CurrentVersions(ctx context.Context) (map[string]int, error)
	// CreateDraft : numéro = dernier + 1 ; domain.ErrDraftExists si un brouillon existe.
	CreateDraft(ctx context.Context, d *domain.Document) error
	// UpdateDraft : domain.ErrNotDraft si la version est publiée.
	UpdateDraft(ctx context.Context, d *domain.Document) error
	// Publish : domain.ErrNotDraft si déjà publiée.
	Publish(ctx context.Context, kind string, version int, by string, at time.Time) (*domain.Document, error)
	// DeleteDraft : domain.ErrNotDraft si la version est publiée.
	DeleteDraft(ctx context.Context, kind string, version int) error
	// Accepted : dernière acceptation de chaque document par l'organisation.
	Accepted(ctx context.Context, org uuid.UUID) (map[string]domain.Acceptance, error)
	// Accept : idempotent (même version déjà acceptée : l'acceptation d'origine).
	Accept(ctx context.Context, a *domain.Acceptance) (*domain.Acceptance, error)
	// AcceptanceCounts : nombre d'organisations ayant accepté chaque version.
	AcceptanceCounts(ctx context.Context, kind string) (map[int]int64, error)
}

// Service : cas d'usage du module.
type Service struct {
	Store Store
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// ── Lecture publique ────────────────────────────────────────────────────

// Text : version d'un document dans une langue.
type Text struct {
	Kind        string    `json:"kind" description:"Document : terms_vps (conditions de vente des VPS) ou aup (politique d'usage acceptable)"`
	Version     int       `json:"version" description:"Numéro de la version"`
	Lang        string    `json:"lang" description:"Langue du texte : fr ou en"`
	Title       string    `json:"title" description:"Titre"`
	Body        string    `json:"body" description:"Texte au format Markdown"`
	PublishedAt time.Time `json:"published_at" description:"Date de publication"`
	Current     bool      `json:"current" description:"Version en vigueur"`
}

// Read : version en vigueur, ou une version publiée plus ancienne (version > 0).
// Un brouillon n'est jamais lu par ce chemin.
func (s *Service) Read(ctx context.Context, kind string, version int, lang string) (*Text, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	current, err := s.Store.Current(ctx, kind)
	if err != nil {
		return nil, err
	}
	d := current
	if version > 0 && version != current.Version {
		if d, err = s.Store.Document(ctx, kind, version); err != nil {
			return nil, err
		}
		if !d.Published() {
			return nil, domain.ErrDocumentNotFound
		}
	}
	title, body := d.Text(lang)
	t := &Text{Kind: kind, Version: d.Version, Lang: lang, Title: title, Body: body, Current: d.Version == current.Version}
	if d.PublishedAt != nil {
		t.PublishedAt = *d.PublishedAt
	}
	return t, nil
}

// ── Organisations ───────────────────────────────────────────────────────

// OrgDocument : où en est une organisation pour un document.
type OrgDocument struct {
	Kind            string     `json:"kind" description:"Document : terms_vps ou aup"`
	CurrentVersion  int        `json:"current_version" description:"Version en vigueur"`
	Title           string     `json:"title" description:"Titre de la version en vigueur, dans la langue de la requête"`
	PublishedAt     time.Time  `json:"published_at" description:"Date de publication de la version en vigueur"`
	AcceptedVersion *int       `json:"accepted_version" description:"Dernière version acceptée par l'organisation (null : jamais)"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty" description:"Date de cette acceptation"`
	AcceptedBy      string     `json:"accepted_by,omitempty" description:"Qui l'a acceptée"`
	NeedsAcceptance bool       `json:"needs_acceptance" description:"La version en vigueur reste à accepter"`
}

// OrgStatus : chaque document publié, version en vigueur et version acceptée.
func (s *Service) OrgStatus(ctx context.Context, org uuid.UUID, lang string) ([]OrgDocument, error) {
	accepted, err := s.Store.Accepted(ctx, org)
	if err != nil {
		return nil, err
	}
	out := []OrgDocument{}
	for _, kind := range domain.Kinds {
		cur, err := s.Store.Current(ctx, kind)
		if errors.Is(err, domain.ErrDocumentNotFound) {
			continue // rien de publié : rien à accepter
		}
		if err != nil {
			return nil, err
		}
		title, _ := cur.Text(lang)
		doc := OrgDocument{Kind: kind, CurrentVersion: cur.Version, Title: title, NeedsAcceptance: true}
		if cur.PublishedAt != nil {
			doc.PublishedAt = *cur.PublishedAt
		}
		if a, ok := accepted[kind]; ok {
			v, at := a.Version, a.AcceptedAt
			doc.AcceptedVersion, doc.AcceptedAt, doc.AcceptedBy = &v, &at, a.Principal
			doc.NeedsAcceptance = domain.NeedsAcceptance(cur.Version, v)
		}
		out = append(out, doc)
	}
	return out, nil
}

// Accept : l'organisation accepte la version en vigueur (et seulement elle).
func (s *Service) Accept(ctx context.Context, org uuid.UUID, kind string, version int, principal, ip string) (*domain.Acceptance, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	cur, err := s.Store.Current(ctx, kind)
	if errors.Is(err, domain.ErrDocumentNotFound) {
		return nil, domain.ErrNotCurrent
	}
	if err != nil {
		return nil, err
	}
	if version != cur.Version {
		return nil, domain.ErrNotCurrent
	}
	return s.Store.Accept(ctx, &domain.Acceptance{ID: uuid.New(), OrganizationID: org, Kind: kind, Version: version,
		Principal: principal, ClientIP: ip, AcceptedAt: s.now()})
}

// RequireAccepted : nil si l'organisation a accepté la version en vigueur de
// chacun de ces documents ; domain.ErrTermsNotAccepted sinon. Un document
// sans version publiée ne bloque pas. Voir contract.TermsGate.
func (s *Service) RequireAccepted(ctx context.Context, org uuid.UUID, kinds ...string) error {
	if len(kinds) == 0 {
		return nil
	}
	current, err := s.Store.CurrentVersions(ctx)
	if err != nil {
		return err
	}
	accepted, err := s.Store.Accepted(ctx, org)
	if err != nil {
		return err
	}
	for _, kind := range kinds {
		if !domain.ValidKind(kind) {
			return domain.ErrUnknownKind
		}
		if domain.NeedsAcceptance(current[kind], accepted[kind].Version) {
			return domain.ErrTermsNotAccepted
		}
	}
	return nil
}

// ── Équipe ──────────────────────────────────────────────────────────────

// VersionSummary : une version, sans son texte, avec ses acceptations.
type VersionSummary struct {
	Kind        string     `json:"kind"`
	Version     int        `json:"version"`
	TitleFR     string     `json:"title_fr"`
	TitleEN     string     `json:"title_en"`
	Status      string     `json:"status"`
	Current     bool       `json:"current"`
	Acceptances int64      `json:"acceptances"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedBy   string     `json:"updated_by"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PublishedBy string     `json:"published_by,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// KindSummary : un document : version en vigueur, brouillon en cours.
type KindSummary struct {
	Kind           string `json:"kind"`
	CurrentVersion int    `json:"current_version"`
	DraftVersion   int    `json:"draft_version"`
	// CurrentAcceptances : organisations ayant accepté la version en vigueur.
	CurrentAcceptances int64 `json:"current_acceptances"`
}

// Overview : chaque document (version en vigueur, brouillon, acceptations).
func (s *Service) Overview(ctx context.Context) ([]KindSummary, error) {
	out := make([]KindSummary, 0, len(domain.Kinds))
	for _, kind := range domain.Kinds {
		versions, err := s.Versions(ctx, kind)
		if err != nil {
			return nil, err
		}
		k := KindSummary{Kind: kind}
		for _, v := range versions {
			switch {
			case v.Status == domain.StatusDraft:
				k.DraftVersion = v.Version
			case v.Current:
				k.CurrentVersion, k.CurrentAcceptances = v.Version, v.Acceptances
			}
		}
		out = append(out, k)
	}
	return out, nil
}

// Versions : versions d'un document, la plus récente d'abord, avec le
// nombre d'organisations qui ont accepté chacune.
func (s *Service) Versions(ctx context.Context, kind string) ([]VersionSummary, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	docs, err := s.Store.Versions(ctx, kind)
	if err != nil {
		return nil, err
	}
	counts, err := s.Store.AcceptanceCounts(ctx, kind)
	if err != nil {
		return nil, err
	}
	current := 0
	for _, d := range docs {
		if d.Published() && d.Version > current {
			current = d.Version
		}
	}
	out := make([]VersionSummary, 0, len(docs))
	for _, d := range docs {
		out = append(out, VersionSummary{Kind: d.Kind, Version: d.Version, TitleFR: d.TitleFR, TitleEN: d.TitleEN,
			Status: d.Status, Current: d.Version == current && d.Published(), Acceptances: counts[d.Version],
			CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt, UpdatedBy: d.UpdatedBy, UpdatedAt: d.UpdatedAt,
			PublishedBy: d.PublishedBy, PublishedAt: d.PublishedAt})
	}
	return out, nil
}

// VersionDetail : une version complète, pour l'équipe.
type VersionDetail struct {
	domain.Document
	Current     bool  `json:"current"`
	Acceptances int64 `json:"acceptances"`
}

func (s *Service) Version(ctx context.Context, kind string, version int) (*VersionDetail, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	d, err := s.Store.Document(ctx, kind, version)
	if err != nil {
		return nil, err
	}
	return s.detail(ctx, d)
}

func (s *Service) detail(ctx context.Context, d *domain.Document) (*VersionDetail, error) {
	counts, err := s.Store.AcceptanceCounts(ctx, d.Kind)
	if err != nil {
		return nil, err
	}
	out := &VersionDetail{Document: *d, Acceptances: counts[d.Version]}
	if d.Published() {
		cur, err := s.Store.CurrentVersions(ctx)
		if err != nil {
			return nil, err
		}
		out.Current = cur[d.Kind] == d.Version
	}
	return out, nil
}

// CreateDraft : nouveau brouillon. Sans texte fourni, copie de la version en
// vigueur (ou de la dernière version) à retoucher.
func (s *Service) CreateDraft(ctx context.Context, kind string, c domain.Content, by string) (*VersionDetail, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	if c.Empty() {
		src, err := s.Store.Current(ctx, kind)
		if errors.Is(err, domain.ErrDocumentNotFound) {
			return nil, domain.ErrNothingToCopy
		}
		if err != nil {
			return nil, err
		}
		c = domain.ContentOf(src)
	}
	c = c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	now := s.now()
	d := &domain.Document{Kind: kind, TitleFR: c.TitleFR, TitleEN: c.TitleEN, BodyFR: c.BodyFR, BodyEN: c.BodyEN,
		Status: domain.StatusDraft, CreatedBy: by, CreatedAt: now, UpdatedBy: by, UpdatedAt: now}
	if err := s.Store.CreateDraft(ctx, d); err != nil {
		return nil, err
	}
	return &VersionDetail{Document: *d}, nil
}

// UpdateDraft : réécrit un brouillon (titre et texte, deux langues).
func (s *Service) UpdateDraft(ctx context.Context, kind string, version int, c domain.Content, by string) (*VersionDetail, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	c = c.Normalize()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	d, err := s.Store.Document(ctx, kind, version)
	if err != nil {
		return nil, err
	}
	if d.Published() {
		return nil, domain.ErrNotDraft
	}
	d.TitleFR, d.TitleEN, d.BodyFR, d.BodyEN = c.TitleFR, c.TitleEN, c.BodyFR, c.BodyEN
	d.UpdatedBy, d.UpdatedAt = by, s.now()
	if err := s.Store.UpdateDraft(ctx, d); err != nil {
		return nil, err
	}
	return &VersionDetail{Document: *d}, nil
}

// Publish : le brouillon devient la version en vigueur ; les organisations
// doivent l'accepter de nouveau. Une version publiée ne change plus.
func (s *Service) Publish(ctx context.Context, kind string, version int, by string) (*VersionDetail, error) {
	if !domain.ValidKind(kind) {
		return nil, domain.ErrUnknownKind
	}
	d, err := s.Store.Document(ctx, kind, version)
	if err != nil {
		return nil, err
	}
	if d.Published() {
		return nil, domain.ErrNotDraft
	}
	if err := domain.ContentOf(d).Validate(); err != nil {
		return nil, err
	}
	d, err = s.Store.Publish(ctx, kind, version, by, s.now())
	if err != nil {
		return nil, err
	}
	return s.detail(ctx, d)
}

// DeleteDraft : abandonne un brouillon (jamais une version publiée).
func (s *Service) DeleteDraft(ctx context.Context, kind string, version int) error {
	if !domain.ValidKind(kind) {
		return domain.ErrUnknownKind
	}
	return s.Store.DeleteDraft(ctx, kind, version)
}
