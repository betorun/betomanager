package service_test

// Conditions et acceptations sur un vrai Postgres (migrations jouées, textes
// de la migration 000066 compris). Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/legal/... -count=1
//
// IAM_E2E_DSN remplace la base par défaut (127.0.0.1:55433).

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/modules/legal/contract"
	"github.com/thekrauss/kubemanager/internal/modules/legal/domain"
	"github.com/thekrauss/kubemanager/internal/modules/legal/repository"
	"github.com/thekrauss/kubemanager/internal/modules/legal/service"
)

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	dsn := os.Getenv("IAM_E2E_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

func TestE2E_LegalPublishAcceptRequire(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	s := &service.Service{Store: &repository.GormStore{DB: db}}
	var gate contract.TermsGate = s
	org := uuid.New()

	// Textes de la migration : publiés, marqués « à faire relire », sans nom de fournisseur.
	for _, kind := range domain.Kinds {
		for _, lang := range []string{domain.LangFR, domain.LangEN} {
			v1, err := s.Read(ctx, kind, 1, lang)
			require.NoError(t, err, kind)
			assert.Contains(t, v1.Body, map[string]string{domain.LangFR: "Projet à faire relire par un juriste", domain.LangEN: "reviewed by a lawyer"}[lang])
			assert.NotContains(t, v1.Body, "OVH")
		}
	}
	terms, err := s.Read(ctx, domain.KindTermsVPS, 1, domain.LangFR)
	require.NoError(t, err)
	assert.Contains(t, terms.Body, "[à compléter : droit applicable et juridiction]")
	aup, err := s.Read(ctx, domain.KindAUP, 1, domain.LangFR)
	require.NoError(t, err)
	assert.Contains(t, aup.Body, "[adresse de contact abus à compléter]")

	// Avant acceptation : refusé.
	assert.ErrorIs(t, gate.RequireAccepted(ctx, org, contract.KindTermsVPS, contract.KindAUP), contract.ErrTermsNotAccepted)
	docs, err := s.OrgStatus(ctx, org, domain.LangFR)
	require.NoError(t, err)
	require.Len(t, docs, 2)
	for _, d := range docs {
		assert.True(t, d.NeedsAcceptance)
		cur := d.CurrentVersion
		_, err := s.Accept(ctx, org, d.Kind, cur+1, "user:client", "")
		assert.ErrorIs(t, err, domain.ErrNotCurrent)
		a, err := s.Accept(ctx, org, d.Kind, cur, "user:client", "203.0.113.9")
		require.NoError(t, err)
		assert.Equal(t, "203.0.113.9", a.ClientIP)
		again, err := s.Accept(ctx, org, d.Kind, cur, "user:client", "")
		require.NoError(t, err)
		assert.Equal(t, a.ID, again.ID, "idempotent")
	}
	require.NoError(t, gate.RequireAccepted(ctx, org, contract.KindTermsVPS, contract.KindAUP))

	// Nouveau brouillon (copie), modifié, publié : il faut accepter de nouveau.
	draft, err := s.CreateDraft(ctx, domain.KindTermsVPS, domain.Content{}, "user:staff")
	require.NoError(t, err)
	_, err = s.CreateDraft(ctx, domain.KindTermsVPS, domain.Content{}, "user:staff")
	assert.ErrorIs(t, err, domain.ErrDraftExists)
	c := domain.ContentOf(&draft.Document)
	c.BodyFR += "\n\n## 16. Ajout\n\nTexte ajouté."
	c.BodyEN += "\n\n## 16. Addition\n\nAdded text."
	_, err = s.UpdateDraft(ctx, domain.KindTermsVPS, draft.Version, c, "user:staff")
	require.NoError(t, err)
	require.NoError(t, gate.RequireAccepted(ctx, org, contract.KindTermsVPS), "brouillon : rien ne change")
	pub, err := s.Publish(ctx, domain.KindTermsVPS, draft.Version, "user:staff")
	require.NoError(t, err)
	assert.True(t, pub.Current)
	assert.Equal(t, "user:staff", pub.PublishedBy)
	assert.ErrorIs(t, gate.RequireAccepted(ctx, org, contract.KindTermsVPS), contract.ErrTermsNotAccepted)
	require.NoError(t, gate.RequireAccepted(ctx, org, contract.KindAUP))
	cur, err := s.Read(ctx, domain.KindTermsVPS, 0, domain.LangEN)
	require.NoError(t, err)
	assert.Equal(t, draft.Version, cur.Version)
	assert.Contains(t, cur.Body, "Added text.")

	// Publiée : immuable, par le service comme en SQL direct (déclencheur).
	_, err = s.UpdateDraft(ctx, domain.KindTermsVPS, draft.Version, c, "user:staff")
	assert.ErrorIs(t, err, domain.ErrNotDraft)
	_, err = s.Publish(ctx, domain.KindTermsVPS, draft.Version, "user:staff")
	assert.ErrorIs(t, err, domain.ErrNotDraft)
	assert.ErrorIs(t, s.DeleteDraft(ctx, domain.KindTermsVPS, draft.Version), domain.ErrNotDraft)
	assert.Error(t, db.Exec(`UPDATE legal_documents SET body_fr = 'changé' WHERE kind = ? AND version = ?`, domain.KindTermsVPS, draft.Version).Error)
	assert.Error(t, db.Exec(`DELETE FROM legal_documents WHERE kind = ? AND version = 1`, domain.KindTermsVPS).Error)

	_, err = s.Accept(ctx, org, domain.KindTermsVPS, draft.Version, "user:client", "")
	require.NoError(t, err)
	require.NoError(t, gate.RequireAccepted(ctx, org, contract.KindTermsVPS, contract.KindAUP))

	// Statistiques par version.
	versions, err := s.Versions(ctx, domain.KindTermsVPS)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(versions), 2)
	assert.Equal(t, draft.Version, versions[0].Version)
	assert.True(t, versions[0].Current)
	assert.GreaterOrEqual(t, versions[0].Acceptances, int64(1))
	docs, err = s.OrgStatus(ctx, org, domain.LangEN)
	require.NoError(t, err)
	for _, d := range docs {
		assert.False(t, d.NeedsAcceptance, d.Kind)
		require.NotNil(t, d.AcceptedVersion)
		assert.Equal(t, d.CurrentVersion, *d.AcceptedVersion)
	}

	// Un brouillon s'abandonne.
	d2, err := s.CreateDraft(ctx, domain.KindAUP, domain.Content{}, "user:staff")
	require.NoError(t, err)
	require.NoError(t, s.DeleteDraft(ctx, domain.KindAUP, d2.Version))
	_, err = s.Version(ctx, domain.KindAUP, d2.Version)
	assert.ErrorIs(t, err, domain.ErrDocumentNotFound)
}
