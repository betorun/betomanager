// Package activities : activités Temporal du stockage objet (ADR 0015).
package activities

import (
	"context"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
)

// ProjectPurger : efface identités, contenu et buckets d'un projet.
type ProjectPurger interface {
	PurgeProject(ctx context.Context, projectID uuid.UUID) error
}

// StorageActivities : appelées par la suppression d'un projet.
type StorageActivities struct {
	// Purger : nil quand le stockage n'est pas configuré (rien à effacer).
	Purger ProjectPurger
}

// DeleteProjectBuckets : buckets du projet effacés avant ses lignes (la
// cascade emporterait leurs lignes, pas leurs données ni leurs identités).
// Idempotent : rejouée après un échec, elle reprend où elle s'était arrêtée.
func (a *StorageActivities) DeleteProjectBuckets(ctx context.Context, projectID string) error {
	id, err := uuid.Parse(projectID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid project id", "InvalidInput", err)
	}
	if a == nil || a.Purger == nil {
		return nil
	}
	return a.Purger.PurgeProject(ctx, id)
}
