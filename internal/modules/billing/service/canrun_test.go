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

// budgetPhase4 : lecture du budget en erreur.
type budgetPhase4 struct {
	repository.Phase4
	err error
}

func (p budgetPhase4) Budget(context.Context, uuid.UUID) (*repository.BudgetRow, error) {
	return nil, p.err
}

// accountLedger : lecture du compte (solde, suspension).
type accountLedger struct {
	repository.Ledger
	err error
}

func (l accountLedger) Account(context.Context, uuid.UUID) (repository.Account, error) {
	return repository.Account{}, l.err
}

var errDB = errors.New("connection refused")

// Une panne de lecture ne doit jamais ouvrir la garde : la réponse est
// ErrBillingCheckUnavailable (503, « réessayez »), cause gardée pour les
// journaux. Seul un projet inconnu laisse la décision au service des
// conteneurs.
func TestCanRun_FailsClosedOnLookupError(t *testing.T) {
	cases := []struct {
		name        string
		svc         *Service
		unavailable bool
	}{
		{"projet inconnu", &Service{Inventory: &fakeInventory{orgErr: fmt.Errorf("billing inventory: %w", domain.ErrProjectUnknown)},
			Phase4: nopPhase4{}}, false},
		{"organisation illisible", &Service{Inventory: &fakeInventory{orgErr: errDB}, Phase4: nopPhase4{}}, true},
		{"solde illisible", &Service{Inventory: &fakeInventory{}, Ledger: accountLedger{err: errDB}, Phase4: nopPhase4{}}, true},
		{"budget illisible", &Service{Inventory: &fakeInventory{}, Ledger: accountLedger{}, Phase4: budgetPhase4{err: errDB}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.svc.CanRun(context.Background(), uuid.New())
			if !tc.unavailable {
				if err != nil {
					t.Fatalf("CanRun = %v, attendu nil", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrBillingCheckUnavailable) {
				t.Fatalf("CanRun = %v, attendu ErrBillingCheckUnavailable", err)
			}
			if !errors.Is(err, errDB) {
				t.Fatalf("cause perdue pour les journaux : %v", err)
			}
		})
	}
}
