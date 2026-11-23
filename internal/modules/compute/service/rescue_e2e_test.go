package service_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/repository"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

func TestE2E_ComputeRescue(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	sealer, err := auth.NewSealer(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	now := time.Date(2033, 1, 5, 10, 0, 0, 0, time.UTC)
	store := &repository.GormStore{DB: db}
	s := &service.Service{Store: store, Sealer: sealer, Now: func() time.Time { return now }}

	org, project := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e vps rescue')`, org).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "res-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	paid := now.AddDate(0, 0, 10)
	v := &domain.VPS{ID: uuid.New(), OrganizationID: org, ProjectID: project, Name: "web", Offer: "VPS-1", Location: "gra",
		Image: "debian-12", Provider: domain.ProviderOVH, ProviderID: "vps-rs-" + uuid.NewString()[:8], State: domain.StateRunning,
		IPv4: "51.68.1.2", PaidUntil: &paid, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(v).Error)

	_, err = s.RevealRescue(ctx, org, v.ID)
	assert.ErrorIs(t, err, domain.ErrNotInRescue)
	got, err := s.EnterRescue(ctx, org, v.ID, "user:ed")
	require.NoError(t, err)
	assert.Equal(t, domain.OpEnteringRescue, got.Operation)

	// Le mode compute a capté l'e-mail : secret chiffré, lié à ce VPS.
	var r domain.Request
	require.NoError(t, db.Where("vps_id = ? AND kind = ?", v.ID, domain.KindRescueEnter).First(&r).Error)
	require.NoError(t, store.FinishRequest(ctx, r.ID, true, "", now))
	require.NoError(t, store.UpdateVPS(ctx, v.ID, map[string]any{"rescue": domain.RescueActive, "rescue_secret": domain.RescueSecretReady}))
	plain, _ := json.Marshal(domain.RescueCredentials{Username: "root", Password: "Xk7pQ2vLm9"})
	sealed, err := sealer.Seal(plain, service.RescueAAD(v.ID))
	require.NoError(t, err)
	require.NoError(t, store.SaveSecret(ctx, &domain.Secret{VPSID: v.ID, Kind: domain.SecretRescuePassword, Ciphertext: sealed,
		CreatedAt: now, ExpiresAt: now.Add(domain.RescueSecretTTL)}))

	// En secours, les autres actions sont refusées.
	_, err = s.Act(ctx, org, v.ID, service.ActionReboot, nil, "user:dev")
	assert.ErrorIs(t, err, domain.ErrInRescue)

	// Deux lectures simultanées : une seule obtient le mot de passe.
	var wg sync.WaitGroup
	results := make([]*service.RescueReveal, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = s.RevealRescue(ctx, org, v.ID) }(i)
	}
	wg.Wait()
	shown := 0
	for i, res := range results {
		if res != nil {
			shown++
			assert.Equal(t, "Xk7pQ2vLm9", res.Password)
			assert.Equal(t, "51.68.1.2", res.Host)
		} else {
			assert.ErrorIs(t, errs[i], domain.ErrRescueSecretUnavailable)
		}
	}
	assert.Equal(t, 1, shown, "affiché une seule fois")
	got, _ = store.VPS(ctx, v.ID)
	assert.Equal(t, domain.RescueSecretRead, got.RescueSecret)

	// Expiré : plus lisible.
	require.NoError(t, store.SaveSecret(ctx, &domain.Secret{VPSID: v.ID, Kind: domain.SecretRescuePassword, Ciphertext: sealed,
		CreatedAt: now, ExpiresAt: now.Add(-time.Minute)}))
	_, err = s.RevealRescue(ctx, org, v.ID)
	assert.ErrorIs(t, err, domain.ErrRescueSecretUnavailable)

	// Sortie.
	got, err = s.ExitRescue(ctx, org, v.ID, "user:ed")
	require.NoError(t, err)
	assert.Equal(t, domain.OpExitingRescue, got.Operation)
}
