package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// syncSpawner : exécute la tâche dans l'appel et retient son nom.
type syncSpawner struct{ names []string }

func (s *syncSpawner) Go(name string, fn func(context.Context)) {
	s.names = append(s.names, name)
	fn(context.Background())
}

// En arrière-plan, l'envoi passe par le Spawner de l'application : l'arrêt
// le voit et l'attend au lieu de couper un envoi en cours.
func TestNotifier_BackgroundSendUsesSpawner(t *testing.T) {
	n, m := newNotifier()
	n.Sync = false
	sp := &syncSpawner{}
	n.Spawner = sp
	n.WorkloadIncident(context.Background(), uuid.New(), uuid.New(), "api", utils.WorkloadOOMKilled)
	require.Len(t, m.sent, 2)
	assert.Equal(t, []string{"alert"}, sp.names)
}
