// Package operator : l'Opérateur du back-office (ADR 0016), un agent
// d'exploitation qui tourne sur le serveur de gestion, hors de l'API et des
// clusters qu'il dépanne.
//
// Ce paquet ne dépend ni du routeur, ni des modules, ni de la configuration
// de l'API (test deps_test.go) : l'Opérateur doit démarrer quand le serveur 1
// est en panne, et ne parle à l'API que par HTTP.
package operator

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/core/opjournal"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/anthropic"
)

// Valeurs par défaut.
const (
	DefaultListen = "127.0.0.1:8095"
	DefaultModel  = "claude-opus-5-5"
	DefaultEffort = "high"
)

// Config : réglages de l'Opérateur, lus dans l'environnement.
//
//	OPERATOR_LISTEN                adresse d'écoute (défaut 127.0.0.1:8095)
//	OPERATOR_LISTEN_ANY            "true" : accepte une adresse non locale
//	                               (conteneur dont le port est publié en local)
//	OPERATOR_DATABASE_URL          base propre de l'Opérateur (requis)
//	OPERATOR_API_URL               API du back-office, chemin normal (facultatif)
//	OPERATOR_CONSOLE_ORIGINS       origines de la console autorisées (CORS),
//	                               séparées par des virgules
//	OPERATOR_KUBECONFIG_DIR        un kubeconfig par cluster (identité de
//	                               dépannage) ; vide = aucun cluster
//	OPERATOR_PROMETHEUS            sources Prometheus du chemin de secours,
//	                               "nom=url,nom=url" ; vide = aucune
//	OPERATOR_NODES                 agents de nœud, "nom=https://hôte:port,…" ;
//	                               vide = aucun
//	OPERATOR_NODE_CERT_FILE / _KEY_FILE  certificat client mTLS de l'Opérateur
//	OPERATOR_NODE_CA_FILE          autorité des certificats des nœuds
//	OPERATOR_ORDER_KEY_FILE        clé Ed25519 qui signe les ordres validés
//	                               (operator order-key) ; vide = lecture seule
//	OPERATOR_MODEL                 modèle (défaut claude-opus-5-5)
//	OPERATOR_EFFORT                effort (défaut high)
//	OPERATOR_ANTHROPIC_API_KEY_FILE  fichier de la clé du modèle (de préférence)
//	OPERATOR_ANTHROPIC_API_KEY     clé du modèle (sinon)
//	OPERATOR_RESCUE_KEY_FILE       clé (32 octets en base64) des secrets TOTP de
//	                               l'accès de secours ; vide = accès désactivé
//	OPERATOR_JOURNAL_KEY_FILE      clé Ed25519 (graine de 32 octets en base64)
//	                               qui signe la recopie du journal dans l'audit
//	                               (operator journal-key) ; vide = pas de recopie
//	OPERATOR_ALERT_WEBHOOK_FILE    fichier de l'URL du webhook d'alerte de
//	                               l'équipe (hors du serveur 1) ; obligatoire
//	                               avec l'accès de secours
//	OPERATOR_ALERT_LOG_ONLY        "true" : développement, alertes dans le
//	                               journal du processus au lieu du webhook ;
//	                               refusé si l'écoute n'est pas locale
type Config struct {
	Listen      string
	DatabaseURL string
	APIURL      string
	// ConsoleOrigins : origines de la console (CORS).
	ConsoleOrigins []string
	// KubeconfigDir : chemin de secours vers les clusters (kube.LoadDir).
	KubeconfigDir string
	// KubeActions : cartes d'action sur les clusters (le rôle de l'identité
	// de dépannage doit ouvrir les verbes d'écriture listés dans la doc).
	KubeActions bool
	// Prometheus : sources du chemin de secours (prom.Parse).
	Prometheus string
	// Nodes : agents de nœud (nodes.Parse) et leurs fichiers mTLS.
	Nodes                     string
	NodeCert, NodeKey, NodeCA string
	// OrderKey : secret, jamais journalisé (graine Ed25519 en base64).
	OrderKey string
	Model    string
	Effort   string
	// AnthropicKey : secret, jamais journalisé. Vide = Opérateur sans agent
	// (la console reste utilisable à la main, ADR 0016 §2).
	AnthropicKey string
	// RescueKey : secret, jamais journalisé (rescue.New).
	RescueKey string
	// JournalKey : secret, jamais journalisé (opjournal.ParsePrivateKey).
	JournalKey string
	// AlertWebhook : secret, jamais journalisé (WebhookAlerter).
	AlertWebhook string
	// AlertLogOnly : développement (LogAlerter).
	AlertLogOnly bool
	// ResultRetention : durée de conservation des résultats complets des
	// outils ; ensuite seul le condensé reste (ADR 0016 §5).
	ResultRetention time.Duration
}

