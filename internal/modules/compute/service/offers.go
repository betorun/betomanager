package service

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// Catalogs : taux de la grille de l'organisation (module billing).
type Catalogs interface {
	// Rate : grille de l'organisation (taux euro, palier, monnaie).
	Rate(ctx context.Context, org uuid.UUID) (domain.Rate, error)
}

// OfferView : une offre vue par le client (prix dans sa monnaie).
type OfferView struct {
	Code      string            `json:"code"`
	Name      string            `json:"name"`
	Monthly   int64             `json:"monthly"`
	Currency  string            `json:"currency"`
	Specs     map[string]any    `json:"specs"`
	Locations []string          `json:"locations"`
	Images    []string          `json:"images"`
	ImageName map[string]string `json:"-"`
}

// Service : VPS.
type Service struct {
	Store    Store
	Catalogs Catalogs
	// Wallet nil ou Ordering faux : commandes fermées (offres et prix seulement).
	Wallet   Wallet
	Ordering bool
	// Events nil : aucun événement temps réel.
	Events Events
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Offers : offres en vente pour une organisation (publiques et tarifées).
func (s *Service) Offers(ctx context.Context, org uuid.UUID) ([]OfferView, error) {
	rate, err := s.Catalogs.Rate(ctx, org)
	if err != nil {
		return nil, err
	}
	currency := rate.Currency
	all, err := s.Store.Offers(ctx)
	if err != nil {
		return nil, err
	}
	out := []OfferView{}
	for _, o := range all {
		price := o.Price(rate)
		if !o.Public || price == 0 || o.ProviderPlanCode == "" {
			continue
		}
		v := OfferView{Code: o.Code, Name: o.Name, Monthly: price, Currency: currency, Specs: map[string]any{},
			Locations: o.LocationList(), Images: []string{}}
		_ = json.Unmarshal(o.Specs, &v.Specs) // illisible : aucune caractéristique
		for img, name := range o.ImageMap() {
			if name != "" {
				v.Images = append(v.Images, img)
			}
		}
		sort.Strings(v.Images)
		out = append(out, v)
	}
	return out, nil
}

// OfferUpdate : réglage d'une offre par l'équipe.
type OfferUpdate struct {
	Code               string            `path:"code" json:"-"`
	Name               string            `json:"name" description:"Nom affiché (ex. VPS-1)"`
	ProviderPlanCode   string            `json:"provider_plan_code" description:"Référence de l'offre chez le fournisseur"`
	ProviderPriceCents int64             `json:"provider_price_cents" description:"Prix mensuel payé au fournisseur, en centimes d'euro, TTC si la TVA est facturée (0 : non vendue)"`
	CoefficientBP      int64             `json:"coefficient_bp" description:"Coefficient de vente en points de base (20000 = ×2)"`
	RoundTo            int64             `json:"round_to" description:"Arrondi au palier supérieur, en unités de la monnaie (ex. 500)"`
	Specs              json.RawMessage   `json:"specs,omitempty" description:"Caractéristiques affichées (vCPU, mémoire, disque, trafic)"`
	Locations          []string          `json:"locations" description:"Emplacements : gra, rbx, sbg"`
	Images             map[string]string `json:"images" description:"Système de la plateforme (ubuntu-24.04, debian-12) → image du fournisseur"`
	SnapshotPlanCode   string            `json:"snapshot_plan_code" description:"Référence de l'option snapshot chez le fournisseur (vide : non vendue)"`
	SnapshotPriceCents int64             `json:"snapshot_price_cents" description:"Prix mensuel de l'option snapshot payé au fournisseur, en centimes d'euro TTC"`
	Public             bool              `json:"public" description:"Proposée aux clients"`
	Position           int               `json:"position" description:"Ordre d'affichage"`
}

var allowedLocations = map[string]bool{domain.LocationGRA: true, domain.LocationRBX: true, domain.LocationSBG: true}
var allowedImages = map[string]bool{domain.ImageUbuntu: true, domain.ImageDebian: true}

// SaveOffer : l'équipe règle une offre (les VPS déjà commandés gardent leur
// prix jusqu'au renouvellement suivant).
func (s *Service) SaveOffer(ctx context.Context, in OfferUpdate, by string) (*domain.Offer, error) {
	code := domain.NormalizeCode(in.Code)
	if code == "" || in.CoefficientBP <= 0 || in.RoundTo <= 0 || in.ProviderPriceCents < 0 || in.SnapshotPriceCents < 0 {
		return nil, domain.ErrInvalidOffer
	}
	for _, l := range in.Locations {
		if !allowedLocations[strings.ToLower(l)] {
			return nil, domain.ErrInvalidOffer
		}
	}
	for img := range in.Images {
		if !allowedImages[img] {
			return nil, domain.ErrInvalidOffer
		}
	}
	locs, _ := json.Marshal(lower(in.Locations))
	imgs, _ := json.Marshal(in.Images)
	specs := in.Specs
	if len(specs) == 0 {
		specs = json.RawMessage(`{}`)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = code
	}
	o := &domain.Offer{Code: code, Name: name, ProviderPlanCode: strings.TrimSpace(in.ProviderPlanCode),
		ProviderPriceCents: in.ProviderPriceCents, CoefficientBP: in.CoefficientBP, RoundTo: in.RoundTo,
		SnapshotPlanCode: strings.TrimSpace(in.SnapshotPlanCode), SnapshotPriceCents: in.SnapshotPriceCents,
		Specs: specs, Locations: locs, Images: imgs, Public: in.Public, Position: in.Position, UpdatedBy: by, UpdatedAt: s.now()}
	if err := s.Store.SaveOffer(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// AdminOffers : toutes les offres, avec leur prix de vente indicatif dans la
// monnaie de référence (rate fourni par l'appelant).
// RateLister : grilles en vigueur, une par monnaie (aperçu des prix pour l'équipe).
type RateLister interface {
	Rates(ctx context.Context) ([]domain.Rate, error)
}

// OfferPrice : prix de vente d'une offre dans une monnaie (0 : invendable).
type OfferPrice struct {
	Currency        string `json:"currency"`
	Monthly         int64  `json:"monthly"`
	SnapshotMonthly int64  `json:"snapshot_monthly"`
}

// AdminOffer : offre et ses prix de vente calculés dans chaque monnaie.
type AdminOffer struct {
	domain.Offer
	Prices []OfferPrice `json:"prices"`
	// Sellable : publique, rattachée à un plan, tarifée, avec au moins un
	// emplacement et un système réglé.
	Sellable bool `json:"sellable"`
}

func (s *Service) AdminOffers(ctx context.Context) ([]AdminOffer, error) {
	list, err := s.Store.Offers(ctx)
	if err != nil {
		return nil, err
	}
	var rates []domain.Rate
	if rl, ok := s.Catalogs.(RateLister); ok && rl != nil {
		rates, _ = rl.Rates(ctx) // aperçu seulement : une erreur laisse la liste vide
	}
	out := make([]AdminOffer, 0, len(list))
	for _, o := range list {
		a := AdminOffer{Offer: o, Prices: []OfferPrice{}}
		hasImage := false
		for _, name := range o.ImageMap() {
			hasImage = hasImage || name != ""
		}
		for _, r := range rates {
			p := OfferPrice{Currency: r.Currency, Monthly: o.Price(r), SnapshotMonthly: o.SnapshotPrice(r)}
			a.Prices = append(a.Prices, p)
			if p.Monthly > 0 {
				a.Sellable = o.Public && o.ProviderPlanCode != "" && len(o.LocationList()) > 0 && hasImage
			}
		}
		out = append(out, a)
	}
	return out, nil
}

func lower(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(v)))
	}
	return out
}
