package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

func TestClassifyMail(t *testing.T) {
	assert.Equal(t, domain.MailAbuse, ClassifyMail("[ABUSE] Plainte concernant 51.68.1.2", ""))
	assert.Equal(t, domain.MailAbuse, ClassifyMail("Notification", "Nous avons détecté du spam émis par votre serveur"))
	assert.Equal(t, domain.MailIncident, ClassifyMail("Travaux programmés sur l'infrastructure", ""))
	assert.Equal(t, domain.MailOther, ClassifyMail("Votre facture est disponible", "Montant : 6,49 €"))
}

func TestContainsToken(t *testing.T) {
	assert.True(t, containsToken("Plainte pour 51.68.1.2 ce matin", "51.68.1.2"))
	assert.True(t, containsToken("51.68.1.2", "51.68.1.2"))
	assert.False(t, containsToken("Plainte pour 151.68.1.23", "51.68.1.2"), "pas une sous-chaîne d'une autre adresse")
	assert.False(t, containsToken("serveur vps-ab12.vps.ovh.netx", "vps-ab12.vps.ovh.net"))
	assert.True(t, containsToken("(vps-ab12.vps.ovh.net)", "vps-ab12.vps.ovh.net"))
}
