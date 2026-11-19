package auth

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
)

// AUTH_E2E_DSN : base de développement migrée, avec au moins un utilisateur.
func TestE2E_DevicesSeen(t *testing.T) {
	dsn := os.Getenv("AUTH_E2E_DSN")
	if dsn == "" {
		t.Skip("AUTH_E2E_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	var user uuid.UUID
	require.NoError(t, db.Raw(`SELECT id FROM users WHERE id NOT IN (SELECT user_id FROM user_devices) LIMIT 1`).Row().Scan(&user))
	t.Cleanup(func() { db.Exec(`DELETE FROM user_devices WHERE user_id = ?`, user) })

	d := &Devices{DB: db}
	ctx := context.Background()
	chrome := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0 Safari/537.36"

	s, err := d.Seen(ctx, user, KindWeb, chrome)
	require.NoError(t, err)
	assert.True(t, s.New && s.First, "première connexion : pas d'e-mail")
	s, err = d.Seen(ctx, user, KindWeb, chrome)
	require.NoError(t, err)
	assert.False(t, s.New, "appareil connu")
	s, err = d.Seen(ctx, user, KindCLI, "betorun-cli (linux)")
	require.NoError(t, err)
	assert.True(t, s.New && !s.First, "nouvel appareil : e-mail")
	assert.Equal(t, "CLI betorun · Linux", s.Label)
}
