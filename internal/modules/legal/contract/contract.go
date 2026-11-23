// Package contract : ce que les autres modules utilisent du module legal
// (docs/modules/legal.md). Exemple : le module compute refuse une commande de
// VPS tant que l'organisation n'a pas accepté les conditions en vigueur.
//
//	type Service struct {
//		Terms contract.TermsGate // nil : pas de contrôle
//	}
//	if s.Terms != nil {
//		if err := s.Terms.RequireAccepted(ctx, org, contract.KindTermsVPS, contract.KindAUP); err != nil {
//			return err // contract.ErrTermsNotAccepted → HTTP 409 TERMS_NOT_ACCEPTED
//		}
//	}
//
// Le câblage passe le *service.Service du module legal (il satisfait TermsGate).
package contract

import (
	"context"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/legal/domain"
)

// Documents à faire accepter.
const (
	KindTermsVPS = domain.KindTermsVPS
	KindAUP      = domain.KindAUP
)

// ErrTermsNotAccepted : une version en vigueur n'est pas acceptée par
// l'organisation (HTTP 409, code TERMS_NOT_ACCEPTED). La console lit alors
// GET /organizations/:id/legal et fait accepter les documents manquants.
var ErrTermsNotAccepted = domain.ErrTermsNotAccepted

// TermsGate : contrôle avant une action soumise aux conditions.
type TermsGate interface {
	// RequireAccepted : nil si l'organisation a accepté la version en vigueur
	// de chacun de ces documents (un document sans version publiée ne bloque
	// pas) ; ErrTermsNotAccepted sinon ; toute autre erreur : base indisponible.
	RequireAccepted(ctx context.Context, org uuid.UUID, kinds ...string) error
}
