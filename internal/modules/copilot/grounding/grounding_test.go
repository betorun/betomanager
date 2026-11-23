package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

var rules = Rules{
	Screens:       map[string]bool{"/billing": true, "/network": true, "/services": true},
	ResourcePages: []string{"/workloads/", "/builds/"},
	ToolNames:     map[string]bool{"get_workload": true, "request_egress_ip": true},
	Hosts:         map[string]bool{},
}

const wl = "3f2b6c1e-8d4a-4f6b-9a2c-1e5d7f8a9b0c"

func evidence() *Evidence {
	e := NewEvidence()
	e.Add(`{"id":"` + wl + `","name":"api","external_url":"https://api-3f2b6c.51.75.141.233.sslip.io","status":"RUNNING"}`)
	e.Add(`{"currency":"XAF","egress_ip_monthly":7300,"database_monthly":2500.5,"sizes":[{"id":"mini","monthly":1500}]}`)
	e.Add(`{"address":"203.0.113.7"}`)
	return e
}

func kinds(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Kind+":"+v.Value)
	}
	return out
}

func TestCheck_ProvenAnswerPasses(t *testing.T) {
	answer := "Votre conteneur [api](/workloads/" + wl + ") tourne. Il sort par **203.0.113.7**. " +
		"L'IP dédiée coûte 7 300 XAF par mois, une base 2 500,5 FCFA, la taille mini 1500 XAF. " +
		"Voir [Facturation](/billing) et https://api-3f2b6c.51.75.141.233.sslip.io. " +
		"Écoutez sur 0.0.0.0, pas sur 127.0.0.1."
	assert.Empty(t, Check(answer, evidence(), rules))
}

func TestCheck_Inventions(t *testing.T) {
	answer := "Le conteneur [web](/workloads/11111111-2222-3333-4444-555555555555) sort par 198.51.100.9. " +
		"Comptez 9 000 XAF par mois (ou 12 €). Allez dans [Pare-feu](/firewall) ou sur https://evil.example.com. " +
		"J'ai appelé `get_workload` puis request_egress_ip."
	got := kinds(Check(answer, evidence(), rules))
	for _, want := range []string{
		"identifier:11111111-2222-3333-4444-555555555555",
		"ip_address:198.51.100.9",
		"amount:9 000 XAF",
		"amount:12 €",
		"console_link:/firewall",
		"console_link:/workloads/11111111-2222-3333-4444-555555555555",
		"url:evil.example.com",
		"internal_tool_name:get_workload",
		"internal_tool_name:request_egress_ip",
	} {
		assert.Contains(t, got, want)
	}
}

func TestNumberForms(t *testing.T) {
	cases := map[string]string{"7 300": "7300", "7 300": "7300", "2 500,5": "2500.5", "1,500.50": "1500.5", "1.500": "1500", "05": "5", "2.50": "2.5"}
	for raw, want := range cases {
		assert.Contains(t, numberForms(raw), want, raw)
	}
}

func TestSanitize(t *testing.T) {
	answer := "Voir [Pare-feu](/firewall), IP 198.51.100.9, coût 9 000 XAF, outil `get_workload`."
	vs := Check(answer, evidence(), rules)
	out := Sanitize(answer, vs, "(non vérifié)")
	assert.NotContains(t, out, "](/firewall)")
	assert.Contains(t, out, "Pare-feu")
	assert.NotContains(t, out, "198.51.100.9")
	assert.NotContains(t, out, "9 000 XAF")
	assert.NotContains(t, out, "`get_workload`")
	assert.Empty(t, Check(out, evidence(), Rules{Screens: rules.Screens, ResourcePages: rules.ResourcePages, Hosts: rules.Hosts}))
}

func TestEvidenceRoundTrip(t *testing.T) {
	e := evidence()
	f := NewEvidence()
	f.AddTokens(e.Tokens())
	assert.Empty(t, Check("[api](/workloads/"+wl+") 7300 XAF 203.0.113.7", f, rules), "jetons gardés : preuves au tour suivant")
	assert.False(t, strings.Contains(strings.Join(e.Tokens(), " "), "RUNNING"), "jamais le texte des résultats, seulement des jetons")
}

func TestFeedbackListsItems(t *testing.T) {
	fb := Feedback([]Violation{{Kind: KindIP, Value: "198.51.100.9"}})
	assert.Contains(t, fb, "198.51.100.9")
	assert.Contains(t, fb, "VERIFICATION FAILED")
}
