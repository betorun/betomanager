package repository_test

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

	"github.com/thekrauss/kubemanager/internal/modules/organizations/domain"
	"github.com/thekrauss/kubemanager/internal/modules/organizations/repository"
)

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	dsn := os.Getenv("IAM_E2E_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:pw@127.0.0.1:55452/postgres?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

// Le pays donné à l'inscription (profil local) passe à l'organisation
// personnelle à sa création ; il se lit et se change ensuite sur elle.
func TestE2E_PersonalOrgCountry(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	user := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO users (id, keycloak_user_id, email, full_name, country) VALUES (?, ?, ?, '', 'GA')`,
		user, user.String(), user.String()+"@e2e.test").Error)

	id, err := repository.EnsurePersonal(ctx, db, user, "")
	require.NoError(t, err)
	org, err := repository.Get(ctx, db, id)
	require.NoError(t, err)
	assert.Equal(t, "GA", org.Country)
	assert.Equal(t, domain.KindPersonal, org.Kind)

	require.NoError(t, repository.SetCountry(ctx, db, id, "CG"))
	c, err := repository.Country(ctx, db, id)
	require.NoError(t, err)
	assert.Equal(t, "CG", c)

	_, err = repository.Get(ctx, db, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, repository.SetCountry(ctx, db, uuid.New(), "GA"), domain.ErrNotFound)
	c, err = repository.Country(ctx, db, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, c, "inexistante : aucun pays")

	// Sans pays à l'inscription : "" (la console le demandera).
	other := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO users (id, keycloak_user_id, email, full_name) VALUES (?, ?, ?, '')`,
		other, other.String(), other.String()+"@e2e.test").Error)
	id, err = repository.EnsurePersonal(ctx, db, other, "")
	require.NoError(t, err)
	org, err = repository.Get(ctx, db, id)
	require.NoError(t, err)
	assert.Empty(t, org.Country)
}
