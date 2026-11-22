package ovhvps

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

func TestBackups(t *testing.T) {
	p, _ := fakeOrder(t, map[string]string{
		"GET /vps/vps-a/option": `["automatedBackup","snapshot"]`,
		"GET /vps/vps-a/automatedBackup/restorePoints?state=available": `["2026-11-20T02:00:00Z","2026-11-22T02:00:00Z","2026-11-21T02:00:00Z"]`,
		"GET /vps/vps-a/automatedBackup/attachedBackup":                `[{"restorePoint":"2026-11-21T02:00:00Z","access":{"additionalDisk":"/dev/sdb","nfs":"10.0.0.1:/export/x","smb":"\\\\host\\x"}}]`,
		"GET /vps/vps-a/snapshot":                                      `{"creationDate":"2026-11-19T10:00:00Z","description":"avant mise à jour","id":"s1","region":"GRA"}`,
		"GET /vps/vps-b/option":                                        `[]`,
	})
	b, err := p.Backups(context.Background(), "vps-a")
	require.NoError(t, err)
	assert.True(t, b.Automated)
	assert.True(t, b.SnapshotOption)
	require.Len(t, b.RestorePoints, 3)
	assert.Equal(t, 22, b.RestorePoints[0].Day(), "plus récent d'abord")
	require.Len(t, b.Mounted, 1)
	assert.Equal(t, "/dev/sdb", b.Mounted[0].Disk, "seul le disque est gardé (pas les accès réseau)")
	assert.Equal(t, "avant mise à jour", b.Snapshot.Description)

	b, err = p.Backups(context.Background(), "vps-b")
	require.NoError(t, err)
	assert.False(t, b.Automated)
	assert.Nil(t, b.Snapshot)
}

func TestRestoreAndSnapshotCalls(t *testing.T) {
	p, rec := fakeOrder(t, map[string]string{
		"POST /vps/vps-a/automatedBackup/restore":      `{"id":70}`,
		"POST /vps/vps-a/automatedBackup/detachBackup": `{"id":71}`,
		"POST /vps/vps-a/createSnapshot":               `{"id":72}`,
		"POST /vps/vps-a/snapshot/revert":              `{"id":73}`,
		"DELETE /vps/vps-a/snapshot":                   `{"id":74}`,
	})
	point := time.Date(2026, 11, 22, 2, 0, 0, 0, time.UTC)
	ctx := context.Background()
	task, err := p.Restore(ctx, "vps-a", point, false)
	require.NoError(t, err)
	assert.Equal(t, "70", task)
	assert.Equal(t, map[string]any{"restorePoint": "2026-11-22T02:00:00Z", "type": "file", "changePassword": false},
		rec.get("POST /vps/vps-a/automatedBackup/restore")[0], "montage, jamais de nouveau mot de passe")
	_, _ = p.Restore(ctx, "vps-a", point, true)
	assert.Equal(t, "full", rec.get("POST /vps/vps-a/automatedBackup/restore")[1]["type"])
	for _, f := range []func() (string, error){
		func() (string, error) { return p.Unmount(ctx, "vps-a", point) },
		func() (string, error) { return p.CreateSnapshot(ctx, "vps-a", "x") },
		func() (string, error) { return p.RevertSnapshot(ctx, "vps-a") },
		func() (string, error) { return p.DeleteSnapshot(ctx, "vps-a") },
	} {
		task, err := f()
		require.NoError(t, err)
		assert.NotEmpty(t, task)
	}
}

func TestAbortStuck(t *testing.T) {
	routes := map[string]string{
		"GET /vps/vps-a/tasks?state=doing": `[1,2]`,
		"GET /vps/vps-a/tasks/1":           `{"type":"rebootVm","date":"2026-11-22T01:00:00Z"}`,
		"GET /vps/vps-a/tasks/2":           `{"type":"createSnapshot","date":"2026-11-22T01:00:00Z"}`,
		"POST /vps/vps-a/abortSnapshot":    ``,
	}
	p, rec := fakeOrder(t, routes)
	p.api.Now = func() time.Time { return time.Date(2026, 11, 22, 4, 0, 0, 0, time.UTC) }
	aborted, err := p.AbortStuck(context.Background(), "vps-a", 6*time.Hour)
	require.NoError(t, err)
	assert.False(t, aborted, "3 h : pas encore bloqué")
	p.api.Now = func() time.Time { return time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC) }
	aborted, err = p.AbortStuck(context.Background(), "vps-a", 6*time.Hour)
	require.NoError(t, err)
	assert.True(t, aborted)
	assert.Len(t, rec.get("POST /vps/vps-a/abortSnapshot"), 1)
}

func TestOrderSnapshotOption(t *testing.T) {
	routes := map[string]string{
		"POST /order/cart":                        `{"cartId":"c9"}`,
		"POST /order/cartServiceOption/vps/vps-a": `{"itemId":3}`,
		"GET /order/cart/c9/checkout":             `{"prices":{"withTax":{"value":1.20}}}`,
		"POST /order/cart/c9/checkout":            `{"orderId":555}`,
	}
	p, rec := fakeOrder(t, routes)
	id, err := p.OrderSnapshotOption(context.Background(), "vps-a", "option-snapshot", 120)
	require.NoError(t, err)
	assert.Equal(t, "555", id)
	assert.Equal(t, "option-snapshot", rec.get("POST /order/cartServiceOption/vps/vps-a")[0]["planCode"])

	p, rec = fakeOrder(t, routes)
	_, err = p.OrderSnapshotOption(context.Background(), "vps-a", "option-snapshot", 100)
	assert.ErrorIs(t, err, service.ErrProviderPriceAbove)
	assert.Empty(t, rec.get("POST /order/cart/c9/checkout"), "rien payé")
}

func TestReleaseSnapshotOption(t *testing.T) {
	// API des services : option trouvée par son plan, résiliée.
	p, rec := fakeOrder(t, map[string]string{
		"GET /services?resourceName=vps-a": `[100]`,
		"GET /services/100/options":        `[{"serviceId":200,"billing":{"plan":{"code":"option-backup"}}},{"serviceId":201,"billing":{"plan":{"code":"option-snapshot"}}}]`,
		"DELETE /services/201":             `{"message":"ok"}`,
	})
	require.NoError(t, p.ReleaseSnapshotOption(context.Background(), "vps-a", "option-snapshot"))
	assert.Len(t, rec.get("DELETE /services/201"), 1)
	assert.Empty(t, rec.get("DELETE /services/200"), "seulement l'option snapshot")
	assert.Empty(t, rec.get("DELETE /vps/vps-a/option/snapshot"))

	// Introuvable par l'API des services : repli sur l'ancienne route.
	p, rec = fakeOrder(t, map[string]string{
		"GET /services?resourceName=vps-a":  `[100]`,
		"GET /services/100/options":         `[]`,
		"DELETE /vps/vps-a/option/snapshot": ``,
	})
	require.NoError(t, p.ReleaseSnapshotOption(context.Background(), "vps-a", "option-snapshot"))
	assert.Len(t, rec.get("DELETE /vps/vps-a/option/snapshot"), 1)
}
