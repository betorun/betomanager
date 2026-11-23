package service

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/legal/domain"
)

// memStore : Store en mémoire, mêmes règles que le dépôt Postgres.
type memStore struct {
	docs map[string]map[int]*domain.Document
	acc  []domain.Acceptance
}

func newMem() *memStore { return &memStore{docs: map[string]map[int]*domain.Document{}} }

func (m *memStore) Current(_ context.Context, kind string) (*domain.Document, error) {
	var best *domain.Document
	for _, d := range m.docs[kind] {
		if d.Published() && (best == nil || d.Version > best.Version) {
			best = d
		}
	}
	if best == nil {
		return nil, domain.ErrDocumentNotFound
	}
	c := *best
	return &c, nil
}

func (m *memStore) Document(_ context.Context, kind string, v int) (*domain.Document, error) {
	d, ok := m.docs[kind][v]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	c := *d
	return &c, nil
}

func (m *memStore) Versions(_ context.Context, kind string) ([]domain.Document, error) {
	out := []domain.Document{}
	for _, d := range m.docs[kind] {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

func (m *memStore) CurrentVersions(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for kind := range m.docs {
		if d, err := m.Current(ctx, kind); err == nil {
			out[kind] = d.Version
		}
	}
	return out, nil
}

func (m *memStore) CreateDraft(_ context.Context, d *domain.Document) error {
	max := 0
	for v, x := range m.docs[d.Kind] {
		if x.Status == domain.StatusDraft {
			return domain.ErrDraftExists
		}
		if v > max {
			max = v
		}
	}
	d.Version = max + 1
	if m.docs[d.Kind] == nil {
		m.docs[d.Kind] = map[int]*domain.Document{}
	}
	c := *d
	m.docs[d.Kind][d.Version] = &c
	return nil
}

func (m *memStore) UpdateDraft(_ context.Context, d *domain.Document) error {
	x, ok := m.docs[d.Kind][d.Version]
	if !ok {
		return domain.ErrDocumentNotFound
	}
	if x.Published() {
		return domain.ErrNotDraft
	}
	c := *d
	m.docs[d.Kind][d.Version] = &c
	return nil
}

func (m *memStore) Publish(ctx context.Context, kind string, v int, by string, at time.Time) (*domain.Document, error) {
	x, ok := m.docs[kind][v]
	if !ok {
		return nil, domain.ErrDocumentNotFound
	}
	if x.Published() {
		return nil, domain.ErrNotDraft
	}
	x.Status, x.PublishedBy, x.PublishedAt = domain.StatusPublished, by, &at
	return m.Document(ctx, kind, v)
}

func (m *memStore) DeleteDraft(_ context.Context, kind string, v int) error {
	x, ok := m.docs[kind][v]
	if !ok {
		return domain.ErrDocumentNotFound
	}
	if x.Published() {
		return domain.ErrNotDraft
	}
	delete(m.docs[kind], v)
	return nil
}

func (m *memStore) Accepted(_ context.Context, org uuid.UUID) (map[string]domain.Acceptance, error) {
	out := map[string]domain.Acceptance{}
	for _, a := range m.acc {
		if a.OrganizationID == org && a.Version > out[a.Kind].Version {
			out[a.Kind] = a
		}
	}
	return out, nil
}

func (m *memStore) Accept(_ context.Context, a *domain.Acceptance) (*domain.Acceptance, error) {
	for _, x := range m.acc {
		if x.OrganizationID == a.OrganizationID && x.Kind == a.Kind && x.Version == a.Version {
			return &x, nil
		}
	}
	m.acc = append(m.acc, *a)
	return a, nil
}

func (m *memStore) AcceptanceCounts(_ context.Context, kind string) (map[int]int64, error) {
	out := map[int]int64{}
	for _, a := range m.acc {
		if a.Kind == kind {
			out[a.Version]++
		}
	}
	return out, nil
}

var text = domain.Content{TitleFR: "Conditions", TitleEN: "Terms", BodyFR: "Texte v1", BodyEN: "Text v1"}

func TestLegalLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC)
	s := &Service{Store: newMem(), Now: func() time.Time { return now }}
	org := uuid.New()

	// Rien de publié : rien à accepter, et pas de copie possible.
	require.NoError(t, s.RequireAccepted(ctx, org, domain.KindTermsVPS))
	_, err := s.CreateDraft(ctx, domain.KindTermsVPS, domain.Content{}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrNothingToCopy)
	_, err = s.Read(ctx, domain.KindTermsVPS, 0, domain.LangFR)
	assert.ErrorIs(t, err, domain.ErrDocumentNotFound)

	v1, err := s.CreateDraft(ctx, domain.KindTermsVPS, text, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, 1, v1.Version)
	_, err = s.CreateDraft(ctx, domain.KindTermsVPS, text, "user:staff")
	assert.ErrorIs(t, err, domain.ErrDraftExists, "un seul brouillon à la fois")
	_, err = s.Read(ctx, domain.KindTermsVPS, 1, domain.LangFR)
	assert.ErrorIs(t, err, domain.ErrDocumentNotFound, "un brouillon n'est pas public")
	assert.NoError(t, s.RequireAccepted(ctx, org, domain.KindTermsVPS), "brouillon : rien à accepter")

	_, err = s.Publish(ctx, domain.KindTermsVPS, 1, "user:staff")
	require.NoError(t, err)
	_, err = s.UpdateDraft(ctx, domain.KindTermsVPS, 1, text, "user:staff")
	assert.ErrorIs(t, err, domain.ErrNotDraft, "publiée : immuable")
	assert.ErrorIs(t, s.DeleteDraft(ctx, domain.KindTermsVPS, 1), domain.ErrNotDraft)
	_, err = s.Publish(ctx, domain.KindTermsVPS, 1, "user:staff")
	assert.ErrorIs(t, err, domain.ErrNotDraft)

	assert.ErrorIs(t, s.RequireAccepted(ctx, org, domain.KindTermsVPS, domain.KindAUP), domain.ErrTermsNotAccepted)
	st, err := s.OrgStatus(ctx, org, domain.LangEN)
	require.NoError(t, err)
	require.Len(t, st, 1, "aup sans version publiée : absent")
	assert.True(t, st[0].NeedsAcceptance)
	assert.Nil(t, st[0].AcceptedVersion)
	assert.Equal(t, "Terms", st[0].Title)

	_, err = s.Accept(ctx, org, domain.KindTermsVPS, 2, "user:client", "203.0.113.7")
	assert.ErrorIs(t, err, domain.ErrNotCurrent)
	_, err = s.Accept(ctx, org, domain.KindAUP, 1, "user:client", "")
	assert.ErrorIs(t, err, domain.ErrNotCurrent, "rien de publié")
	_, err = s.Accept(ctx, org, "cgu", 1, "user:client", "")
	assert.ErrorIs(t, err, domain.ErrUnknownKind)
	a, err := s.Accept(ctx, org, domain.KindTermsVPS, 1, "user:client", "203.0.113.7")
	require.NoError(t, err)
	again, err := s.Accept(ctx, org, domain.KindTermsVPS, 1, "user:autre", "")
	require.NoError(t, err)
	assert.Equal(t, a.ID, again.ID, "idempotent")
	require.NoError(t, s.RequireAccepted(ctx, org, domain.KindTermsVPS, domain.KindAUP))
	assert.ErrorIs(t, s.RequireAccepted(ctx, org, "cgu"), domain.ErrUnknownKind)

	// Nouvelle version : copie par défaut, puis publication : à accepter de nouveau.
	v2, err := s.CreateDraft(ctx, domain.KindTermsVPS, domain.Content{}, "user:staff")
	require.NoError(t, err)
	assert.Equal(t, 2, v2.Version)
	assert.Equal(t, "Texte v1", v2.BodyFR, "copie de la version en vigueur")
	_, err = s.UpdateDraft(ctx, domain.KindTermsVPS, 2, domain.Content{TitleFR: "Conditions", TitleEN: "Terms", BodyFR: "Texte v2", BodyEN: "Text v2"}, "user:staff")
	require.NoError(t, err)
	_, err = s.UpdateDraft(ctx, domain.KindTermsVPS, 2, domain.Content{TitleFR: "x"}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrInvalidDocument)
	require.NoError(t, s.RequireAccepted(ctx, org, domain.KindTermsVPS), "brouillon : la v1 reste en vigueur")
	pub, err := s.Publish(ctx, domain.KindTermsVPS, 2, "user:staff")
	require.NoError(t, err)
	assert.True(t, pub.Current)
	assert.ErrorIs(t, s.RequireAccepted(ctx, org, domain.KindTermsVPS), domain.ErrTermsNotAccepted)

	cur, err := s.Read(ctx, domain.KindTermsVPS, 0, domain.LangEN)
	require.NoError(t, err)
	assert.Equal(t, 2, cur.Version)
	assert.Equal(t, "Text v2", cur.Body)
	assert.True(t, cur.Current)
	old, err := s.Read(ctx, domain.KindTermsVPS, 1, domain.LangFR)
	require.NoError(t, err)
	assert.Equal(t, "Texte v1", old.Body)
	assert.False(t, old.Current)

	versions, err := s.Versions(ctx, domain.KindTermsVPS)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, 2, versions[0].Version)
	assert.True(t, versions[0].Current)
	assert.Equal(t, int64(1), versions[1].Acceptances)
	over, err := s.Overview(ctx)
	require.NoError(t, err)
	assert.Equal(t, KindSummary{Kind: domain.KindTermsVPS, CurrentVersion: 2}, over[0])
	assert.Equal(t, KindSummary{Kind: domain.KindAUP}, over[1])

	// Un brouillon s'abandonne.
	_, err = s.CreateDraft(ctx, domain.KindTermsVPS, domain.Content{}, "user:staff")
	require.NoError(t, err)
	require.NoError(t, s.DeleteDraft(ctx, domain.KindTermsVPS, 3))
	_, err = s.Version(ctx, domain.KindTermsVPS, 3)
	assert.ErrorIs(t, err, domain.ErrDocumentNotFound)
}
