package repository_test

// Adresses mises de côté sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/mail/... -count=1

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

	"github.com/thekrauss/kubemanager/internal/modules/mail/domain"
	"github.com/thekrauss/kubemanager/internal/modules/mail/repository"
)

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

func TestE2E_Suppressions(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	r := &repository.GormRepository{DB: db}
	email := uuid.NewString()[:8] + "@Rebond.example"

	require.NoError(t, r.Suppress(ctx, domain.NewSuppression(email, domain.ReasonBounce, domain.SourceSendGrid, "550 user unknown")))
	require.NoError(t, r.Suppress(ctx, domain.NewSuppression(email, domain.ReasonComplaint, domain.SourceSendGrid, "")), "idempotent")
	ok, err := r.IsSuppressed(ctx, " "+email)
	require.NoError(t, err)
	assert.True(t, ok, "comparaison sans casse ni espaces")

	list, err := r.List(ctx, email[:8], 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, domain.ReasonBounce, list[0].Reason, "la première raison est gardée")
	none, err := r.List(ctx, "%", 10)
	require.NoError(t, err)
	for _, s := range none {
		assert.Contains(t, s.Email, "%", "le % de la recherche est un caractère, pas un joker")
	}

	removed, err := r.Remove(ctx, email)
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = r.Remove(ctx, email)
	require.NoError(t, err)
	assert.False(t, removed)
}
