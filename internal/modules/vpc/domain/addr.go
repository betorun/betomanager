package domain

import (
	"net/netip"
	"regexp"
	"strings"
)

// Pool : plages des VPC, un /24 chacun, hors des plages des pods
// (10.42.0.0/16) et services (10.43.0.0/16) des clusters. 2 048 VPC par
// cluster ; deux clusters peuvent réutiliser une plage (VPC isolés).
var Pool = netip.MustParsePrefix("10.248.0.0/13")

// RangeBits : taille d'une plage de VPC.
const RangeBits = 24

// NextRange : première plage /24 du Pool absente de used.
func NextRange(used []netip.Prefix) (netip.Prefix, error) {
	taken := make(map[netip.Prefix]bool, len(used))
	for _, p := range used {
		taken[p.Masked()] = true
	}
	a := Pool.Addr()
	for Pool.Contains(a) {
		p := netip.PrefixFrom(a, RangeBits)
		if !taken[p] {
			return p, nil
		}
		b := a.As4()
		b[2]++
		if b[2] == 0 {
			b[1]++
		}
		a = netip.AddrFrom4(b)
	}
	return netip.Prefix{}, ErrRangeExhausted
}

// GatewayOf : première adresse utile de la plage, réservée à la plateforme.
func GatewayOf(p netip.Prefix) netip.Addr { return p.Masked().Addr().Next() }

// NextAddress : première adresse libre d'un VPS dans la plage : ni le
// réseau (.0), ni la plateforme (.1), ni la diffusion (.255).
func NextAddress(p netip.Prefix, used []netip.Addr) (netip.Addr, error) {
	taken := make(map[netip.Addr]bool, len(used))
	for _, a := range used {
		taken[a] = true
	}
	p = p.Masked()
	a := GatewayOf(p).Next()
	for p.Contains(a) {
		next := a.Next()
		if !p.Contains(next) {
			break // diffusion
		}
		if !taken[a] {
			return a, nil
		}
		a = next
	}
	return netip.Addr{}, ErrRangeExhausted
}

// InternalName : nom d'un membre dans le réseau privé (vps-1.internal).
func InternalName(name string) string { return strings.ToLower(name) + NameSuffix }

// publicKeyRe : clé publique envoyée par un VPS : texte court, sans espace
// ni caractère de contrôle. La mise en œuvre en vérifie le format exact.
var publicKeyRe = regexp.MustCompile(`^[A-Za-z0-9+/=_:.-]{16,256}$`)

// ValidPublicKey : forme acceptable d'une clé publique.
func ValidPublicKey(k string) bool { return publicKeyRe.MatchString(k) }
