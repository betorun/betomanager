package domain

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextRange(t *testing.T) {
	p, err := NextRange(nil)
	require.NoError(t, err)
	assert.Equal(t, "10.248.0.0/24", p.String())

	p, err = NextRange([]netip.Prefix{netip.MustParsePrefix("10.248.0.0/24"), netip.MustParsePrefix("10.248.2.0/24")})
	require.NoError(t, err)
	assert.Equal(t, "10.248.1.0/24", p.String(), "premier trou")

	// Passage d'octet : 10.248.255.0 → 10.249.0.0.
	var used []netip.Prefix
	for i := range 256 {
		used = append(used, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 248, byte(i), 0}), 24))
	}
	p, err = NextRange(used)
	require.NoError(t, err)
	assert.Equal(t, "10.249.0.0/24", p.String())

	// Plage pleine : 2 048 /24.
	used = used[:0]
	for b1 := range 8 {
		for b2 := range 256 {
			used = append(used, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(248 + b1), byte(b2), 0}), 24))
		}
	}
	_, err = NextRange(used)
	assert.ErrorIs(t, err, ErrRangeExhausted)
	assert.False(t, Pool.Overlaps(netip.MustParsePrefix("10.42.0.0/16")), "pods")
	assert.False(t, Pool.Overlaps(netip.MustParsePrefix("10.43.0.0/16")), "services")
}

func TestNextAddress(t *testing.T) {
	p := netip.MustParsePrefix("10.248.7.0/24")
	assert.Equal(t, "10.248.7.1", GatewayOf(p).String())
	a, err := NextAddress(p, nil)
	require.NoError(t, err)
	assert.Equal(t, "10.248.7.2", a.String(), "ni .0 ni .1")

	a, err = NextAddress(p, []netip.Addr{netip.MustParseAddr("10.248.7.2"), netip.MustParseAddr("10.248.7.4")})
	require.NoError(t, err)
	assert.Equal(t, "10.248.7.3", a.String())

	var used []netip.Addr
	for i := 2; i <= 254; i++ {
		used = append(used, netip.AddrFrom4([4]byte{10, 248, 7, byte(i)}))
	}
	_, err = NextAddress(p, used)
	assert.ErrorIs(t, err, ErrRangeExhausted, ".255 jamais donnée")
}

func TestValidPublicKey(t *testing.T) {
	assert.True(t, ValidPublicKey("xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="))
	assert.False(t, ValidPublicKey("court"))
	assert.False(t, ValidPublicKey("clé avec espace pour voir si ça passe"))
	assert.False(t, ValidPublicKey("abc\ndefghijklmnopqrstuvwxyz"))
	assert.Equal(t, "web-1.internal", InternalName("Web-1"))
}
