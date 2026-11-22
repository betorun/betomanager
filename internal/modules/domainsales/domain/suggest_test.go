package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitQuery(t *testing.T) {
	for _, c := range []struct{ in, label, tld string }{
		{"bombashop", "bombashop", "com"},
		{"Bomba Shop", "bombashop", "com"},
		{"https://www.bombashop.cg/", "bombashop", "cg"},
		{"mon-site.fr", "mon-site", "fr"},
	} {
		l, tl, ok := SplitQuery(c.in, "com")
		assert.True(t, ok, c.in)
		assert.Equal(t, c.label, l, c.in)
		assert.Equal(t, c.tld, tl, c.in)
	}
	for _, bad := range []string{"", "   ", "--", "@@@", "x.c"} {
		_, _, ok := SplitQuery(bad, "com")
		assert.False(t, ok, bad)
	}
}

func TestSuggestCandidates(t *testing.T) {
	got := SuggestCandidates("bombashop", "com", []string{"com", "net", ".fr", "io"})
	var names []string
	for _, c := range got {
		names = append(names, c.Domain)
	}
	assert.Equal(t, []string{"bombashop.net", "bombashop.fr", "bombashop.io",
		"getbombashop.com", "monbombashop.com", "bombashopapp.com", "bombashophq.com", "bombashoponline.com"}, names,
		"extensions d'abord, sans le nom exact, puis les variantes")
	assert.Equal(t, SuggestExtension, got[0].Kind)
	assert.Equal(t, SuggestVariant, got[len(got)-1].Kind)
	assert.LessOrEqual(t, len(SuggestCandidates("a", "com", DefaultSuggestTLDs)), MaxSuggestions)
	// Un libellé de 63 caractères : les variantes trop longues sont écartées.
	long := "a123456789b123456789c123456789d123456789e123456789f123456789abc"
	for _, c := range SuggestCandidates(long, "com", []string{"net"}) {
		assert.NotEmpty(t, NormalizeDomain(c.Domain))
	}
}
