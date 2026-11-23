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
