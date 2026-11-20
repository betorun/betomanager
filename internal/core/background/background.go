// Package background : tâches de fond lancées par les modules (envoi
// d'alertes, sauvegarde manuelle, redimensionnement de disque…), suivies par
// l'arrêt de l'application au lieu de partir dans un « go func() » orphelin.
package background

import "context"

// Spawner lance une tâche de fond suivie : l'arrêt de l'application annule
// stop puis attend la fin des tâches (délai borné). Une panique est
// journalisée sans faire tomber le process.
//
// stop ne remplace pas le contexte de la tâche : chacune décide quoi faire
// de l'arrêt (s'interrompre proprement, ou finir dans le délai accordé).
type Spawner interface {
	Go(name string, fn func(stop context.Context))
}

// Go lance fn par sp ; sans Spawner (tests, outils), une goroutine simple
// dont le contexte d'arrêt n'est jamais annulé.
func Go(sp Spawner, name string, fn func(stop context.Context)) {
	if sp == nil {
		go fn(context.Background())
		return
	}
	sp.Go(name, fn)
}
