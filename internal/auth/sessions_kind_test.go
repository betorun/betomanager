package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLifetimesByKind(t *testing.T) {
	idle, abs := lifetimes(KindMobile)
	assert.Equal(t, MobileIdleTTL, idle)
	assert.Equal(t, MobileSessionTTL, abs)
	assert.Less(t, MobileSessionTTL, CLISessionTTL, "un téléphone se perd : session plus courte que la CLI")

	idle, abs = lifetimes(KindCLI)
	assert.Equal(t, CLIIdleTTL, idle)
	assert.Equal(t, CLISessionTTL, abs)

	idle, abs = lifetimes("autre")
	assert.Equal(t, WebIdleTTL, idle)
	assert.Equal(t, WebSessionTTL, abs)
}

func TestPasskeyOrigins(t *testing.T) {
	got := passkeyOrigins("https://admin.bisocloud.com", []string{" android:apk-key-hash:abc ", "", "https://admin.bisocloud.com"})
	assert.Equal(t, []string{"https://admin.bisocloud.com", "android:apk-key-hash:abc"}, got)

	wa, err := NewWebAuthn("https://admin.bisocloud.com", "android:apk-key-hash:abc")
	assert.NoError(t, err)
	assert.Equal(t, "admin.bisocloud.com", wa.Config.RPID)
}