// LoadConfig : lit et vérifie les réglages. getenv vaut os.Getenv hors tests.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Listen:          or(getenv("OPERATOR_LISTEN"), DefaultListen),
		DatabaseURL:     getenv("OPERATOR_DATABASE_URL"),
		APIURL:          strings.TrimRight(getenv("OPERATOR_API_URL"), "/"),
		KubeconfigDir:   getenv("OPERATOR_KUBECONFIG_DIR"),
		Prometheus:      getenv("OPERATOR_PROMETHEUS"),
		Nodes:           getenv("OPERATOR_NODES"),
		NodeCert:        getenv("OPERATOR_NODE_CERT_FILE"),
		NodeKey:         getenv("OPERATOR_NODE_KEY_FILE"),
		NodeCA:          getenv("OPERATOR_NODE_CA_FILE"),
		Model:           or(getenv("OPERATOR_MODEL"), DefaultModel),
		Effort:          or(getenv("OPERATOR_EFFORT"), DefaultEffort),
		ResultRetention: 30 * 24 * time.Hour,
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("operator: OPERATOR_DATABASE_URL est obligatoire")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return Config{}, fmt.Errorf("operator: OPERATOR_LISTEN invalide : %w", err)
	}
	if !loopback(host) && getenv("OPERATOR_LISTEN_ANY") != "true" {
		return Config{}, errors.New("operator: OPERATOR_LISTEN doit rester local (tunnel SSH) ; OPERATOR_LISTEN_ANY=true pour un conteneur")
	}
	for _, o := range strings.Split(getenv("OPERATOR_CONSOLE_ORIGINS"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			return Config{}, fmt.Errorf("operator: origine de console invalide : %q", o)
		}
		c.ConsoleOrigins = append(c.ConsoleOrigins, o)
	}
	if c.APIURL != "" {
		u, err := url.Parse(c.APIURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, errors.New("operator: OPERATOR_API_URL invalide")
		}
	}
	if f := getenv("OPERATOR_ANTHROPIC_API_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return Config{}, fmt.Errorf("operator: lecture de la clé du modèle : %w", err)
		}
		c.AnthropicKey = strings.TrimSpace(string(b))
	} else {
		c.AnthropicKey = strings.TrimSpace(getenv("OPERATOR_ANTHROPIC_API_KEY"))
	}
	if f := getenv("OPERATOR_RESCUE_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return Config{}, fmt.Errorf("operator: lecture de la clé de secours : %w", err)
		}
		c.RescueKey = strings.TrimSpace(string(b))
	}
	if f := getenv("OPERATOR_JOURNAL_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return Config{}, fmt.Errorf("operator: lecture de la clé du journal : %w", err)
		}
		c.JournalKey = strings.TrimSpace(string(b))
		if _, err := opjournal.ParsePrivateKey(c.JournalKey); err != nil {
			return Config{}, err
		}
	}
	if f := getenv("OPERATOR_ORDER_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return Config{}, fmt.Errorf("operator: lecture de la clé d'ordres : %w", err)
		}
		c.OrderKey = strings.TrimSpace(string(b))
		if _, err := opjournal.ParsePrivateKey(c.OrderKey); err != nil {
			return Config{}, err
		}
	}
	if f := getenv("OPERATOR_ALERT_WEBHOOK_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return Config{}, fmt.Errorf("operator: lecture du webhook d'alerte : %w", err)
		}
		c.AlertWebhook = strings.TrimSpace(string(b))
		if u, err := url.Parse(c.AlertWebhook); err != nil || u.Scheme != "https" || u.Host == "" {
			return Config{}, errors.New("operator: le webhook d'alerte doit être une URL https")
		}
	}
	// Un accès de secours que personne ne voit passer n'est pas acceptable.
	if getenv("OPERATOR_ALERT_LOG_ONLY") == "true" {
		if !loopback(host) || getenv("OPERATOR_LISTEN_ANY") == "true" {
			return Config{}, errors.New("operator: OPERATOR_ALERT_LOG_ONLY est réservé au développement (écoute locale)")
		}
		c.AlertLogOnly = true
	}
	c.KubeActions = getenv("OPERATOR_KUBE_ACTIONS") == "true"
	if c.KubeActions && c.KubeconfigDir == "" {
		return Config{}, errors.New("operator: OPERATOR_KUBE_ACTIONS exige OPERATOR_KUBECONFIG_DIR")
	}
	if c.RescueKey != "" && c.AlertWebhook == "" && !c.AlertLogOnly {
		return Config{}, errors.New("operator: l'accès de secours exige OPERATOR_ALERT_WEBHOOK_FILE (alerte de l'équipe)")
	}
	return c, nil
}

// LLM : le client du modèle, ou nil sans clé.
func (c Config) LLM() (*anthropic.Client, error) {
	if c.AnthropicKey == "" {
		return nil, nil
	}
	return anthropic.New(anthropic.Config{Name: "claude", APIKey: c.AnthropicKey, Model: c.Model, Effort: c.Effort})
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
