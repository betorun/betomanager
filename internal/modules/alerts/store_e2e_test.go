package alerts

// Sur un vrai Postgres (migrations complètes). Ignoré sans ALERTS_E2E_DSN :
//
//	ALERTS_E2E_DSN=postgres://… go test ./internal/modules/alerts/ -run E2E -count=1 -v

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func e2eDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("ALERTS_E2E_DSN")
	if dsn == "" {
		t.Skip("ALERTS_E2E_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

func TestE2E_ClaimThrottles(t *testing.T) {
	db := e2eDB(t)
	s := NewStore(db)
	ctx := context.Background()
	subject := uuid.New()
	t.Cleanup(func() { db.Exec(`DELETE FROM alert_deliveries WHERE subject_id = ?`, subject) })

	ok, err := s.Claim(ctx, subject, KindWorkloadIncident, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "premier envoi")
	ok, err = s.Claim(ctx, subject, KindWorkloadIncident, time.Hour)
	require.NoError(t, err)
	assert.False(t, ok, "dans la période")
	ok, err = s.Claim(ctx, subject, KindBuildFailed, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "autre type")

	require.NoError(t, db.Exec(`UPDATE alert_deliveries SET sent_at = now() - interval '2 hours' WHERE subject_id = ?`, subject).Error)
	ok, err = s.Claim(ctx, subject, KindWorkloadIncident, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "période écoulée")
}

// Les owners d'un projet existant : lus via iam_bindings.
func TestE2E_RecipientsAndProjectName(t *testing.T) {
	db := e2eDB(t)
	var row struct {
		ID   uuid.UUID
		Name string
	}
	require.NoError(t, db.Raw(`SELECT p.id, p.name FROM projects p
		JOIN iam_bindings b ON b.resource_name = 'projects/' || p.id::text AND b.role = 'roles/owner' LIMIT 1`).Scan(&row).Error)
	if row.ID == uuid.Nil {
		t.Skip("aucun projet avec owner dans cette base")
	}
	s := NewStore(db)
	owners, err := s.Recipients(context.Background(), row.ID)
	require.NoError(t, err)
	require.NotEmpty(t, owners)
	assert.NotEmpty(t, owners[0].Email)
	name, err := s.ProjectName(context.Background(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, row.Name, name)
}
