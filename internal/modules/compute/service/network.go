package service

import (
	"context"
	"encoding/json"
	"net"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// ReverseRequest : DNS inverse d'une adresse du VPS.
type ReverseRequest struct {
	Family  string `json:"family" binding:"required" description:"Adresse : v4 ou v6"`
	Reverse string `json:"reverse" description:"Nom d'hôte complet qui pointe déjà vers l'adresse (vide : retirer)"`
}

// Resolve : adresses d'un nom (DNS public ; remplaçable dans les tests).
var Resolve = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// SetReverse : vérifié avant toute demande : le nom doit déjà pointer vers
// l'adresse (le fournisseur le refuserait sinon).
func (s *Service) SetReverse(ctx context.Context, org, id uuid.UUID, req ReverseRequest, by string) (*domain.VPS, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if err := ready(v); err != nil {
		return nil, err
	}
	ip := v.IPv4
	if req.Family == "v6" {
		ip = v.IPv6
	} else if req.Family != "v4" {
		return nil, domain.ErrNoAddress
	}
	if ip == "" {
		return nil, domain.ErrNoAddress
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Reverse)), ".")
	if host != "" {
		if !domain.ValidReverse(host) {
			return nil, domain.ErrInvalidReverse
		}
		addrs, err := Resolve(ctx, host)
		if err != nil || !containsIP(addrs, ip) {
			return nil, domain.ErrReverseNotPointing
		}
	}
	payload, _ := json.Marshal(domain.ReversePayload{Family: req.Family, Reverse: host})
	return s.enqueueOp(ctx, v, domain.KindReverse, domain.OpSettingReverse, payload, by, nil, nil)
}

func containsIP(addrs []string, ip string) bool {
	want := net.ParseIP(ip)
	for _, a := range addrs {
		if got := net.ParseIP(a); got != nil && want != nil && got.Equal(want) {
			return true
		}
	}
	return false
}

// TerminateRequest : confirmation par le nom du VPS.
type TerminateRequest struct {
	ConfirmName string `json:"confirm_name" binding:"required" description:"Nom du VPS, pour confirmer la résiliation"`
}

// Terminate : VPS coupé tout de suite côté plateforme (résilié, plus
// renouvelé, son nom libéré) ; chez le fournisseur, arrêt puis suppression à
// l'expiration du mois. Aucun remboursement du mois entamé.
func (s *Service) Terminate(ctx context.Context, org, id uuid.UUID, req TerminateRequest, by string) (*domain.VPS, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.ConfirmName) != v.Name {
		return nil, domain.ErrInvalidName
	}
	return s.terminate(ctx, v, by)
}

func (s *Service) terminate(ctx context.Context, v *domain.VPS, by string) (*domain.VPS, error) {
	if v.State == domain.StateDeleted {
		return v, nil
	}
	if v.Operation != "" {
		return nil, domain.ErrBusy
	}
	now := s.now()
	if err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if v.ProviderID != "" {
			if err := s.Store.EnqueueTx(tx, &domain.Request{OrganizationID: v.OrganizationID, VPSID: &v.ID, Kind: domain.KindTerminate, CreatedBy: by}); err != nil {
				return err
			}
		}
		return s.Store.UpdateVPSTx(tx, v.ID, map[string]any{"state": domain.StateDeleted, "terminated_at": now,
			"operation": op(v.ProviderID != "", domain.OpTerminating), "snapshot_option": "", "rescue": "", "rescue_secret": "", "updated_at": now})
	}); err != nil {
		return nil, err
	}
	Notify(ctx, s.Store, s.Events, v.ID)
	return s.Store.VPS(ctx, v.ID)
}

func op(cond bool, name string) string {
	if cond {
		return name
	}
	return ""
}

// TerminateOrganization : données supprimées après une longue suspension
// (module billing) ; un VPS occupé est repris au passage suivant.
func (s *Service) TerminateOrganization(ctx context.Context, org uuid.UUID) error {
	list, err := s.Store.OrganizationVPS(ctx, org)
	if err != nil {
		return err
	}
	var first error
	for i := range list {
		if _, err := s.terminate(ctx, &list[i], "billing"); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ── Équipe (back-office) ───────────────────────────────────────────────

// UnblockSpam : l'équipe lève le blocage anti-spam après vérification.
func (s *Service) UnblockSpam(ctx context.Context, id uuid.UUID, by string) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.SpamBlockedAt == nil || v.IPv4 == "" {
		return nil, domain.ErrNotSpamBlocked
	}
	if v.Operation != "" {
		return nil, domain.ErrBusy
	}
	return s.enqueueOp(ctx, v, domain.KindSpamUnblock, domain.OpUnblockingSpam, []byte(`{}`), by, nil, nil)
}

// SMTPRequest : port 25 sortant ouvert ou fermé.
type SMTPRequest struct {
	VPSID   string `path:"vpsID" json:"-"`
	Allowed bool   `json:"allowed" description:"Port 25 sortant ouvert (prend effet à la prochaine installation)"`
}

// SetSMTP : choix de l'équipe, appliqué à la prochaine installation (le
// client retire la règle de l'installation en cours, voir la doc).
func (s *Service) SetSMTP(ctx context.Context, id uuid.UUID, allowed bool) (*domain.VPS, error) {
	v, err := s.Store.VPS(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdateVPS(ctx, v.ID, map[string]any{"smtp_allowed": allowed, "updated_at": s.now()}); err != nil {
		return nil, err
	}
	Notify(ctx, s.Store, s.Events, v.ID)
	return s.Store.VPS(ctx, v.ID)
}
