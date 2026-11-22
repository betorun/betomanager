// Package service : revente de noms de domaine. Recherche et prix (en
// monnaie du compte), achat payé par le solde, commande chez le registraire
// en tâche de fond, renouvellement, branchement sur un conteneur avec DNS
// écrits automatiquement.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/domains/registrar"
	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/domain"
	"github.com/thekrauss/kubemanager/internal/modules/domainsales/repository"
	workloadsDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// Wallet : débit et remboursement (module billing).
type Wallet interface {
	AccountCatalog(ctx context.Context, org uuid.UUID) (billingDomain.Catalog, error)
	ChargePurchase(ctx context.Context, org uuid.UUID, amount billingDomain.Milli, reference, description, by string) error
	RefundPurchase(ctx context.Context, org uuid.UUID, amount billingDomain.Milli, reference, description string) error
}

// Workloads : domaines des conteneurs (module workloads).
type Workloads interface {
	WorkloadOrg(ctx context.Context, workload uuid.UUID) (uuid.UUID, error)
	AddDomain(ctx context.Context, workload uuid.UUID, hostname, path string) (*workloadsDomain.DomainResponse, error)
	VerifyDomain(ctx context.Context, workload, id uuid.UUID) (*workloadsDomain.DomainResponse, error)
}

// Réglages du traitement en tâche de fond.
const (
	DefaultEvery = time.Minute
	batch        = 50
)

