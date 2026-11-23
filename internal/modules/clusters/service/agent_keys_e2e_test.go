package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// Fichier de clé émis pour l'agent : celui que lit le watcher, révocable.
func TestE2E_ClusterAgentKeys(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" || os.Getenv("IAM_E2E_DSN") == "" {
		t.Skip("IAM_E2E / IAM_E2E_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(os.Getenv("IAM_E2E_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	engine := iam.NewEngine(&iam.GormStore{DB: db}, 0)
	sas, err := iam.NewServiceAccountService(db, engine, []byte("integration-test-pepper-0123456789abcdef"))
	require.NoError(t, err)
	keys := ClusterAgentKeys{Agents: iam.NewClusterAgents(sas)}
	cluster := "agent-" + uuid.NewString()[:8]

	id, raw, err := keys.Issue(ctx, "user:11111111-1111-1111-1111-111111111111", cluster)
	require.NoError(t, err)
	var file iam.KeyFile
	require.NoError(t, json.Unmarshal(raw, &file))
	assert.Equal(t, id, file.KeyID)
	assert.Equal(t, cluster, file.ClusterID)
	assert.NotEmpty(t, file.PrivateKey)

	require.NoError(t, keys.Revoke(ctx, id))
	list, err := keys.Agents.List(ctx)
	require.NoError(t, err)
	for _, k := range list {
		if k.KeyID == id {
			assert.Equal(t, "revoked", k.Status)
		}
	}
}
