// Package service : cas d'usage de la facturation. Relevé de l'usage (worker)
// et estimation du coût (API). Ne dépend que de ports : inventaire et dépôt.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// Inventory : ce que les ressources réservent (adaptateur billing/inventory).
type Inventory interface {
	Reservations(ctx context.Context, since time.Time) ([]domain.Reservation, error)
	ProjectNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
	RunningWorkloads(ctx context.Context, org uuid.UUID, project *uuid.UUID) ([]domain.WorkloadRef, error)
	ProjectOrg(ctx context.Context, project uuid.UUID) (uuid.UUID, error)
	// ProjectPendingDeletion : projet en suppression programmée.
	ProjectPendingDeletion(ctx context.Context, project uuid.UUID) (bool, error)
	ProjectUsages(ctx context.Context, org uuid.UUID) ([]domain.ProjectUsage, error)
	LimitTargets(ctx context.Context, org *uuid.UUID, plan *string) ([]domain.LimitTarget, error)
}

// Réglages par défaut du relevé.
const (
	// DefaultSampleEvery : fréquence du relevé (plusieurs par heure : une
	// ressource créée puis supprimée dans l'heure est vue).
	DefaultSampleEvery = 5 * time.Minute
	// buildLookback : builds terminés relus à chaque relevé (rattrape un
	// worker arrêté quelques heures ; la clé rend la relecture sans effet).
	buildLookback = 6 * time.Hour
)

