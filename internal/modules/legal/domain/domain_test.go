package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNeedsAcceptance(t *testing.T) {
	assert.False(t, NeedsAcceptance(0, 0), "rien de publié : rien à accepter")
	assert.True(t, NeedsAcceptance(1, 0), "jamais accepté")
	assert.False(t, NeedsAcceptance(2, 2))
	assert.True(t, NeedsAcceptance(3, 2), "nouvelle version publiée")
}

func TestContentValidate(t *testing.T) {
	ok := Content{TitleFR: " Titre ", TitleEN: "Title", BodyFR: "Texte", BodyEN: "Text"}
	assert.NoError(t, ok.Validate())
	assert.Equal(t, "Titre", ok.Normalize().TitleFR)
	assert.ErrorIs(t, Content{TitleFR: "Titre", BodyFR: "Texte", BodyEN: "Text"}.Validate(), ErrInvalidDocument)
	assert.ErrorIs(t, Content{TitleFR: "t", TitleEN: "t", BodyFR: "b", BodyEN: "  "}.Validate(), ErrInvalidDocument)
	long := ok
	long.BodyEN = strings.Repeat("x", MaxBody+1)
	assert.ErrorIs(t, long.Validate(), ErrDocumentTooLong)
	assert.True(t, Content{TitleFR: "  "}.Empty())
	assert.False(t, ok.Empty())
}

func TestKindsAndLang(t *testing.T) {
	assert.True(t, ValidKind(KindTermsVPS))
	assert.True(t, ValidKind(KindAUP))
	assert.False(t, ValidKind("cgu"))
	l, ok := ParseLang("", LangEN)
	assert.True(t, ok)
	assert.Equal(t, LangEN, l)
	l, ok = ParseLang("FR", LangEN)
	assert.True(t, ok)
	assert.Equal(t, LangFR, l)
	_, ok = ParseLang("de", LangFR)
	assert.False(t, ok)
	d := &Document{TitleFR: "fr", TitleEN: "en", BodyFR: "corps", BodyEN: "body"}
	title, body := d.Text(LangEN)
	assert.Equal(t, []string{"en", "body"}, []string{title, body})
	title, _ = d.Text("xx")
	assert.Equal(t, "fr", title)
}
