package egressagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIP : interface simulée (adresses étiquetées ou non).
type fakeIP struct {
	route   string
	addrs   map[string]string // adresse → étiquette
	calls   []string
	failAdd string
}

func (f *fakeIP) run(_ context.Context, args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	switch {
	case cmd == "-4 route show default":
		return f.route, nil
	case strings.HasPrefix(cmd, "-4 -o addr show dev eth0 label "):
		lbl := args[len(args)-1]
		var keys []string
		for a := range f.addrs {
			keys = append(keys, a)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, a := range keys {
			if f.addrs[a] == lbl {
				fmt.Fprintf(&b, "2: eth0    inet %s/32 scope global %s\\       valid_lft forever preferred_lft forever\n", a, lbl)
			}
		}
		return b.String(), nil
	case strings.HasPrefix(cmd, "-4 addr add "):
		a := strings.TrimSuffix(args[3], "/32")
		if a == f.failAdd {
			return "", errors.New("RTNETLINK answers: Operation not permitted")
		}
		f.addrs[a] = args[len(args)-1]
		return "", nil
	case strings.HasPrefix(cmd, "-4 addr del "):
		delete(f.addrs, strings.TrimSuffix(args[3], "/32"))
		return "", nil
	}
	return "", fmt.Errorf("unexpected: %s", cmd)
}

func TestDefaultInterface(t *testing.T) {
	f := &fakeIP{route: "default via 51.75.141.1 dev eth0 proto dhcp src 51.75.141.233 metric 100\n"}
	iface, err := DefaultInterface(context.Background(), f.run)
	require.NoError(t, err)
	assert.Equal(t, "eth0", iface)
	f.route = ""
	_, err = DefaultInterface(context.Background(), f.run)
	assert.ErrorIs(t, err, ErrNoDefaultRoute)
}

// N'ajoute que ce qui manque, ne retire que ses propres adresses (jamais
// l'adresse principale du nœud, non étiquetée).
func TestReconcile(t *testing.T) {
	ctx := context.Background()
	f := &fakeIP{addrs: map[string]string{"51.75.141.233": "eth0", "203.0.113.9": "eth0:bt"}}
	have, err := Reconcile(ctx, f.run, "eth0", []string{"203.0.113.7", " 203.0.113.7", "nope", "2001:db8::1", "203.0.113.8"})
	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.7", "203.0.113.8"}, have)
	assert.Equal(t, "eth0", f.addrs["51.75.141.233"], "adresse principale intacte")
	_, gone := f.addrs["203.0.113.9"]
	assert.False(t, gone, "adresse étiquetée plus voulue : retirée")

	f.calls = nil
	have, err = Reconcile(ctx, f.run, "eth0", []string{"203.0.113.7", "203.0.113.8"})
	require.NoError(t, err)
	assert.Len(t, have, 2)
	for _, c := range f.calls {
		assert.NotContains(t, c, "addr add", "rien à faire : aucune modification")
		assert.NotContains(t, c, "addr del")
	}

	f.failAdd = "203.0.113.10"
	have, err = Reconcile(ctx, f.run, "eth0", []string{"203.0.113.7", "203.0.113.10"})
	assert.Error(t, err)
	assert.Equal(t, []string{"203.0.113.7"}, have, "l'échec d'une adresse n'arrête pas les autres ; statut vrai")
}

func TestLabelTooLong(t *testing.T) {
	_, err := Reconcile(context.Background(), (&fakeIP{}).run, "enp129s0f1np1", nil)
	assert.ErrorIs(t, err, ErrInterfaceName)
}

func TestParseDesiredAndStatus(t *testing.T) {
	d, err := ParseDesired(`{"gw-1":["203.0.113.7"]}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.7"}, d["gw-1"])
	d, err = ParseDesired("")
	require.NoError(t, err)
	assert.Empty(t, d)
	_, err = ParseDesired("{oops")
	assert.Error(t, err)

	now := time.Now()
	s := Status{Addresses: []string{"203.0.113.7"}, At: now.Add(-time.Minute)}
	assert.Equal(t, []string{"203.0.113.7"}, s.Mounted(now))
	s.At = now.Add(-5 * time.Minute)
	assert.Nil(t, s.Mounted(now), "agent silencieux : rien n'est tenu pour monté")
}
