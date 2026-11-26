// Package vpcgateway : passerelle du réseau privé (VPC) d'un projet, mise en
// œuvre « tunnel » de l'ADR 0019. Un pod par projet, dans l'espace du projet :
// il termine le tunnel des VPS rattachés et relaie leur trafic vers les
// conteneurs et bases du projet (NetworkPolicy du projet), jamais ailleurs.
//
// L'API écrit la configuration (ConfigMap, clés publiques et adresses) et la
// clé privée de la passerelle (Secret) ; l'agent les relit et les applique.
// Ce paquet ne dépend ni de Kubernetes ni de la base : il est partagé par
// l'API (backend/tunnel) et l'agent (cmd/vpc-gateway).
package vpcgateway

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Noms et chemins communs à l'API et à l'agent.
const (
	// Interface : interface du tunnel dans le pod de la passerelle.
	Interface = "vpc0"
	// ListenPort : port UDP du tunnel dans le pod (le Service l'expose sur
	// le port propre au VPC).
	ListenPort = 51820
	// StatusPort : état des pairs (GET /status), lu par l'API via le
	// mandataire de l'API Kubernetes ; jamais exposé hors du cluster.
	StatusPort = 8080

	// ConfigKey : clé du ConfigMap ; PrivateKeyKey : clé du Secret.
	ConfigKey     = "gateway.json"
	PrivateKeyKey = "private"
	// StatusTokenKey : jeton exigé par GET /status (même Secret que la clé).
	StatusTokenKey = "status-token"

	// Points de montage dans le pod.
	ConfigDir  = "/etc/vpc/config"
	KeyDir     = "/etc/vpc/key"
	RuntimeDir = "/run/vpc"
)

// Config : ce que la passerelle applique (aucun secret : la clé privée est
// dans le Secret à part).
type Config struct {
	// CIDR : plage du VPC ; Gateway : adresse de la passerelle (.1).
	CIDR    string `json:"cidr"`
	Gateway string `json:"gateway"`
	// Routes : plages joignables depuis les VPS (pods et services du cluster).
	Routes []string `json:"routes"`
	Peers  []Peer   `json:"peers"`
}

// Peer : un VPS rattaché. PublicKey vide : pas encore inscrit (pas de pair).
type Peer struct {
	VPSID      string `json:"vps_id"`
	Name       string `json:"name"`
	Address    string `json:"address"`
	PublicKey  string `json:"public_key,omitempty"`
	PublicIPv4 string `json:"public_ipv4,omitempty"`
}

// Parse : configuration lue par l'agent, contrôlée (rien d'inattendu n'est
// passé aux commandes du système).
func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate : plages, adresses et clés bien formées.
func (c *Config) Validate() error {
	p, err := netip.ParsePrefix(c.CIDR)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return fmt.Errorf("invalid cidr %q", c.CIDR)
	}
	gw, err := netip.ParseAddr(c.Gateway)
	if err != nil || !p.Contains(gw) {
		return fmt.Errorf("invalid gateway %q", c.Gateway)
	}
	for _, r := range c.Routes {
		if rp, err := netip.ParsePrefix(r); err != nil || rp != rp.Masked() {
			return fmt.Errorf("invalid route %q", r)
		}
	}
	for _, peer := range c.Peers {
		a, err := netip.ParseAddr(peer.Address)
		if err != nil || !p.Contains(a) || a == gw {
			return fmt.Errorf("invalid peer address %q", peer.Address)
		}
		if peer.PublicKey != "" && !ValidKey(peer.PublicKey) {
			return fmt.Errorf("invalid peer key for %s", peer.VPSID)
		}
	}
	return nil
}

// Prefix : longueur de la plage (pour l'adresse de l'interface).
func (c *Config) Prefix() int {
	p, _ := netip.ParsePrefix(c.CIDR)
	return p.Bits()
}

// Render : configuration du tunnel (format de « wg syncconf »). Seuls les
// VPS inscrits sont des pairs ; chacun n'a que son adresse (/32).
func Render(c *Config, privateKey string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nListenPort = %d\n", strings.TrimSpace(privateKey), ListenPort)
	peers := append([]Peer(nil), c.Peers...)
	sort.Slice(peers, func(i, j int) bool { return peers[i].Address < peers[j].Address })
	for _, p := range peers {
		if p.PublicKey == "" {
			continue
		}
		fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n", p.PublicKey, p.Address)
	}
	return b.String()
}

// PeerStatus : état d'un pair (GET /status) ; jamais de clé privée.
type PeerStatus struct {
	PublicKey string `json:"public_key"`
	// LatestHandshake : secondes Unix de la dernière poignée de main (0 : jamais).
	LatestHandshake int64 `json:"latest_handshake"`
	RxBytes         int64 `json:"rx_bytes"`
	TxBytes         int64 `json:"tx_bytes"`
}

// ParseDump : sortie de « wg show <if> dump ». La première ligne (celle de
// l'interface, avec la clé privée) est toujours ignorée.
func ParseDump(out string) []PeerStatus {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	res := []PeerStatus{}
	for i, l := range lines {
		if i == 0 {
			continue // interface : clé privée, jamais rendue
		}
		f := strings.Split(l, "\t")
		if len(f) < 8 {
			continue
		}
		var hs, rx, tx int64
		fmt.Sscan(f[4], &hs)
		fmt.Sscan(f[5], &rx)
		fmt.Sscan(f[6], &tx)
		res = append(res, PeerStatus{PublicKey: f[0], LatestHandshake: hs, RxBytes: rx, TxBytes: tx})
	}
	return res
}
