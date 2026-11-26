package vpcgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Agent : applique la configuration montée (ConfigMap et Secret) au tunnel
// et au filtrage du pod. Run et Output exécutent les commandes du système
// (ip, wg, iptables) ; remplacés dans les tests.
type Agent struct {
	Run    func(ctx context.Context, name string, args ...string) error
	Output func(ctx context.Context, name string, args ...string) (string, error)
	// ConfigDir, KeyDir, RuntimeDir : défauts du paquet si vides.
	ConfigDir, KeyDir, RuntimeDir string
	Every                         time.Duration
	Logger                        *slog.Logger

	applied [32]byte
	ready   atomic.Bool
}

// Exec : commandes réelles du système.
func Exec(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ExecOutput : sortie standard d'une commande du système.
func ExecOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func (a *Agent) dir(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func (a *Agent) load() (*Config, string, error) {
	raw, err := os.ReadFile(filepath.Join(a.dir(a.ConfigDir, ConfigDir), ConfigKey))
	if err != nil {
		return nil, "", err
	}
	c, err := Parse(raw)
	if err != nil {
		return nil, "", err
	}
	key, err := os.ReadFile(filepath.Join(a.dir(a.KeyDir, KeyDir), PrivateKeyKey))
	if err != nil {
		return nil, "", err
	}
	k := strings.TrimSpace(string(key))
	if !ValidKey(k) {
		return nil, "", errors.New("invalid gateway private key")
	}
	return c, k, nil
}

// Setup : interface du tunnel et filtrage, une fois au démarrage (le pod a
// son propre espace réseau : rien n'existe encore). Les VPS ne joignent que
// les plages Routes ; ni Internet, ni la passerelle elle-même, ni (lot 3)
// les autres VPS.
func (a *Agent) Setup(ctx context.Context, c *Config) error {
	cidr := c.CIDR
	steps := [][]string{
		{"ip", "link", "add", Interface, "type", "wireguard"},
		{"ip", "address", "replace", fmt.Sprintf("%s/%d", c.Gateway, c.Prefix()), "dev", Interface},
		{"ip", "link", "set", Interface, "up"},
		{"iptables", "-w", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "!", "-o", Interface, "-j", "MASQUERADE"},
		{"iptables", "-w", "-A", "INPUT", "-i", Interface, "-j", "DROP"},
		{"iptables", "-w", "-A", "FORWARD", "-i", Interface, "-o", Interface, "-j", "DROP"},
	}
	for _, r := range c.Routes {
		steps = append(steps, []string{"iptables", "-w", "-A", "FORWARD", "-i", Interface, "-d", r, "-j", "ACCEPT"})
	}
	steps = append(steps,
		[]string{"iptables", "-w", "-A", "FORWARD", "-i", Interface, "-j", "DROP"},
		// Retour des connexions ouvertes par les VPS seulement.
		[]string{"iptables", "-w", "-A", "FORWARD", "-o", Interface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		[]string{"iptables", "-w", "-A", "FORWARD", "-o", Interface, "-j", "DROP"},
	)
	for i, s := range steps {
		if s[0] == "iptables" {
			// Idempotent : un conteneur relancé garde l'espace réseau du pod.
			check := append([]string(nil), s[1:]...)
			for j, v := range check {
				if v == "-A" {
					check[j] = "-C"
				}
			}
			if a.Run(ctx, "iptables", check...) == nil {
				continue
			}
		}
		if err := a.Run(ctx, s[0], s[1:]...); err != nil {
			if i == 0 && strings.Contains(err.Error(), "File exists") {
				continue // interface déjà créée (conteneur relancé)
			}
			return err
		}
	}
	return nil
}

// Sync : configuration du tunnel appliquée si elle a changé (wg syncconf :
// pairs ajoutés et retirés sans couper les autres).
func (a *Agent) Sync(ctx context.Context) error {
	c, key, err := a.load()
	if err != nil {
		return err
	}
	if !a.ready.Load() {
		if err := a.Setup(ctx, c); err != nil {
			return err
		}
		a.ready.Store(true)
	}
	conf := Render(c, key)
	sum := sha256.Sum256([]byte(conf))
	if sum == a.applied {
		return nil
	}
	rt := a.dir(a.RuntimeDir, RuntimeDir)
	if err := os.MkdirAll(rt, 0o700); err != nil {
		return err
	}
	path := filepath.Join(rt, Interface+".conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		return err
	}
	defer os.Remove(path)
	if err := a.Run(ctx, "wg", "syncconf", Interface, path); err != nil {
		return err
	}
	a.applied = sum
	if a.Logger != nil {
		n := 0
		for _, p := range c.Peers {
			if p.PublicKey != "" {
				n++
			}
		}
		a.Logger.Info("tunnel configuration applied", "peers", n)
	}
	return nil
}

// Loop : Sync à intervalle (le ConfigMap monté suit l'API en une minute au
// plus) jusqu'à l'arrêt.
func (a *Agent) Loop(ctx context.Context) {
	every := a.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.Sync(ctx); err != nil && a.Logger != nil {
			a.Logger.Error("sync", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Handler : GET /status (pairs et dernière poignée de main), GET /healthz.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		// Le port est joignable depuis le cluster (autres projets compris) :
		// seule l'API, qui lit le jeton dans le Secret, a l'état des pairs.
		want, err := os.ReadFile(filepath.Join(a.dir(a.KeyDir, KeyDir), StatusTokenKey))
		got := r.URL.Query().Get("token")
		if err != nil || len(bytes.TrimSpace(want)) == 0 ||
			subtle.ConstantTimeCompare(bytes.TrimSpace(want), []byte(got)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		out, err := a.Output(r.Context(), "wg", "show", Interface, "dump")
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ParseDump(out))
	})
	return mux
}
