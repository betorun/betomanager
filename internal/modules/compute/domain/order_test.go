package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Clés jetables, générées pour ces tests.
const (
	testEd25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA74/lOopHYYorBomTQtkD2FGCfu4OIN/VlNbSYUgYqW test@exemple"
	testRSA1024 = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDmaQp9/hc5zX9xDrPdkFjZr41qX66kZkORuGsHMnxyJiiqHynarNGUInovQtnaApBu1/EeJ2Zro21tqjxt6lYV8SgLlodjrFVxOP/HyMKaPwzWEkNgcx76RpAcVE5w5JoovCOinNglMtehszu9Nr4y70VCiRS5FikHbExMrxopFw=="
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"web", "web-1", "a", "0db"} {
		assert.True(t, ValidName(ok), ok)
	}
	for _, bad := range []string{"", "-web", "web-", "Web", "web_1", "web.1", "é", string(make([]byte, 64))} {
		assert.False(t, ValidName(bad), bad)
	}
}

func TestParseSSHKey(t *testing.T) {
	k, err := ParseSSHKey("  " + testEd25519 + "\n")
	require.NoError(t, err)
	assert.Equal(t, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA74/lOopHYYorBomTQtkD2FGCfu4OIN/VlNbSYUgYqW", k.Line, "commentaire retiré")
	assert.Contains(t, k.Fingerprint, "SHA256:")

	for _, bad := range []string{"", "ssh-ed25519 pas-du-base64", testRSA1024, testEd25519 + "\n" + testEd25519, "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		_, err := ParseSSHKey(bad)
		assert.ErrorIs(t, err, ErrInvalidSSHKey, bad)
	}
}

func TestPeriods(t *testing.T) {
	id := uuid.MustParse("6f1c2c7e-0000-4000-8000-000000000001")
	from := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	assert.Equal(t, "compute:vps:6f1c2c7e-0000-4000-8000-000000000001:2026-01-31", Period(id, from))
	assert.Equal(t, time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC), NextPeriod(from), "borné au dernier jour de février")
	assert.Equal(t, time.Date(2026, 12, 15, 10, 0, 0, 0, time.UTC), NextPeriod(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)))
	assert.Equal(t, time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), NextPeriod(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)), "passage d'année")
}

func TestSellsNeedsProviderImage(t *testing.T) {
	o := Offer{Locations: []byte(`["gra"]`), Images: []byte(`{"ubuntu-24.04":"Ubuntu 24.04","debian-12":""}`),
		Public: true, ProviderPlanCode: "vps-2025-model1", ProviderPriceCents: 649, CoefficientBP: 20000, RoundTo: 500}
	assert.True(t, o.Sells("gra", "ubuntu-24.04"))
	assert.False(t, o.Sells("gra", "debian-12"), "image du fournisseur pas réglée")
	assert.False(t, o.Sells("sbg", "ubuntu-24.04"))
	assert.True(t, o.Sellable(655957))
	o.Public = false
	assert.False(t, o.Sellable(655957))
}
