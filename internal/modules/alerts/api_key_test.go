package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyCreated(t *testing.T) {
	n, m := newNotifier()
	n.APIKeyCreated(context.Background(), uuid.New(), uuid.New(), "deploy-ci", "{{.Project}} ci", "awa@example.com",
		time.Date(2027, 2, 17, 0, 0, 0, 0, time.UTC))
	require.Len(t, m.sent, 2)
	msg := m.sent[0]
	assert.Equal(t, "Nouvelle clé API pour « deploy-ci »", msg.Subject)
	assert.Contains(t, msg.Text, "« .Project ci »", "accolades retirées : pas d'injection de modèle")
	assert.Contains(t, msg.Text, "par awa@example.com")
	assert.Contains(t, msg.Text, "jusqu'au 17/02/2027")
	assert.Contains(t, msg.Text, "https://console.example.com/service-accounts")
}
