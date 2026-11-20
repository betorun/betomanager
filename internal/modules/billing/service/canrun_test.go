package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// nopPhase4 : CanRun ne doit pas l'atteindre quand l'organisation est inconnue.
type nopPhase4 struct{ repository.Phase4 }

// Une panne de la base ne doit pas ouvrir la garde : seul un projet inconnu
// laisse la décision au service des conteneurs.
func TestCanRun_FailsClosedOnLookupError(t *testing.T) {
	cases := []struct {
		name    string
		orgErr  error
		wantErr bool
	}{
		{"projet inconnu", fmt.Errorf("billing inventory: %w", domain.ErrProjectUnknown), false},
		{"base indisponible", errors.New("connection refused"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{Inventory: &fakeInventory{orgErr: tc.orgErr}, Phase4: nopPhase4{}}
			err := s.CanRun(context.Background(), uuid.New())
			if (err != nil) != tc.wantErr {
				t.Fatalf("CanRun err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
