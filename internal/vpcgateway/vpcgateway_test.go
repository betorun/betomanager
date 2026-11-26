package vpcgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const vpsKey = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="

func sample() *Config {
	return &Config{CIDR: "10.248.3.0/24", Gateway: "10.248.3.1", Routes: []string{"10.42.0.0/16", "10.43.0.0/16"},
		Peers: []Peer{
			{VPSID: "b", Name: "db", Address: "10.248.3.3", PublicKey: vpsKey},
			{VPSID: "a", Name: "web", Address: "10.248.3.2"}, // pas encore inscrit
		}}
}

func TestKeys(t *testing.T) {
	priv, pub, err := GenerateKey()
	require.NoError(t, err)
	assert.True(t, ValidKey(priv))
	assert.True(t, ValidKey(pub))
	again, err := PublicKey(priv)
	require.NoError(t, err)
	assert.Equal(t, pub, again)
	assert.True(t, ValidKey(vpsKey))
	for _, k := range []string{"", "pas une clé", vpsKey[:43], strings.Repeat("A", 44)} {
		assert.False(t, ValidKey(k), k)
	}
}

func TestValidate(t *testing.T) {
	require.NoError(t, sample().Validate())
	bad := []func(c *Config){
		func(c *Config) { c.CIDR = "10.248.3.1/24" },
		func(c *Config) { c.CIDR = "fd00::/64" },
		func(c *Config) { c.Gateway = "10.248.4.1" },
		func(c *Config) { c.Routes = []string{"10.42.0.1/16"} },
		func(c *Config) { c.Peers[0].Address = "10.248.3.1" },
		func(c *Config) { c.Peers[0].Address = "10.9.9.9" },
		func(c *Config) { c.Peers[0].PublicKey = "x; rm -rf /" },
	}
	for i, f := range bad {
		c := sample()
		f(c)
		assert.Error(t, c.Validate(), "cas %d", i)
	}
}

func TestRender(t *testing.T) {
	out := Render(sample(), "PRIV")
	assert.Contains(t, out, "[Interface]\nPrivateKey = PRIV\nListenPort = 51820\n")
	assert.Equal(t, 1, strings.Count(out, "[Peer]"), "seuls les VPS inscrits sont des pairs")
	assert.Contains(t, out, "PublicKey = "+vpsKey+"\nAllowedIPs = 10.248.3.3/32\n")
}

func TestParseDumpNeverReturnsPrivateKey(t *testing.T) {
	dump := "PRIVATEKEY\tPUB\t51820\toff\n" +
		vpsKey + "\t(none)\t203.0.113.7:41000\t10.248.3.3/32\t1764150000\t1200\t3400\t25\n"
	got := ParseDump(dump)
	require.Len(t, got, 1)
	assert.Equal(t, PeerStatus{PublicKey: vpsKey, LatestHandshake: 1764150000, RxBytes: 1200, TxBytes: 3400}, got[0])
	raw, _ := json.Marshal(got)
	assert.NotContains(t, string(raw), "PRIVATEKEY")
	assert.Empty(t, ParseDump(""))
}

type recorder struct {
	calls []string
	fail  map[string]error
}

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	for k, err := range r.fail {
		if strings.Contains(line, k) {
			return err
		}
	}
	return nil
}

func writeFiles(t *testing.T, c *Config, priv string) (cfg, key, rt string) {
	t.Helper()
	cfg, key, rt = t.TempDir(), t.TempDir(), t.TempDir()
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cfg, ConfigKey), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(key, PrivateKeyKey), []byte(priv+"\n"), 0o600))
	return
}

func TestSync(t *testing.T) {
	priv, _, err := GenerateKey()
	require.NoError(t, err)
	cfg, key, rt := writeFiles(t, sample(), priv)
	// Règles absentes (-C échoue) : ajoutées une fois.
	r := &recorder{fail: map[string]error{" -C ": errors.New("absent")}}
	a := &Agent{Run: r.run, ConfigDir: cfg, KeyDir: key, RuntimeDir: rt}
	require.NoError(t, a.Sync(context.Background()))
	joined := strings.Join(r.calls, "\n")
	assert.Contains(t, joined, "ip link add vpc0 type wireguard")
	assert.Contains(t, joined, "ip address replace 10.248.3.1/24 dev vpc0")
	assert.Contains(t, joined, "iptables -w -t nat -A POSTROUTING -s 10.248.3.0/24 ! -o vpc0 -j MASQUERADE")
	assert.Contains(t, joined, "iptables -w -A FORWARD -i vpc0 -d 10.43.0.0/16 -j ACCEPT")
	assert.Contains(t, joined, "iptables -w -A FORWARD -i vpc0 -j DROP")
	assert.Contains(t, joined, "wg syncconf vpc0 "+filepath.Join(rt, "vpc0.conf"))
	_, err = os.Stat(filepath.Join(rt, "vpc0.conf"))
	assert.True(t, os.IsNotExist(err), "la configuration (clé privée) ne reste pas sur le disque")

	// Rien de changé : rien d'appliqué.
	n := len(r.calls)
	require.NoError(t, a.Sync(context.Background()))
	assert.Len(t, r.calls, n)

	// Règles déjà là (conteneur relancé) : pas de doublon.
	r2 := &recorder{fail: map[string]error{"ip link add": errors.New("RTNETLINK answers: File exists")}}
	a2 := &Agent{Run: r2.run, ConfigDir: cfg, KeyDir: key, RuntimeDir: rt}
	require.NoError(t, a2.Sync(context.Background()))
	for _, c := range r2.calls {
		assert.NotContains(t, c, "iptables -w -A", "règle déjà présente")
	}
}

func TestSyncRefusesBadKey(t *testing.T) {
	cfg, key, rt := writeFiles(t, sample(), "pas une clé")
	r := &recorder{}
	a := &Agent{Run: r.run, ConfigDir: cfg, KeyDir: key, RuntimeDir: rt}
	assert.Error(t, a.Sync(context.Background()))
	assert.Empty(t, r.calls)
}

func TestHandler(t *testing.T) {
	key := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(key, StatusTokenKey), []byte("secret-token\n"), 0o600))
	a := &Agent{KeyDir: key, Output: func(context.Context, string, ...string) (string, error) {
		return "PRIV\tPUB\t51820\toff\n" + vpsKey + "\t(none)\t(none)\t10.248.3.3/32\t0\t0\t0\toff\n", nil
	}}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
	res.Body.Close()

	for _, q := range []string{"", "?token=", "?token=autre"} {
		res, err = http.Get(srv.URL + "/status" + q)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, res.StatusCode, "jeton %q", q)
		res.Body.Close()
	}
	res, err = http.Get(srv.URL + "/status?token=secret-token")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	var st []PeerStatus
	require.NoError(t, json.NewDecoder(res.Body).Decode(&st))
	assert.Equal(t, []PeerStatus{{PublicKey: vpsKey}}, st)

	// Sans jeton configuré : refus, jamais ouvert.
	b := &Agent{KeyDir: t.TempDir(), Output: a.Output}
	srv2 := httptest.NewServer(b.Handler())
	defer srv2.Close()
	res2, err := http.Get(srv2.URL + "/status?token=")
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, res2.StatusCode)
	res2.Body.Close()
}
