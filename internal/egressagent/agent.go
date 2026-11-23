// Package egressagent : agent de nœud des IP de sortie dédiées (ADR 0014,
// lot 2). La plateforme écrit dans le cluster les IP que chaque nœud
// passerelle doit porter (ConfigMap DesiredConfigMap) ; l'agent, sur chaque
// nœud, les monte sur l'interface de sortie, retire celles qui ne sont plus
// voulues, et publie ce qu'il porte vraiment (StatusConfigMap). Il ne touche
// qu'aux adresses qu'il a posées (étiquette <interface>:bt).
package egressagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	// Namespace : celui du compte plateforme et des agents.
	Namespace = "betorun-system"
	// DesiredConfigMap : IP voulues, par nœud (écrit par la plateforme).
	DesiredConfigMap = "betorun-egress"
	// DesiredKey : clé de DesiredConfigMap (JSON : nœud → adresses).
	DesiredKey = "addresses.json"
	// StatusConfigMap : IP portées, une clé par nœud (écrit par les agents).
	StatusConfigMap = "betorun-egress-status"
	// Name : DaemonSet, compte et rôle de l'agent.
	Name = "betorun-egress-agent"
	// labelSuffix : étiquette des adresses posées par l'agent.
	labelSuffix = ":bt"
	// maxLabel : longueur maximale d'une étiquette d'adresse (IFNAMSIZ-1).
	maxLabel = 15
)

// Desired : nœud → adresses IPv4 à porter.
type Desired map[string][]string

// Status : ce qu'un nœud porte (clé <nœud> de StatusConfigMap).
type Status struct {
	Interface string    `json:"interface"`
	Addresses []string  `json:"addresses"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

// StaleAfter : statut plus vieux : l'agent du nœud est tenu pour arrêté.
const StaleAfter = 2 * time.Minute

// Mounted : adresses réellement portées (statut récent, sans erreur pour elles).
func (s Status) Mounted(now time.Time) []string {
	if now.Sub(s.At) > StaleAfter {
		return nil
	}
	return s.Addresses
}

// Runner : lance la commande ip (iproute2) ; remplacé dans les tests.
type Runner func(ctx context.Context, args ...string) (string, error)

// IP : la vraie commande ip.
func IP(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ErrNoDefaultRoute : pas de route par défaut (interface de sortie inconnue).
var ErrNoDefaultRoute = errors.New("no IPv4 default route")

// ErrInterfaceName : nom d'interface trop long pour une étiquette.
var ErrInterfaceName = errors.New("interface name too long to label addresses")

// DefaultInterface : interface de la route IPv4 par défaut.
func DefaultInterface(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, "-4", "route", "show", "default")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "dev" {
				return f[i+1], nil
			}
		}
	}
	return "", ErrNoDefaultRoute
}

func label(iface string) (string, error) {
	l := iface + labelSuffix
	if len(l) > maxLabel {
		return "", ErrInterfaceName
	}
	return l, nil
}

// Owned : adresses posées par l'agent sur l'interface.
func Owned(ctx context.Context, run Runner, iface string) ([]string, error) {
	lbl, err := label(iface)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, "-4", "-o", "addr", "show", "dev", iface, "label", lbl)
	if err != nil {
		return nil, err
	}
	var have []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" {
				if p, err := netip.ParsePrefix(f[i+1]); err == nil {
					have = append(have, p.Addr().String())
				}
			}
		}
	}
	sort.Strings(have)
	return have, nil
}

// Normalize : IPv4 valides, uniques, triées (une entrée invalide est ignorée).
func Normalize(addrs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range addrs {
		ip, err := netip.ParseAddr(strings.TrimSpace(a))
		if err != nil || !ip.Is4() || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}

// Reconcile : l'interface porte exactement want parmi ses adresses
// étiquetées ; rend ce qu'elle porte après coup. Les erreurs d'une adresse
// n'arrêtent pas les autres.
func Reconcile(ctx context.Context, run Runner, iface string, want []string) ([]string, error) {
	lbl, err := label(iface)
	if err != nil {
		return nil, err
	}
	have, err := Owned(ctx, run, iface)
	if err != nil {
		return nil, err
	}
	want = Normalize(want)
	wanted := map[string]bool{}
	for _, a := range want {
		wanted[a] = true
	}
	present := map[string]bool{}
	for _, a := range have {
		present[a] = true
	}
	var errs []error
	for _, a := range want {
		if !present[a] {
			if _, err := run(ctx, "-4", "addr", "add", a+"/32", "dev", iface, "label", lbl); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, a := range have {
		if !wanted[a] {
			if _, err := run(ctx, "-4", "addr", "del", a+"/32", "dev", iface); err != nil {
				errs = append(errs, err)
			}
		}
	}
	after, err := Owned(ctx, run, iface)
	if err != nil {
		return nil, err
	}
	return after, errors.Join(errs...)
}

// ParseDesired : contenu de DesiredConfigMap (absent ou vide : rien à porter).
func ParseDesired(raw string) (Desired, error) {
	d := Desired{}
	if strings.TrimSpace(raw) == "" {
		return d, nil
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("desired egress addresses: %w", err)
	}
	return d, nil
}
