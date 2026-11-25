package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func healthOf(state string, alloc, req int64, reasons ...string) *Health {
	return &Health{State: state, CPUAllocMilli: alloc, CPUReqMilli: req, Reasons: reasons}
}

func TestPlace(t *testing.T) {
	cs := []Candidate{
		{ClusterID: "def", Region: "eu-west-gra", State: StateActive, IsDefault: true, Reachable: true, Health: healthOf(HealthOK, 6000, 5000)},
		{ClusterID: "gra-2", Region: "eu-west-gra", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 6000, 1000)},
		{ClusterID: "gra-full", Region: "eu-west-gra", State: StateActive, Reachable: true, Health: healthOf(HealthDegraded, 6000, 100, ReasonMemoryPressure)},
		{ClusterID: "rbx-down", Region: "eu-west-rbx", State: StateActive, Reachable: true, Health: healthOf(HealthDown, 0, 0, ReasonAPIUnreachable)},
		{ClusterID: "sbg-closed", Region: "eu-west-sbg", State: StateClosed, Reachable: true, Health: healthOf(HealthOK, 6000, 0)},
		{ClusterID: "dkr-noaccess", Region: "af-west-dkr", State: StateActive, Health: healthOf(HealthUnknown, 0, 0, ReasonNoAccess)},
	}
	got, err := Place(cs, "")
	assert.NoError(t, err)
	assert.Equal(t, "def", got, "sans région : le défaut s'il peut recevoir")

	got, err = Place(cs, "eu-west-gra")
	assert.NoError(t, err)
	assert.Equal(t, "gra-2", got, "le plus de place libre, jamais un cluster sous pression avant les autres")

	for _, region := range []string{"eu-west-rbx", "eu-west-sbg", "af-west-dkr", "us-east"} {
		_, err = Place(cs, region)
		assert.ErrorIs(t, err, ErrNoClusterInRegion, region)
	}

	// Défaut en panne, sans région : le meilleur des autres.
	cs[0].Health = healthOf(HealthDown, 0, 0, ReasonAPIUnreachable)
	got, _ = Place(cs, "")
	assert.Equal(t, "gra-2", got)

	// Installation à un cluster jamais relevé : le défaut.
	got, _ = Place([]Candidate{{ClusterID: "seul", State: StateActive, IsDefault: true, Reachable: true}}, "")
	assert.Equal(t, "seul", got)
	got, _ = Place(nil, "")
	assert.Equal(t, "", got, "registre vide : cluster de la configuration")
}

func TestPlace_TieBreaks(t *testing.T) {
	cs := []Candidate{
		{ClusterID: "b", Region: "r", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 1000, 500), Projects: 3},
		{ClusterID: "a", Region: "r", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 1000, 500), Projects: 3},
		{ClusterID: "c", Region: "r", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 1000, 500), Projects: 1},
	}
	got, _ := Place(cs, "r")
	assert.Equal(t, "c", got, "à place égale : le moins de projets")
	cs[2].Projects = 3
	got, _ = Place(cs, "r")
	assert.Equal(t, "a", got, "puis l'identifiant")
}

func TestRegions(t *testing.T) {
	rs := Regions([]Candidate{
		{ClusterID: "x", Region: "eu-west-rbx", State: StateActive, Reachable: true, Health: healthOf(HealthDown, 0, 0)},
		{ClusterID: "d", Region: "eu-west-gra", State: StateActive, IsDefault: true, Reachable: true, Health: healthOf(HealthOK, 1, 0)},
		{ClusterID: "s", Region: "eu-west-sbg", State: StateClosed, Reachable: true},
		{ClusterID: "n", State: StateActive, Reachable: true},
	})
	assert.Equal(t, []Region{{Code: "eu-west-gra", Available: true, Default: true}, {Code: "eu-west-rbx"}}, rs)
	assert.True(t, ValidSetting("traefik") && ValidSetting("lb.example.com") && ValidSetting(""))
	assert.False(t, ValidSetting("Traefik Class"))
}

func TestNormalizeProbeURL(t *testing.T) {
	for in, want := range map[string]string{"": "", "  ": "", " https://lbv.example.test/healthz ": "https://lbv.example.test/healthz",
		"https://lbv.example.test:8443/ping?x=1": "https://lbv.example.test:8443/ping?x=1",
		"HTTPS://Probe.LBV.Example.TEST/Healthz": "https://probe.lbv.example.test/Healthz",
		"https://bücher.example/healthz":         "https://xn--bcher-kva.example/healthz",
		"https://BÜCHER.example":                 "https://xn--bcher-kva.example",
		"https://xn--bcher-kva.example":          "https://xn--bcher-kva.example",
		"https://a-b.c1.example.test:65535/":     "https://a-b.c1.example.test:65535/",
		"https://lbv.example.test:00443/":        "https://lbv.example.test:443/",
		"https://203.0.113.7/healthz":            "https://203.0.113.7/healthz",
		"https://[2001:DB8::1]:8443/healthz":     "https://[2001:db8::1]:8443/healthz",
		"https://lbv.example.test/a;b,c":         "https://lbv.example.test/a;b,c",
	} {
		got, err := NormalizeProbeURL(in)
		assert.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"http://lbv.example.test", "lbv.example.test", "https://", "https://u:p@lbv.example.test", "https://lbv.example.test/#x", "ftp://x",
		// Caractères de la syntaxe de la politique de contenu dans l'hôte.
		"https://a.com;sandbox/x", "https://a.com%3Bsandbox/x", "https://a%2Ccom", "https://a,b.com", "https://*", "https://*.example.com",
		"https://a'b.com", "https://a\"b.com", "https://a&b.com", "https://a b.com", "https://a_b.example.com",
		// Ports vides, non numériques ou hors plage.
		"https://a.com:", "https://a.com:0", "https://a.com:65536", "https://a.com:99999999", "https://a.com:+80", "https://a.com:8a",
		// Noms mal formés, IP invalides, zone IPv6.
		"https://localhost", "https://-a.example.com", "https://a-.example.com", "https://a..example.com", "https://a.example.com.",
		"https://999.1.1.1", "https://1.2.3", "https://[2001:db8::1%25eth0]", "https://[fe80::1%eth0]", "https://[1.2.3.4]", "https://[::1",
		"https://" + strings.Repeat("a", 64) + ".example.com", "https:lbv.example.test",
	} {
		_, err := NormalizeProbeURL(in)
		assert.ErrorIs(t, err, ErrInvalidProbeURL, in)
	}
	o, err := ProbeOrigin(" https://Probe.Example.TEST:8443/healthz?x=1 ")
	require.NoError(t, err)
	assert.Equal(t, "https://probe.example.test:8443", o)
	_, err = ProbeOrigin("https://a.com;sandbox/x")
	assert.ErrorIs(t, err, ErrInvalidProbeURL)
}
