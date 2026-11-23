package ovhvps

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail/imapbox"
)

func TestParseRescueMail(t *testing.T) {
	const sn = "vps-0a1b2c3d.vps.ovh.net"
	fr := "Bonjour,\nVotre VPS " + sn + " a été redémarré en mode rescue.\nAdresse IP : 51.68.1.2\nNom d'utilisateur : root\nMot de passe : Xk7pQ2vLm9\nCordialement"
	c, ok := ParseRescueMail("Mode rescue de votre VPS "+sn, fr, sn)
	require.True(t, ok)
	assert.Equal(t, "root", c.Username)
	assert.Equal(t, "Xk7pQ2vLm9", c.Password)

	en := "Your VPS " + sn + " has been rebooted in rescue mode.\nUsername: root\nPassword (temporary): Zq9-Hh2kPw."
	c, ok = ParseRescueMail("Rescue mode", en, sn)
	require.True(t, ok)
	assert.Equal(t, "Zq9-Hh2kPw", c.Password, "ponctuation finale retirée")

	html := imapbox.HTMLText(`<p>VPS <b>` + sn + `</b> &ndash; rescue</p><table><tr><td>Password</td><td>:</td><td>Abc123xyz</td></tr></table>`)
	c, ok = ParseRescueMail("Rescue", html, sn)
	require.True(t, ok)
	assert.Equal(t, "Abc123xyz", c.Password)
	assert.Equal(t, "root", c.Username, "root par défaut")

	_, ok = ParseRescueMail("Mode rescue", fr, "vps-autre.vps.ovh.net")
	assert.False(t, ok, "e-mail d'un autre VPS")
	_, ok = ParseRescueMail("Facture", "Votre VPS "+sn+" : facture disponible. Mot de passe : inchangé", sn)
	assert.False(t, ok, "pas un e-mail de secours")
	_, ok = ParseRescueMail("Mode rescue", "VPS "+sn+" en rescue.", sn)
	assert.False(t, ok, "sans mot de passe")
}

func TestSetRescue(t *testing.T) {
	p, rec := fakeOrder(t, map[string]string{"PUT /vps/vps-a": ``})
	require.NoError(t, p.SetRescue(context.Background(), "vps-a", true))
	require.NoError(t, p.SetRescue(context.Background(), "vps-a", false))
	got := rec.get("PUT /vps/vps-a")
	assert.Equal(t, "rescue", got[0]["netbootMode"])
	assert.Equal(t, "local", got[1]["netbootMode"])
}

func TestIsRescueMail(t *testing.T) {
	assert.True(t, IsRescueMail("Votre VPS", "Le VPS a été redémarré en mode secours.\nMot de passe : Xk7pQ2vLm9"), "mot-clé dans le texte seulement")
	assert.True(t, IsRescueMail("Rescue mode", "Password: Xk7pQ2vLm9"))
	assert.False(t, IsRescueMail("[ABUSE] spam", "Plainte reçue pour 51.68.1.2"))
}
