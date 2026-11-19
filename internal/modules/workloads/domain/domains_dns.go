package domain

import (
	"strings"

	"golang.org/x/net/publicsuffix"
)

const maxPathLen = 100

// NormalizePath : chemin routé ; vide = « / ». Pas de « / » final (sauf la
// racine), ni « . » / « .. », ni caractère hors de l'URL simple.
func NormalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") || len(p) > maxPathLen {
		return "", ErrInvalidPath
	}
	p = strings.TrimRight(p, "/")
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrInvalidPath
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
				return "", ErrInvalidPath
			}
		}
	}
	return p, nil
}

// Zone : domaine enregistré (exemple.sn pour www.exemple.sn, exemple.co.uk
// pour app.exemple.co.uk). Inconnu : le nom lui-même.
func Zone(host string) string {
	z, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return z
}

// IsApex : le nom est le sommet de sa zone (exemple.sn).
func IsApex(host string) bool { return Zone(host) == host }

// RelativeHost : nom à saisir chez un fournisseur DNS, relatif à la zone.
func RelativeHost(fqdn, zone string) string {
	if fqdn == zone {
		return "@"
	}
	return strings.TrimSuffix(fqdn, "."+zone)
}

// dnsProviders : suffixe des serveurs de noms → fournisseur. La console a un
// guide pour chacun ; les autres ont le guide général.
var dnsProviders = []struct{ suffix, id string }{
	{"ns.cloudflare.com", "cloudflare"},
	{"ovh.net", "ovh"},
	{"anycast.me", "ovh"},
	{"gandi.net", "gandi"},
	{"domaincontrol.com", "godaddy"},
	{"registrar-servers.com", "namecheap"},
	{"googledomains.com", "google"},
	{"awsdns", "route53"},
	{"ui-dns.", "ionos"},
	{"dns-parking.com", "hostinger"},
	{"porkbun.com", "porkbun"},
	{"vercel-dns.com", "vercel"},
	{"digitalocean.com", "digitalocean"},
	{"azure-dns.", "azure"},
	{"lwsdns.com", "lws"},
	{"lws.fr", "lws"},
}

// DNSProviderOf : fournisseur reconnu d'après ses serveurs de noms.
func DNSProviderOf(nameservers []string) string {
	for _, ns := range nameservers {
		ns = strings.TrimSuffix(strings.ToLower(ns), ".")
		for _, p := range dnsProviders {
			if strings.HasSuffix(ns, p.suffix) || (strings.HasSuffix(p.suffix, ".") && strings.Contains(ns, p.suffix)) ||
				(p.suffix == "awsdns" && strings.Contains(ns, "awsdns")) {
				return p.id
			}
		}
	}
	return ""
}
