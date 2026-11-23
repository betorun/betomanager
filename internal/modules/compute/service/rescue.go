package service

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// EnterRescue : le VPS redémarre sur le système de secours ; le mot de passe
// envoyé par le fournisseur est intercepté, chiffré, affiché une seule fois.
func (s *Service) EnterRescue(ctx context.Context, org, id uuid.UUID, by string) (*domain.VPS, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if err := ready(v); err != nil {
		return nil, err
	}
	now := s.now()
	return s.enqueueOp(ctx, v, domain.KindRescueEnter, domain.OpEnteringRescue, []byte(`{}`), by,
		map[string]any{"rescue_requested_at": now, "rescue_secret": ""}, nil)
}

// ExitRescue : retour au démarrage sur le disque du VPS.
func (s *Service) ExitRescue(ctx context.Context, org, id uuid.UUID, by string) (*domain.VPS, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	switch {
	case v.Rescue != domain.RescueActive:
		return nil, domain.ErrNotInRescue
	case v.Operation != "":
		return nil, domain.ErrBusy
	case v.State == domain.StateSuspended:
		return nil, domain.ErrSuspended
	}
	return s.enqueueOp(ctx, v, domain.KindRescueExit, domain.OpExitingRescue, []byte(`{}`), by, nil, nil)
}

// RescueReveal : identifiants du mode secours, rendus une seule fois.
type RescueReveal struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Host     string `json:"host"`
}

// RevealRescue : le mot de passe est déchiffré une fois puis n'est plus lisible.
func (s *Service) RevealRescue(ctx context.Context, org, id uuid.UUID) (*RescueReveal, error) {
	if s.Sealer == nil {
		return nil, domain.ErrRescueSecretUnavailable
	}
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if v.Rescue != domain.RescueActive {
		return nil, domain.ErrNotInRescue
	}
	sec, err := s.Store.TakeSecret(ctx, v.ID, domain.SecretRescuePassword, s.now())
	if err != nil {
		return nil, err
	}
	if sec == nil {
		return nil, domain.ErrRescueSecretUnavailable
	}
	plain, err := s.Sealer.Open(sec.Ciphertext, RescueAAD(v.ID))
	if err != nil {
		return nil, domain.ErrRescueSecretUnavailable
	}
	var creds domain.RescueCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return nil, domain.ErrRescueSecretUnavailable
	}
	_ = s.Store.UpdateVPS(ctx, v.ID, map[string]any{"rescue_secret": domain.RescueSecretRead, "updated_at": s.now()})
	Notify(ctx, s.Store, s.Events, v.ID)
	return &RescueReveal{Username: creds.Username, Password: creds.Password, Host: v.IPv4}, nil
}

// RescueAAD : données associées du chiffrement (un secret recopié sur un
// autre VPS ne se déchiffre pas).
func RescueAAD(vps uuid.UUID) string {
	return "compute:vps:" + vps.String() + ":" + domain.SecretRescuePassword
}