// Service : revente de domaines.
type Service struct {
	Store     repository.Store
	Registrar registrar.Registrar // nil : revente fermée
	Wallet    Wallet
	Workloads Workloads
	// Ordering : achats réels ouverts (sinon recherche et prix seulement).
	Ordering bool
	// SuggestTLDs : extensions proposées en suggestion (nil : domain.DefaultSuggestTLDs).
	SuggestTLDs []string
	// cache : disponibilités récentes des suggestions.
	cache  availabilityCache
	Now    func() time.Time
	Logger *zap.SugaredLogger
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *zap.SugaredLogger {
	if s.Logger == nil {
		return zap.NewNop().Sugar()
	}
	return s.Logger
}

// hide : une erreur du registraire ne sort jamais telle quelle (marque
// blanche) : elle est journalisée, le client reçoit ErrUnavailable.
func (s *Service) hide(op string, err error) error {
	s.log().Warnw("domains: registrar call failed", "op", op, "error", err)
	return domain.ErrUnavailable
}

// quote : prix de vente d'un nom (relu chez le registraire).
func (s *Service) quote(ctx context.Context, org uuid.UUID, name string) (*domain.Quote, *registrar.Availability, error) {
	if s.Registrar == nil {
		return nil, nil, domain.ErrSalesDisabled
	}
	c, err := s.Wallet.AccountCatalog(ctx, org)
	if err != nil {
		return nil, nil, err
	}
	q := &domain.Quote{Domain: name, Currency: c.Currency, Ordering: s.Ordering}
	held, err := s.Store.Held(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if held {
		q.Reason = "taken"
		return q, nil, nil
	}
	a, err := s.Registrar.Check(ctx, name)
	if err != nil {
		return nil, nil, s.hide("check", err)
	}
	priceQuote(q, c, a)
	return q, a, nil
}

// priceQuote : vendable ou non, et à quel prix (même règle pour la recherche,
// les suggestions et l'achat).
func priceQuote(q *domain.Quote, c billingDomain.Catalog, a *registrar.Availability) {
	switch {
	case !a.Available:
		q.Reason = "taken"
	case a.Premium:
		q.Reason = "premium"
	case a.Currency != "EUR" || c.DomainPrice(a.PriceCents) == 0:
		q.Reason = "unpriced"
	default:
		q.Available = true
		q.Price = c.DomainPrice(a.PriceCents)
		q.RenewPrice = c.DomainPrice(a.RenewCents)
	}
}

// Search : disponibilité et prix d'un nom.
func (s *Service) Search(ctx context.Context, org uuid.UUID, raw string) (*domain.Quote, error) {
	name := domain.NormalizeDomain(raw)
	if name == "" {
		return nil, domain.ErrInvalidDomain
	}
	q, _, err := s.quote(ctx, org, name)
	return q, err
}

// List : domaines de l'organisation.
func (s *Service) List(ctx context.Context, org uuid.UUID) (*domain.OrdersResponse, error) {
	rows, err := s.Store.List(ctx, org)
	if err != nil {
		return nil, err
	}
	out := &domain.OrdersResponse{Domains: make([]domain.OrderView, 0, len(rows)), Ordering: s.Ordering && s.Registrar != nil}
	for _, o := range rows {
		out.Domains = append(out.Domains, o.View())
	}
	return out, nil
}

// Buy : le client paie (débit de son solde), la commande part en tâche de
// fond. Le prix annoncé doit être encore celui du moment.
func (s *Service) Buy(ctx context.Context, org uuid.UUID, req domain.BuyRequest, by string) (*domain.OrderView, error) {
	if !s.Ordering || s.Registrar == nil {
		return nil, domain.ErrSalesDisabled
	}
	name := domain.NormalizeDomain(req.Domain)
	if name == "" {
		return nil, domain.ErrInvalidDomain
	}
	owner, err := req.Owner.Normalize()
	if err != nil {
		return nil, err
	}
	q, a, err := s.quote(ctx, org, name)
	if err != nil {
		return nil, err
	}
	if !q.Available {
		if q.Reason == "premium" {
			return nil, domain.ErrPremium
		}
		if q.Reason == "unpriced" {
			return nil, domain.ErrNoPrice
		}
		return nil, domain.ErrNotAvailable
	}
	if req.Price != q.Price {
		return nil, domain.ErrPriceChanged
	}
	contact, err := json.Marshal(owner)
	if err != nil {
		return nil, err
	}
	now := s.now()
	o := &domain.Order{ID: uuid.New(), OrganizationID: org, Domain: name, Status: domain.StatusOrdering, Currency: q.Currency,
		Price: int64(billingDomain.FromUnits(q.Price)), RenewPrice: int64(billingDomain.FromUnits(q.RenewPrice)),
		RegistrarCents: a.PriceCents, RenewCents: a.RenewCents, RegistrarCurrency: a.Currency, Contact: contact,
		AutoRenew: req.AutoRenew, CreatedBy: by, CreatedAt: now, UpdatedAt: now}
	// Payé d'abord : la commande n'est créée que si le débit passe. Le
	// débit est idempotent (référence de la commande).
	if err := s.Wallet.ChargePurchase(ctx, org, billingDomain.Milli(o.Price), domain.PurchaseReference(o.ID), "Domaine "+name, by); err != nil {
		return nil, err
	}
	if err := s.Store.Create(ctx, o); err != nil {
		// Le nom vient d'être pris (index unique) : rembourser.
		if rerr := s.Wallet.RefundPurchase(ctx, org, billingDomain.Milli(o.Price), domain.RefundReference(o.ID), "Domaine "+name); rerr != nil {
			s.log().Errorw("domains: refund after failed order creation", "order", o.ID, "error", rerr)
		}
		return nil, err
	}
	s.log().Infow("domains: order paid", "organization", org, "domain", name, "order", o.ID)
	v := o.View()
	return &v, nil
}

// Process : commandes payées à passer, commandes en cours à suivre,
// renouvellements à débiter (worker).
func (s *Service) Process(ctx context.Context) {
	if s.Registrar == nil {
		return
	}
	if rows, err := s.Store.ByStatus(ctx, domain.StatusOrdering, batch); err == nil {
		for i := range rows {
			s.place(ctx, &rows[i])
		}
	}
	if rows, err := s.Store.ByStatus(ctx, domain.StatusPlaced, batch); err == nil {
		for i := range rows {
			s.follow(ctx, &rows[i])
		}
	}
	s.renew(ctx)
}

// Run : boucle du worker.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s.Process(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// fail : commande non obtenue ; le client est remboursé.
func (s *Service) fail(ctx context.Context, o *domain.Order, reason string) {
	if err := s.Wallet.RefundPurchase(ctx, o.OrganizationID, billingDomain.Milli(o.Price), domain.RefundReference(o.ID), "Domaine "+o.Domain); err != nil {
		s.log().Errorw("domains: refund failed", "order", o.ID, "error", err)
		return // nouvel essai au passage suivant
	}
	_ = s.Store.Update(ctx, o.ID, map[string]any{"status": domain.StatusFailed, "error": reason})
	s.log().Warnw("domains: order failed, refunded", "order", o.ID, "domain", o.Domain, "reason", reason)
}

// place : commande réelle chez le registraire. Le prix est revérifié : s'il
// a monté au point de dépasser ce que le client a payé, rien n'est commandé.
func (s *Service) place(ctx context.Context, o *domain.Order) {
	c, err := s.Wallet.AccountCatalog(ctx, o.OrganizationID)
	if err != nil {
		return
	}
	a, err := s.Registrar.Check(ctx, o.Domain)
	if err != nil {
		return // registraire injoignable : au passage suivant
	}
	if !a.Available || a.Premium {
		s.fail(ctx, o, "no longer available")
		return
	}
	if billingDomain.FromUnits(c.DomainPrice(a.PriceCents)) > billingDomain.Milli(o.Price) {
		s.fail(ctx, o, "registrar price increased")
		return
	}
	var owner domain.Contact
	_ = json.Unmarshal(o.Contact, &owner)
	res, err := s.Registrar.Register(ctx, registrar.Order{Domain: o.Domain, Years: 1, Owner: registrar.Contact{
		FirstName: owner.FirstName, LastName: owner.LastName, Organization: owner.Organization, Email: owner.Email, Phone: owner.Phone,
		Address: owner.Address, City: owner.City, PostalCode: owner.PostalCode, Country: owner.Country}})
	if err != nil {
		if errors.Is(err, registrar.ErrOrderingDisabled) {
			return // achats fermés entre-temps : la commande attend
		}
		// Refus avant paiement chez le registraire : on ne sait pas toujours
		// si la commande est partie. L'équipe vérifie avant tout remboursement
		// manuel ; ici, échec franc et remboursement.
		s.fail(ctx, o, "registrar: "+err.Error())
		return
	}
	_ = s.Store.Update(ctx, o.ID, map[string]any{"status": domain.StatusPlaced, "registrar_order": res.OrderID})
	s.log().Infow("domains: order placed", "order", o.ID, "domain", o.Domain, "registrar_order", res.OrderID)
}

// follow : commande en cours chez le registraire.
func (s *Service) follow(ctx context.Context, o *domain.Order) {
	st, err := s.Registrar.OrderStatus(ctx, o.RegistrarOrder)
	if err != nil {
		return
	}
	switch st {
	case registrar.OrderFailed:
		s.fail(ctx, o, "registrar order not paid or cancelled")
	case registrar.OrderDelivered:
		info, err := s.Registrar.Info(ctx, o.Domain)
		if err != nil {
			return // le domaine apparaît parfois un peu après la livraison
		}
		_ = s.Registrar.SetAutoRenew(ctx, o.Domain, o.AutoRenew)
		now := s.now()
		_ = s.Store.Update(ctx, o.ID, map[string]any{"status": domain.StatusActive, "expires_at": info.Expiration,
			"paid_until": info.Expiration, "active_at": now})
		s.log().Infow("domains: domain active", "order", o.ID, "domain", o.Domain, "expires", info.Expiration)
	}
}

// renew : 30 jours avant l'expiration, le renouvellement est débité ; sans
// solde, le registraire est prévenu de ne pas renouveler (le domaine expirera).
// L'expiration est relue chez le registraire (renouvelé : elle avance).
func (s *Service) renew(ctx context.Context) {
	rows, err := s.Store.RenewDue(ctx, s.now(), domain.RenewBefore, batch)
	if err != nil {
		return
	}
	for _, o := range rows {
		if info, err := s.Registrar.Info(ctx, o.Domain); err == nil && o.ExpiresAt != nil && info.Expiration.After(*o.ExpiresAt) {
			// Déjà renouvelé chez le registraire (période suivante payée ?) :
			// l'expiration avance, le débit attendra la fenêtre suivante.
			_ = s.Store.Update(ctx, o.ID, map[string]any{"expires_at": info.Expiration})
			continue
		}
		until := o.ExpiresAt.AddDate(1, 0, 0)
		err := s.Wallet.ChargePurchase(ctx, o.OrganizationID, billingDomain.Milli(o.RenewPrice), domain.RenewReference(o.ID, until),
			"Renouvellement "+o.Domain, "billing")
		if errors.Is(err, billingDomain.ErrInsufficientFunds) {
			if o.RenewFailedAt == nil {
				_ = s.Registrar.SetAutoRenew(ctx, o.Domain, false)
				_ = s.Store.Update(ctx, o.ID, map[string]any{"renew_failed_at": s.now()})
				s.log().Warnw("domains: renewal not paid, auto-renew off at the registrar", "order", o.ID, "domain", o.Domain)
			}
			continue
		}
		if err != nil {
			continue
		}
		_ = s.Registrar.SetAutoRenew(ctx, o.Domain, true)
		_ = s.Store.Update(ctx, o.ID, map[string]any{"paid_until": until, "renew_failed_at": nil})
	}
}

// SetAutoRenew : le client active ou coupe le renouvellement.
func (s *Service) SetAutoRenew(ctx context.Context, org, id uuid.UUID, on bool) (*domain.OrderView, error) {
	o, err := s.Store.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if o.Status == domain.StatusActive && s.Registrar != nil {
		// Coupé : le registraire laissera expirer ; réactivé : le débit
		// annuel décidera (sans solde, il recoupera).
		if err := s.Registrar.SetAutoRenew(ctx, o.Domain, on); err != nil {
			return nil, s.hide("auto-renew", err)
		}
	}
	if err := s.Store.Update(ctx, o.ID, map[string]any{"auto_renew": on, "renew_failed_at": nil}); err != nil {
		return nil, err
	}
	o.AutoRenew, o.RenewFailedAt = on, nil
	v := o.View()
	return &v, nil
}

// Attach : branche le domaine (et www si demandé) sur un conteneur de
// l'organisation, et écrit lui-même les enregistrements DNS dans la zone :
// le client n'a rien à configurer.
func (s *Service) Attach(ctx context.Context, org, id uuid.UUID, req domain.AttachRequest) (*domain.AttachResponse, error) {
	o, err := s.Store.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if o.Status != domain.StatusActive {
		return nil, domain.ErrNotActive
	}
	workload, err := uuid.Parse(req.WorkloadID)
	if err != nil {
		return nil, domain.ErrOrderNotFound
	}
	if wOrg, err := s.Workloads.WorkloadOrg(ctx, workload); err != nil || wOrg != org {
		return nil, domain.ErrOrderNotFound
	}
	hosts := []string{o.Domain}
	if req.WWW {
		hosts = append(hosts, "www."+o.Domain)
	}
	out := &domain.AttachResponse{Hostnames: hosts, Verified: true}
	for _, h := range hosts {
		d, err := s.Workloads.AddDomain(ctx, workload, h, "/")
		if err != nil {
			return nil, err
		}
		var records []registrar.Record
		for _, r := range d.Records {
			if r.Alternative {
				continue
			}
			value := r.Value
			if r.Type == workloadsDomain.RecordCNAME && net.ParseIP(value) == nil && value != "" && value[len(value)-1] != '.' {
				value += "."
			}
			records = append(records, registrar.Record{Type: r.Type, Name: r.Name, Value: value, TTL: 300})
		}
		if err := s.Registrar.SetRecords(ctx, o.Domain, records); err != nil {
			return nil, s.hide("dns", err)
		}
		if v, err := s.Workloads.VerifyDomain(ctx, workload, d.ID); err != nil || v == nil || !v.Pointing {
			out.Verified = false // la propagation DNS prend quelques minutes ; « Vérifier » la relance
		}
	}
	return out, nil
}
