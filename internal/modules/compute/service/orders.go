package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// DefaultProvider : fournisseur des nouveaux VPS.
const DefaultProvider = domain.ProviderOVH

// OrderRequest : commande d'un VPS par un client.
type OrderRequest struct {
	OrganizationID string `path:"id" json:"-"`
	ProjectID      string `json:"project_id" binding:"required" description:"Projet qui recevra le VPS"`
	Name           string `json:"name" binding:"required" description:"Nom du VPS (lettres minuscules, chiffres, tirets ; 63 au plus)"`
	Offer          string `json:"offer" binding:"required" description:"Code de l'offre (ex. VPS-1)"`
	Location       string `json:"location" binding:"required" description:"Emplacement (gra, rbx, sbg)"`
	Image          string `json:"image" binding:"required" description:"Système (ubuntu-24.04, debian-12)"`
	SSHPublicKey   string `json:"ssh_public_key" binding:"required" description:"Clé publique SSH installée sur le VPS (aucun mot de passe n'est créé)"`
	// MonthlyPrice : prix annoncé au client ; refusé s'il a changé depuis.
	MonthlyPrice int64 `json:"monthly_price" binding:"required" description:"Prix mensuel affiché au client, dans la monnaie de l'organisation"`
}

// Order : débite le premier mois et dépose la demande de création, ensemble.
func (s *Service) Order(ctx context.Context, req OrderRequest, by string) (*domain.VPS, error) {
	if !s.Ordering || s.Wallet == nil {
		return nil, domain.ErrUnavailable
	}
	org, err := uuid.Parse(req.OrganizationID)
	if err != nil {
		return nil, domain.ErrProjectNotInOrganization
	}
	project, err := uuid.Parse(req.ProjectID)
	if err != nil {
		return nil, domain.ErrProjectNotInOrganization
	}
	if owner, err := s.Store.ProjectOrganization(ctx, project); err != nil {
		return nil, err
	} else if owner != org {
		return nil, domain.ErrProjectNotInOrganization
	}
	name := strings.TrimSpace(req.Name)
	if !domain.ValidName(name) {
		return nil, domain.ErrInvalidName
	}
	key, err := domain.ParseSSHKey(req.SSHPublicKey)
	if err != nil {
		return nil, err
	}
	offer, err := s.Store.Offer(ctx, req.Offer)
	if err != nil {
		return nil, err
	}
	location, image := strings.ToLower(strings.TrimSpace(req.Location)), strings.ToLower(strings.TrimSpace(req.Image))
	rate, currency, err := s.Catalogs.EURRate(ctx, org)
	if err != nil {
		return nil, err
	}
	if !offer.Sellable(rate) || !offer.Sells(location, image) {
		return nil, domain.ErrNotSold
	}
	price := offer.MonthlyPrice(rate)
	if req.MonthlyPrice != price {
		return nil, domain.ErrPriceChanged
	}

	now := s.now()
	id := uuid.New()
	until := domain.NextPeriod(now)
	period := domain.Period(id, now)
	amount := price * domain.MilliPerUnit
	v := &domain.VPS{
		ID: id, OrganizationID: org, ProjectID: project, Name: name, Offer: offer.Code, Location: location, Image: image,
		Provider: DefaultProvider, State: domain.StatePending, SSHKeyFingerprint: key.Fingerprint, Version: 1,
		PaidUntil: &until, MonthlyPrice: price, Currency: currency, CreatedAt: now, UpdatedAt: now,
	}
	payload, err := json.Marshal(domain.CreatePayload{SSHPublicKey: key.Line, Period: period, AmountMilli: amount})
	if err != nil {
		return nil, err
	}
	if err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := s.Store.CreateVPSTx(tx, v); err != nil {
			return err
		}
		if _, err := s.Wallet.ChargeComputeTx(ctx, tx, org, billingDomain.Milli(amount), period,
			"VPS "+name+" ("+offer.Name+"), 1 mois", by); err != nil {
			return err
		}
		return s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: org, VPSID: &id, Kind: domain.KindCreate,
			Payload: payload, CreatedBy: by})
	}); err != nil {
		return nil, err
	}
	s.Wallet.AfterComputeCharge(ctx, org)
	return v, nil
}