// Service : facturation.
type Service struct {
	Store     repository.Store
	Inventory Inventory
	// Phase 3 : grand livre, administration, paiements, alertes. Payments
	// nil : recharges fermées (pas de XYKPay configuré).
	Ledger        repository.Ledger
	Admin         repository.Admin
	Payments      Payments
	WebhookSecret string
	Alerts        BalanceAlerter
	// Phase 4 : grâce et suspension, budgets, documents. Containers nil :
	// rien n'est arrêté (garde seule) ; Issuer incomplet : pas de facture.
	Phase4     repository.Phase4
	Containers Containers
	// VPS nil : aucun VPS arrêté à la suspension.
	VPS     VPSSuspender
	Budgets BudgetAlerter
	Issuer  domain.Issuer
	// Phase 5 : plans. Limits nil : le plan est enregistré sans toucher aux
	// limites des projets (tests).
	Plans  repository.Plans
	Limits ProjectLimiter
	// Postpayé : réglages, créances, relances (nil : prépayé seulement).
	Postpaid  repository.Postpaid
	Reminders InvoiceReminder
	// Suppression des données après une longue suspension. Purger nil :
	// rien n'est supprimé (avertissements compris).
	Purges      repository.Purges
	Purger      DataPurger
	PurgeAlerts PurgeAlerter
	Now         func() time.Time
	Logger      *zap.SugaredLogger
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Sample : relève ce qui est réservé et l'enregistre pour l'heure en cours.
// Idempotent : relancé dans la même heure, il ne compte rien deux fois.
func (s *Service) Sample(ctx context.Context) (int, error) {
	now := s.now()
	hour := now.Truncate(time.Hour)
	res, err := s.Inventory.Reservations(ctx, now.Add(-buildLookback))
	if err != nil {
		return 0, err
	}
	catalogs := map[uuid.UUID]domain.Catalog{}
	var lines []domain.Usage
	for _, r := range res {
		c, ok := catalogs[r.OrganizationID]
		if !ok {
			if c, err = s.catalog(ctx, r.OrganizationID); err != nil {
				return 0, err
			}
			catalogs[r.OrganizationID] = c
		}
		lines = append(lines, c.Price(r, hour)...)
	}
	return len(lines), s.Store.RecordUsage(ctx, lines)
}

// catalog : grille en vigueur de la monnaie du compte. Une monnaie sans
// grille retombe sur la monnaie par défaut (journalisé : à corriger dans
// l'administration ; l'usage reste relevé).
func (s *Service) catalog(ctx context.Context, org uuid.UUID) (domain.Catalog, error) {
	currency, err := s.Store.Currency(ctx, org)
	if err != nil {
		return domain.Catalog{}, err
	}
	c, err := s.Store.ActiveCatalog(ctx, currency)
	if errors.Is(err, domain.ErrUnknownCurrency) && currency != domain.DefaultCurrency {
		s.log().Warnw("billing: no catalog for this currency, default used", "organization", org, "currency", currency)
		return s.Store.ActiveCatalog(ctx, domain.DefaultCurrency)
	}
	return c, err
}

// Run : relève à intervalle régulier jusqu'à l'arrêt (worker). Une erreur est
// journalisée ; le relevé suivant rattrape l'heure.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultSampleEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if n, err := s.Sample(ctx); err != nil {
			s.log().Warnw("billing: usage sample failed", "error", err)
		} else {
			s.log().Debugw("billing: usage sampled", "lines", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Estimate : coût estimé d'une organisation pour un mois ("" : le mois en
// cours). Rien n'est débité (phase 2).
func (s *Service) Estimate(ctx context.Context, org uuid.UUID, month string) (*domain.Estimate, error) {
	now := s.now()
	from, err := domain.ParseMonth(month, now)
	if err != nil {
		return nil, err
	}
	c, err := s.catalog(ctx, org)
	if err != nil {
		return nil, err
	}
	totals, err := s.Store.Totals(ctx, org, from, from.AddDate(0, 1, 0))
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(totals))
	seen := map[uuid.UUID]bool{}
	for _, t := range totals {
		if !seen[t.ProjectID] {
			seen[t.ProjectID] = true
			ids = append(ids, t.ProjectID)
		}
	}
	names, err := s.Inventory.ProjectNames(ctx, ids)
	if err != nil {
		return nil, err
	}

	// Projection : mois en cours seulement, au rythme de l'heure en cours.
	var last map[uuid.UUID]map[domain.Meter]domain.Milli
	remaining := domain.HoursLeft(from, now)
	if current := now.Truncate(time.Hour); !current.Before(from) && current.Before(from.AddDate(0, 1, 0)) {
		if last, err = s.Store.HourAmounts(ctx, org, current); err != nil {
			return nil, err
		}
	}
	e := c.BuildEstimate(from, totals, names, last, remaining, now)
	return &e, nil
}

// Catalog : grille en vigueur d'une monnaie ("" : la monnaie par défaut).
func (s *Service) Catalog(ctx context.Context, currency string) (*domain.CatalogView, error) {
	if currency == "" {
		currency = domain.DefaultCurrency
	}
	c, err := s.Store.ActiveCatalog(ctx, domain.NormalizeCurrency(currency))
	if err != nil {
		return nil, err
	}
	v := c.View()
	return &v, nil
}

// Catalogs : grilles en vigueur (administration).
func (s *Service) Catalogs(ctx context.Context) (*domain.CatalogsResponse, error) {
	list, err := s.Store.ActiveCatalogs(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.CatalogsResponse{Catalogs: list}, nil
}

// UpdateCatalog : nouvelle version de la grille d'une monnaie (équipe de la
// plateforme). S'applique dès le relevé suivant ; le passé garde ses prix.
func (s *Service) UpdateCatalog(ctx context.Context, u domain.CatalogUpdate, by string) (*domain.CatalogView, error) {
	c, err := domain.CatalogFromUpdate(u)
	if err != nil {
		return nil, err
	}
	saved, err := s.Store.SaveCatalog(ctx, c, by)
	if err != nil {
		return nil, err
	}
	s.log().Infow("billing: price catalog updated", "currency", saved.Currency, "version", saved.Version, "by", by)
	v := saved.View()
	return &v, nil
}

func (s *Service) log() *zap.SugaredLogger {
	if s.Logger == nil {
		return zap.NewNop().Sugar()
	}
	return s.Logger
}
