package ovhvps

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetReverse(t *testing.T) {
	p, rec := fakeOrder(t, map[string]string{
		"POST /ip/51.68.1.2/32/reverse":             `{}`,
		"DELETE /ip/51.68.1.2/32/reverse/51.68.1.2": ``,
		"POST /ip/2001:41d0:1:2::1/128/reverse":     `{}`,
	})
	ctx := context.Background()
	require.NoError(t, p.SetReverse(ctx, "51.68.1.2", "mail.exemple.cg"))
	assert.Equal(t, map[string]any{"ipReverse": "51.68.1.2", "reverse": "mail.exemple.cg."}, rec.get("POST /ip/51.68.1.2/32/reverse")[0], "point final ajouté")
	require.NoError(t, p.SetReverse(ctx, "51.68.1.2", ""))
	assert.Len(t, rec.get("DELETE /ip/51.68.1.2/32/reverse/51.68.1.2"), 1, "vide : retiré")
	require.NoError(t, p.SetReverse(ctx, "2001:41d0:1:2::1", "v6.exemple.cg."))
	assert.Equal(t, "v6.exemple.cg.", rec.get("POST /ip/2001:41d0:1:2::1/128/reverse")[0]["reverse"])
}

func TestSpamAndRenewal(t *testing.T) {
	p, rec := fakeOrder(t, map[string]string{
		"GET /ip/51.68.1.2/32/spam/51.68.1.2":          `{"state":"blockedForSpam","date":"2026-11-20T08:00:00Z","time":3600}`,
		"POST /ip/51.68.1.2/32/spam/51.68.1.2/unblock": `{"state":"unblocking"}`,
		"GET /vps/vps-a/serviceInfos":                  `{"renew":{"automatic":true,"deleteAtExpiration":false,"forced":false,"period":1}}`,
		"PUT /vps/vps-a/serviceInfos":                  ``,
	})
	ctx := context.Background()
	blocked, since, err := p.SpamBlocked(ctx, "51.68.1.2")
	require.NoError(t, err)
	assert.True(t, blocked)
	assert.Equal(t, 20, since.Day())
	blocked, _, err = p.SpamBlocked(ctx, "51.68.9.9")
	require.NoError(t, err)
	assert.False(t, blocked, "aucun blocage connu (404)")
	require.NoError(t, p.UnblockSpam(ctx, "51.68.1.2"))

	require.NoError(t, p.StopRenewal(ctx, "vps-a"))
	renew := rec.get("PUT /vps/vps-a/serviceInfos")[0]["renew"].(map[string]any)
	assert.Equal(t, false, renew["automatic"])
	assert.Equal(t, true, renew["deleteAtExpiration"])
	assert.Equal(t, float64(1), renew["period"], "le reste du réglage est gardé")
	require.NoError(t, p.StopRenewal(ctx, "vps-gone"), "déjà supprimé : rien à faire")
}

func TestRebuildPostInstallScript(t *testing.T) {
	p, rec := fakeOrder(t, map[string]string{
		"GET /vps/vps-a/images/available":    `["i2"]`,
		"GET /vps/vps-a/images/available/i2": `{"id":"i2","name":"Debian 12"}`,
		"POST /vps/vps-a/rebuild":            `{"id":9}`,
	})
	_, err := p.Rebuild(context.Background(), "vps-a", "Debian 12", "ssh-ed25519 AAAA", "#!/bin/sh\necho ok\n")
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho ok\n", rec.get("POST /vps/vps-a/rebuild")[0]["postInstallScript"])
}