// List / Get : VPS d'une organisation.
func (s *Service) List(ctx context.Context, org uuid.UUID) ([]domain.VPS, error) {
	return s.Store.OrganizationVPS(ctx, org)
}

func (s *Service) Get(ctx context.Context, org, id uuid.UUID) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.OrganizationID != org || v.State == domain.StateDeleted {
		return nil, domain.ErrVPSNotFound
	}
	return v, nil
}

// ── Renouvellement ──────────────────────────────────────────────────────

// Réglages du renouvellement (worker).
const (
	DefaultRenewEvery = 15 * time.Minute
	renewBatch        = 50
)

// errUnpaid : solde insuffisant au renouvellement (annule la transaction).
var errUnpaid = errors.New("compute: renewal unpaid")

// RenewDue : débite le mois suivant des VPS arrivés au bout du mois payé.
// Payé : le mois commence à la fin du précédent (ou maintenant pour un VPS
// suspendu, qui est relancé). Non payé : suspendu et arrêté, retenté à
// chaque passage. Prix : celui de la grille au moment du renouvellement.
func (s *Service) RenewDue(ctx context.Context) (renewed, suspended int, err error) {
	if s.Wallet == nil {
		return 0, 0, nil
	}
	now := s.now()
	due, err := s.Store.DueVPS(ctx, now, renewBatch)
	if err != nil {
		return 0, 0, err
	}
	// Une erreur sur un VPS n'arrête pas les autres ; la première est rendue.
	var first error
	for _, v := range due {
		ok, susp, err := s.renew(ctx, v, now)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if ok {
			renewed++
		}
		if susp {
			suspended++
		}
	}
	return renewed, suspended, first
}

func (s *Service) renew(ctx context.Context, v domain.VPS, now time.Time) (renewed, suspended bool, err error) {
	price := v.MonthlyPrice
	currency := v.Currency
	if o, err := s.Store.Offer(ctx, v.Offer); err == nil {
		if rate, cur, err := s.Catalogs.EURRate(ctx, v.OrganizationID); err == nil {
			if p := o.MonthlyPrice(rate); p > 0 {
				price, currency = p, cur
			}
		}
	}
	if price <= 0 {
		return false, false, nil // jamais tarifé : rien à débiter
	}
	if v.State == domain.StateSuspended && v.FailureCode != domain.FailureUnpaid {
		return false, false, nil // suspendu par l'équipe (abus…) : pas relancé par un paiement
	}
	from := *v.PaidUntil
	if v.State == domain.StateSuspended {
		from = now // les mois de suspension ne sont pas facturés
	}
	period := domain.Period(v.ID, from)
	err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if _, err := s.Wallet.ChargeComputeTx(ctx, tx, v.OrganizationID, billingDomain.Milli(price*domain.MilliPerUnit), period,
			"VPS "+v.Name+" ("+v.Offer+"), 1 mois", "billing"); err != nil {
			if errors.Is(err, billingDomain.ErrInsufficientFunds) {
				return errUnpaid
			}
			return err
		}
		if err := s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"paid_until": domain.NextPeriod(from),
			"monthly_price": price, "currency": currency, "failure_code": "", "updated_at": now}); err != nil {
			return err
		}
		if v.State == domain.StateSuspended {
			return s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: domain.KindStart, CreatedBy: "billing"})
		}
		return nil
	})
	switch {
	case err == nil:
		s.Wallet.AfterComputeCharge(ctx, v.OrganizationID)
		return true, false, nil
	case errors.Is(err, errUnpaid):
		if v.State == domain.StateSuspended {
			return false, false, nil
		}
		err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
			if err := s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"state": domain.StateSuspended,
				"failure_code": domain.FailureUnpaid, "updated_at": now}); err != nil {
				return err
			}
			return s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: domain.KindStop, CreatedBy: "billing"})
		})
		return false, err == nil, err
	default:
		return false, false, err
	}
}
