package operator

import (
	"os"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.Model != DefaultModel || c.Effort != DefaultEffort || c.AnthropicKey != "" {
		t.Fatalf("défauts = %+v", c)
	}
	if m, err := c.LLM(); err != nil || m != nil {
		t.Fatalf("sans clé : %v %v", m, err)
	}

	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-fichier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_ANTHROPIC_API_KEY_FILE": key,
		"OPERATOR_ANTHROPIC_API_KEY": "sk-env", "OPERATOR_LISTEN": "0.0.0.0:8095", "OPERATOR_LISTEN_ANY": "true",
		"OPERATOR_API_URL": "http://api:8080/kmanager/v1/"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AnthropicKey != "sk-fichier" || c.APIURL != "http://api:8080/kmanager/v1" {
		t.Fatalf("réglages = %+v", c)
	}
	if m, err := c.LLM(); err != nil || m == nil {
		t.Fatalf("avec clé : %v", err)
	}

	bad := map[string]map[string]string{
		"sans base":       {},
		"écoute publique": {"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_LISTEN": "0.0.0.0:8095"},
		"écoute invalide": {"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_LISTEN": "8095"},
		"api invalide":    {"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_API_URL": "api:8080"},
		"clé illisible":   {"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_ANTHROPIC_API_KEY_FILE": "/nonexistent/key"},
	}
	for name, m := range bad {
		if _, err := LoadConfig(env(m)); err == nil {
			t.Errorf("%s : erreur attendue", name)
		}
	}
	c, err = LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y",
		"OPERATOR_CONSOLE_ORIGINS": "http://localhost:3001/, https://console.example.test"}))
	if err != nil || len(c.ConsoleOrigins) != 2 || c.ConsoleOrigins[0] != "http://localhost:3001" {
		t.Fatalf("origines = %v %v", c.ConsoleOrigins, err)
	}
	if _, err := LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_CONSOLE_ORIGINS": "*"})); err == nil {
		t.Error("origine * acceptée")
	}

	// Accès de secours : jamais sans alerte de l'équipe.
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rk, hook, plain := write("rescue", "Y2xlZg=="), write("hook", "https://chat.example.test/hooks/x"), write("plain", "http://chat.example.test/x")
	if _, err := LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_RESCUE_KEY_FILE": rk})); err == nil {
		t.Error("secours sans webhook accepté")
	}
	if _, err := LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_RESCUE_KEY_FILE": rk, "OPERATOR_ALERT_WEBHOOK_FILE": plain})); err == nil {
		t.Error("webhook http accepté")
	}
	if c, err := LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_RESCUE_KEY_FILE": rk, "OPERATOR_ALERT_LOG_ONLY": "true"})); err != nil || !c.AlertLogOnly {
		t.Fatalf("développement : %v", err)
	}
	if _, err := LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_RESCUE_KEY_FILE": rk, "OPERATOR_ALERT_LOG_ONLY": "true",
		"OPERATOR_LISTEN": "0.0.0.0:8095", "OPERATOR_LISTEN_ANY": "true"})); err == nil {
		t.Error("alertes en journal seulement acceptées hors du poste local")
	}
	c, err = LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_RESCUE_KEY_FILE": rk, "OPERATOR_ALERT_WEBHOOK_FILE": hook}))
	if err != nil || c.AlertWebhook != "https://chat.example.test/hooks/x" {
		t.Fatalf("webhook = %q %v", c.AlertWebhook, err)
	}

	c, _ = LoadConfig(env(map[string]string{"OPERATOR_DATABASE_URL": "postgres://x/y", "OPERATOR_ANTHROPIC_API_KEY": "k", "OPERATOR_EFFORT": "turbo"}))
	if _, err := c.LLM(); err == nil {
		t.Error("effort inconnu accepté")
	}
}
