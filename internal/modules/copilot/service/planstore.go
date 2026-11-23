package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Suivi d'un plan « mettre en ligne » en cours : état gardé dans Redis
// (betorun:copilot:plan:<jeton>, 24 h), lu par la console, écrit par la
// goroutine qui joue le plan.

const (
	// PlanTTL : un plan (en cours ou fini) reste lisible 24 h.
	PlanTTL = 24 * time.Hour
	// planHeartbeatStale : un plan « running » sans battement depuis plus
	// longtemps est rendu « interrupted » à la lecture (API redémarrée).
	planHeartbeatStale = 2 * time.Minute
)

// Statuts d'un plan et de ses étapes (contrat de la console).
const (
	PlanRunning     = "running"
	PlanDone        = "done"
	PlanFailed      = "failed"
	PlanPaused      = "paused"
	PlanInterrupted = "interrupted"

	StepPending = "pending"
	StepRunning = "running"
	StepDone    = "done"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// DNSRecord : enregistrement à poser chez le fournisseur DNS.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StepState : une étape et son issue.
type StepState struct {
	Key       string            `json:"key"`
	Kind      StepKind          `json:"kind"`
	Params    map[string]string `json:"params"`
	Status    string            `json:"status"`
	Message   string            `json:"message,omitempty"`
	StartedAt *time.Time        `json:"started_at,omitempty"`
	EndedAt   *time.Time        `json:"ended_at,omitempty"`
	DNS       *DNSRecord        `json:"dns,omitempty"`
}

// PlanResources : identifiants des ressources créées par le plan.
type PlanResources struct {
	WorkloadID string `json:"workload_id,omitempty"`
	BuildID    string `json:"build_id,omitempty"`
	DatabaseID string `json:"database_id,omitempty"`
	AddonID    string `json:"addon_id,omitempty"`
}

// PlanState : suivi d'un plan (contrat de la console).
type PlanState struct {
	Token     string        `json:"token"`
	Status    string        `json:"status"`
	Title     string        `json:"title"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   *time.Time    `json:"ended_at,omitempty"`
	Steps     []StepState   `json:"steps"`
	Resources PlanResources `json:"resources"`
}

// PlanRecord : ce que Redis garde d'un plan (état, propriétaire, battement).
type PlanRecord struct {
	UserID         string    `json:"user_id"`
	ConversationID string    `json:"conversation_id"`
	HeartbeatAt    time.Time `json:"heartbeat_at"`
	// DomainID : domaine ajouté (hors contrat de la console).
	DomainID string    `json:"domain_id,omitempty"`
	State    PlanState `json:"state"`
}

var ErrPlanNotFound = errors.New("plan not found or expired")

// PlanStore : stockage des plans (Redis en production ; en mémoire pour les tests).
type PlanStore interface {
	Save(ctx context.Context, rec *PlanRecord) error
	// Load : le plan, seulement s'il appartient à userID (sinon introuvable).
	Load(ctx context.Context, token, userID string) (*PlanRecord, error)
	RequestPause(ctx context.Context, token string) error
	PauseRequested(ctx context.Context, token string) bool
}

func (s *Service) PlansStore() PlanStore {
	if s.Plans != nil {
		return s.Plans
	}
	return &plans{rdb: s.Redis}
}

type plans struct {
	rdb redis.UniversalClient
}

func planKey(token string) string      { return "betorun:copilot:plan:" + token }
func planPauseKey(token string) string { return "betorun:copilot:plan:" + token + ":pause" }

func (s *plans) Save(ctx context.Context, rec *PlanRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, planKey(rec.State.Token), raw, PlanTTL).Err()
}

func (s *plans) Load(ctx context.Context, token, userID string) (*PlanRecord, error) {
	if len(token) != 64 {
		return nil, ErrPlanNotFound
	}
	raw, err := s.rdb.Get(ctx, planKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, err
	}
	var rec PlanRecord
	if json.Unmarshal(raw, &rec) != nil || rec.UserID != userID {
		return nil, ErrPlanNotFound
	}
	return &rec, nil
}

func (s *plans) RequestPause(ctx context.Context, token string) error {
	return s.rdb.Set(ctx, planPauseKey(token), "1", PlanTTL).Err()
}

// PauseRequested : Redis injoignable = pas de pause (le plan continue).
func (s *plans) PauseRequested(ctx context.Context, token string) bool {
	n, err := s.rdb.Exists(ctx, planPauseKey(token)).Result()
	return err == nil && n > 0
}

// LoadPlan : l'état d'un plan de la personne ; un plan « running » sans
// battement récent (API redémarrée en cours de route) est rendu
// « interrupted » : ses étapes non finies sont sautées.
func (s *Service) LoadPlan(ctx context.Context, token, userID string) (*PlanState, error) {
	rec, err := s.PlansStore().Load(ctx, token, userID)
	if err != nil {
		return nil, err
	}
	st := rec.State
	now := s.CurrentTime().UTC()
	if st.Status == PlanRunning && now.Sub(rec.HeartbeatAt) > planHeartbeatStale {
		st.Status = PlanInterrupted
		st.EndedAt = &now
		for i := range st.Steps {
			if st.Steps[i].Status == StepPending || st.Steps[i].Status == StepRunning {
				st.Steps[i].Status = StepSkipped
			}
		}
	}
	return &st, nil
}

// PausePlan : demande d'arrêt ; le plan finit l'étape en cours puis passe
// « paused » (pas de reprise dans ce lot).
func (s *Service) PausePlan(ctx context.Context, token, userID string) (*PlanState, error) {
	st, err := s.LoadPlan(ctx, token, userID)
	if err != nil {
		return nil, err
	}
	if st.Status == PlanRunning {
		if err := s.PlansStore().RequestPause(ctx, token); err != nil {
			return nil, err
		}
	}
	return st, nil
}
