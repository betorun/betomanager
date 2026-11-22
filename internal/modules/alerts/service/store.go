// Package alerts : alertes email minimales (conteneur tombé ou qui redémarre en
// boucle, build échoué). Voir docs/adr/0009-alertes-email.md.
package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/alerts/domain"
)

// Store : limitation de fréquence et destinataires.
type Store interface {
	// Claim : true si aucune alerte de ce type n'est partie pour ce sujet
	// depuis `every` ; l'envoi est alors réservé (atomique entre instances).
	Claim(ctx context.Context, subject uuid.UUID, kind string, every time.Duration) (bool, error)
	// Recipients : propriétaires et éditeurs du projet, directs ou par un
	// groupe, liaison en cours de validité, comptes actifs.
	Recipients(ctx context.Context, project uuid.UUID) ([]domain.Recipient, error)
	// ProjectName : nom affiché du projet.
	ProjectName(ctx context.Context, project uuid.UUID) (string, error)
	// OrgRecipients : owners de l'organisation (comptes actifs).
	OrgRecipients(ctx context.Context, org uuid.UUID) ([]domain.Recipient, error)
	// OrgName : nom affiché de l'organisation.
	OrgName(ctx context.Context, org uuid.UUID) (string, error)
	// Muted : parmi ces personnes, celles qui ont coupé cette catégorie.
	Muted(ctx context.Context, category string, users []uuid.UUID) (map[uuid.UUID]bool, error)
}
