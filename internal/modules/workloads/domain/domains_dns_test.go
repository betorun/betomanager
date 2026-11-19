package domain

import "testing"

func TestNormalizePath(t *testing.T) {
	ok := map[string]string{"": "/", "/": "/", "/auth": "/auth", "/auth/": "/auth", " /api/v1 ": "/api/v1", "/a-b_c.d~e": "/a-b_c.d~e"}
	for in, want := range ok {
		if got, err := NormalizePath(in); err != nil || got != want {
			t.Errorf("%q → %q, %v ; attendu %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"auth", "/a//b", "/a/../b", "/./a", "/a b", "/a?x", "/é"} {
		if _, err := NormalizePath(bad); err != ErrInvalidPath {
			t.Errorf("%q accepté", bad)
		}
	}
}

func TestZoneAndRelativeHost(t *testing.T) {
	cases := []struct{ host, zone, rel string }{
		{"exemple.sn", "exemple.sn", "@"},
		{"www.exemple.sn", "exemple.sn", "www"},
		{"a.b.exemple.co.uk", "exemple.co.uk", "a.b"},
		{"_betorun-challenge.exemple.sn", "exemple.sn", "_betorun-challenge"},
	}
	for _, c := range cases {
		if z := Zone(c.host); z != c.zone {
			t.Errorf("Zone(%q) = %q", c.host, z)
		}
		if r := RelativeHost(c.host, c.zone); r != c.rel {
			t.Errorf("RelativeHost(%q) = %q", c.host, r)
		}
	}
	if !IsApex("exemple.sn") || IsApex("www.exemple.sn") {
		t.Error("IsApex")
	}
}

func TestDNSProviderOf(t *testing.T) {
	cases := map[string][]string{
		"cloudflare": {"ada.ns.cloudflare.com."},
		"ovh":        {"dns200.anycast.me", "ns200.anycast.me"},
		"godaddy":    {"ns51.domaincontrol.com."},
		"namecheap":  {"dns1.registrar-servers.com"},
		"route53":    {"ns-1536.awsdns-00.co.uk."},
		"ionos":      {"ns1045.ui-dns.de"},
		"":           {"ns1.inconnu.example"},
	}
	for want, ns := range cases {
		if got := DNSProviderOf(ns); got != want {
			t.Errorf("%v → %q, attendu %q", ns, got, want)
		}
	}
}
