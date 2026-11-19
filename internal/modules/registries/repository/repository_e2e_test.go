package repository

// Sur un vrai Postgres (migrations complètes). Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/registries/repository/ -count=1 -v

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
)

const e2eDSN = "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"

func e2eStore(t *testing.T) (Store, *gorm.DB, uuid.UUID) {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sealer, err := auth.NewSealer(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	require.NoError(t, err)
	project := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id)
		SELECT ?, ?, ?, organization_id FROM projects LIMIT 1`, project, "e2e-"+project.String(), "km-"+project.String()[:8]).Error)
	return New(db, sealer), db, project
}

func TestE2E_PutListAuthsDelete(t *testing.T) {
	store, db, project := e2eStore(t)
	ctx := context.Background()

	_, err := store.Put(ctx, project, "ghcr.io", "moi", "ghp_premier", nil)
	require.NoError(t, err)
	v, err := store.Put(ctx, project, "ghcr.io", "moi", "ghp_second", nil)
	require.NoError(t, err, "remplacement")
	assert.False(t, v.UpdatedAt.IsZero())
	_, err = store.Put(ctx, project, "docker.io", "moi", "dckr", nil)
	require.NoError(t, err)

	views, err := store.List(ctx, project)
	require.NoError(t, err)
	require.Len(t, views, 2)
	assert.Equal(t, "docker.io", views[0].Host)

	// Le jeton n'est jamais stocké en clair.
	var raw []byte
	require.NoError(t, db.Raw(`SELECT sealed_password FROM project_registry_credentials WHERE project_id = ? AND host = 'ghcr.io'`, project).Row().Scan(&raw))
	assert.NotContains(t, string(raw), "ghp_second")

	auths, err := store.Auths(ctx, project, "ghcr.io")
	require.NoError(t, err)
	require.Len(t, auths, 1)
	assert.Equal(t, "ghp_second", auths[0].Password)

	has, err := store.Has(ctx, project, "ghcr.io")
	require.NoError(t, err)
	assert.True(t, has)
	has, err = store.Has(ctx, uuid.New(), "ghcr.io")
	require.NoError(t, err)
	assert.False(t, has, "un autre projet n'en profite pas")

	require.NoError(t, store.Delete(ctx, project, "ghcr.io"))
	assert.ErrorIs(t, store.Delete(ctx, project, "ghcr.io"), domain.ErrNotFound)
}

// Un chiffré recopié sur la ligne d'un autre hôte ne s'ouvre pas.
func TestE2E_SealedValueBoundToRow(t *testing.T) {
	store, db, project := e2eStore(t)
	ctx := context.Background()
	_, err := store.Put(ctx, project, "ghcr.io", "moi", "ghp_x", nil)
	require.NoError(t, err)
	_, err = store.Put(ctx, project, "quay.io", "moi", "q", nil)
	require.NoError(t, err)
	require.NoError(t, db.Exec(`UPDATE project_registry_credentials SET sealed_password =
		(SELECT sealed_password FROM project_registry_credentials WHERE project_id = ? AND host = 'ghcr.io')
		WHERE project_id = ? AND host = 'quay.io'`, project, project).Error)
	_, err = store.Auths(ctx, project, "quay.io")
	assert.Error(t, err)
}
