package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// fleetLimit : VPS montés dans la vue de l'équipe (les plus récents).
const fleetLimit = 500

// FleetVPS : un VPS vu par l'équipe (fournisseur compris, jamais montré au client).
type FleetVPS struct {
	ID               uuid.UUID `json:"id"`
	OrganizationID   uuid.UUID `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	ProjectID        uuid.UUID `json:"project_id"`
	Name             string    `json:"name"`
	Offer            string    `json:"offer"`
	Location         string    `json:"location"`
	Image            string    `json:"image"`
	State            string    `json:"state"`
	IPv4             string    `json:"ipv4"`
	IPv6             string    `json:"ipv6"`
	Provider         string    `json:"provider"`
	ProviderID       string    `json:"provider_id"`
	CreatedAt        time.Time `json:"created_at"`
	// Observed : dernier relevé chez le fournisseur (absent : jamais vu).
	Observed *domain.ProviderMachine `json:"observed,omitempty"`
	// Drift : écart relevé (domain.Drift*), vide si aucun.
	Drift string `json:"drift"`
	// Rescue / RescueSecret : mode secours ; « missing » = e-mail du fournisseur jamais arrivé.
	Rescue       string `json:"rescue"`
	RescueSecret string `json:"rescue_secret"`
	Operation    string `json:"operation"`
	// Réseau : blocage anti-spam (déblocage par l'équipe), port 25 ouvert.
	SpamBlockedAt *time.Time `json:"spam_blocked_at,omitempty"`
	SMTPAllowed   bool       `json:"smtp_allowed"`
}

// Fleet : vue « VPS » du back-office.
type Fleet struct {
	VPS     []FleetVPS               `json:"vps"`
	Orphans []domain.ProviderMachine `json:"orphans"`
	// Runs : dernières passes de réconciliation, la plus récente d'abord.
	Runs []domain.ReconcileRun `json:"runs"`
}

// Fleet : VPS des clients rapprochés du dernier relevé du fournisseur.
func (s *Service) Fleet(ctx context.Context) (*Fleet, error) {
	list, err := s.Store.ListVPS(ctx, fleetLimit)
	if err != nil {
		return nil, err
	}
	machines, err := s.Store.Machines(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := s.Store.Runs(ctx, 10)
	if err != nil {
		return nil, err
	}
	orgIDs := make([]uuid.UUID, 0, len(list))
	seenOrg := map[uuid.UUID]bool{}
	for _, v := range list {
		if !seenOrg[v.OrganizationID] {
			seenOrg[v.OrganizationID] = true
			orgIDs = append(orgIDs, v.OrganizationID)
		}
	}
	names, err := s.Store.OrganizationNames(ctx, orgIDs)
	if err != nil {
		return nil, err
	}

	type key struct{ provider, id string }
	byKey := make(map[key]*domain.ProviderMachine, len(machines))
	out := &Fleet{VPS: make([]FleetVPS, 0, len(list)), Orphans: []domain.ProviderMachine{}, Runs: runs}
	for i := range machines {
		m := &machines[i]
		if m.Drift == domain.DriftOrphan {
			out.Orphans = append(out.Orphans, *m)
			continue
		}
		byKey[key{m.Provider, m.ProviderID}] = m
	}
	// Absent du relevé : seulement si une passe a réussi depuis sa création.
	var lastOK *time.Time
	for _, r := range runs {
		if r.Error == "" && r.FinishedAt != nil {
			lastOK = &r.StartedAt
			break
		}
	}
	for _, v := range list {
		f := FleetVPS{ID: v.ID, OrganizationID: v.OrganizationID, OrganizationName: names[v.OrganizationID], ProjectID: v.ProjectID,
			Name: v.Name, Offer: v.Offer, Location: v.Location, Image: v.Image, State: v.State, IPv4: v.IPv4, IPv6: v.IPv6,
			Provider: v.Provider, ProviderID: v.ProviderID, CreatedAt: v.CreatedAt,
			Rescue: v.Rescue, RescueSecret: v.RescueSecret, Operation: v.Operation,
			SpamBlockedAt: v.SpamBlockedAt, SMTPAllowed: v.SMTPAllowed}
		if m, ok := byKey[key{v.Provider, v.ProviderID}]; ok && v.ProviderID != "" {
			f.Observed = m
			f.Drift = m.Drift
		} else if v.ProviderID != "" && expectedAtProvider(v.State) && lastOK != nil && v.CreatedAt.Before(*lastOK) {
			f.Drift = domain.DriftMissing
		}
		out.VPS = append(out.VPS, f)
	}
	return out, nil
}
