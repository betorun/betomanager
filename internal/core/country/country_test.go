package country

import (
	"golang.org/x/text/language"

	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodes(t *testing.T) {
	assert.Len(t, codes, 249, "ISO 3166-1 alpha-2 : 249 codes attribués")
}

func TestParse(t *testing.T) {
	for in, want := range map[string]string{"GA": "GA", " ga ": "GA", "fr": "FR", "": "", "  ": ""} {
		got, err := Parse(in)
		assert.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"XX", "G", "GAB", "UK", "EU", "1A", "G-"} {
		_, err := Parse(in)
		assert.ErrorIs(t, err, ErrInvalid, in)
	}
	assert.True(t, Valid("CG"))
	assert.False(t, Valid("cg"), "Valid attend un code normalisé")
}

// Garde-fou contre une faute de frappe dans la liste : chaque code est un pays
// connu des données CLDR (golang.org/x/text). x/text ne remplace pas la liste :
// il accepte aussi des codes périmés ou réservés (UK, SU, YU, ZR, AN, XK, AC…).
func TestCodesAreCLDRCountries(t *testing.T) {
	if len(codes) != 249 {
		t.Fatalf("%d codes, 249 attendus (ISO 3166-1 alpha-2)", len(codes))
	}
	for c := range codes {
		r, err := language.ParseRegion(c)
		if err != nil || !r.IsCountry() || r.String() != c {
			t.Errorf("%s n'est pas un pays pour CLDR", c)
		}
	}
	for _, old := range []string{"UK", "SU", "YU", "ZR", "AN", "XK", "EU", "ZZ"} {
		if Valid(old) {
			t.Errorf("%s ne doit pas être accepté", old)
		}
	}
}
