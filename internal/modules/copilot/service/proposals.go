package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Propositions d'action. Le modèle n'exécute jamais une écriture : il la
// PROPOSE. La requête est figée (tools.Prepared), gardée quelques minutes, et
// présentée sur une carte. Seule la validation de la personne la joue, une
// seule fois (réservation du jeton), avec un jeton délégué en écriture.
const (
	// ProposalTTL : délai pour valider une carte.
	ProposalTTL = 10 * time.Minute
	// UndoTTL : délai pour annuler une action faite.
	UndoTTL = 30 * time.Minute
	// claimTTL : une carte traitée (validée ou refusée) le reste.
	claimTTL = 24 * time.Hour
)

// Proposal : une action en attente de validation.
type Proposal struct {
	Token          string         `json:"token"`
	UserID         string         `json:"user_id"`
	ConversationID string         `json:"conversation_id"`
	Request        tools.Prepared `json:"request"`
	Risk           tools.Risk     `json:"risk"`
	Target         string         `json:"target,omitempty"` // nom lisible de la ressource visée
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	UndoOf         string         `json:"undo_of,omitempty"`
	// Strong : carte sensible, jouée avec la session de la personne (double
	// authentification exigée par l'API) ; ConfirmName : Target se retape.
	Strong      bool `json:"strong,omitempty"`
	ConfirmName bool `json:"confirm_name,omitempty"`
	// Plan : carte « mettre en ligne » (lot B) : plusieurs étapes jouées à la
	// suite par le serveur après UNE validation (Request ne porte que le nom
	// de l'outil).
	Plan *LaunchPlan `json:"plan,omitempty"`
}

// View : ce que la console affiche d'une proposition (jamais l'utilisateur
// ni la requête brute).
type View struct {
	Token     string         `json:"token"`
	Tool      string         `json:"tool"`
	Risk      tools.Risk     `json:"risk"`
	Target    string         `json:"target,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	ExpiresAt time.Time      `json:"expires_at"`
	Undo      bool           `json:"undo,omitempty"`
	// Strong : la validation demande la double authentification ;
	// ConfirmName : la personne retape Target avant de valider.
	Strong      bool `json:"strong,omitempty"`
	ConfirmName bool `json:"confirm_name,omitempty"`
	// Plan : étapes d'une carte « mettre en ligne » (contrat de la console).
	Plan *PlanView `json:"plan,omitempty"`
}

// view : détails = arguments hors identifiants de chemin (déjà résumés par Target).
func (p *Proposal) View(pathParams map[string]bool) View {
	details := map[string]any{}
	for k, v := range p.Request.Args {
		if !pathParams[k] {
			details[k] = v
		}
	}
	if len(details) == 0 {
		details = nil
	}
	v := View{Token: p.Token, Tool: p.Request.Tool, Risk: p.Risk, Target: p.Target,
		Details: details, ExpiresAt: p.ExpiresAt, Undo: p.UndoOf != "", Strong: p.Strong, ConfirmName: p.ConfirmName}
	if p.Plan != nil {
		pv := p.Plan.View()
		v.Plan, v.Details = &pv, nil
	}
	return v
}

var (
	ErrProposalNotFound = errors.New("proposal not found or expired")
	ErrProposalHandled  = errors.New("proposal already handled")
)

// ProposalStore : stockage des propositions (Redis en production ; une
// version en mémoire sert aux tests).
type ProposalStore interface {
	Save(ctx context.Context, p *Proposal, ttl time.Duration) error
	Load(ctx context.Context, token, userID string) (*Proposal, error)
	Claim(ctx context.Context, token string) error
	// Release : la carte redevient validable (confirmation d'identité
	// demandée : rien n'a été fait).
	Release(ctx context.Context, token string)
	Drop(ctx context.Context, token string)
}

// proposals : stockage Redis des propositions.
type proposals struct {
	rdb redis.UniversalClient
}

func proposalKey(token string) string { return "betorun:copilot:action:" + token }
func claimKey(token string) string    { return "betorun:copilot:action:" + token + ":claim" }

// newToken : 32 octets aléatoires (le jeton est la seule preuve de la carte,
// avec l'identité de la personne).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *proposals) Save(ctx context.Context, p *Proposal, ttl time.Duration) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, proposalKey(p.Token), raw, ttl).Err()
}

// load : la proposition, seulement si elle appartient à userID (sinon
// « introuvable » : on ne révèle pas l'existence d'une carte d'autrui).
func (s *proposals) Load(ctx context.Context, token, userID string) (*Proposal, error) {
	if len(token) != 64 {
		return nil, ErrProposalNotFound
	}
	raw, err := s.rdb.Get(ctx, proposalKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrProposalNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Proposal
	if json.Unmarshal(raw, &p) != nil || p.UserID != userID {
		return nil, ErrProposalNotFound
	}
	return &p, nil
}

// claim : réserve la carte pour UN traitement (validation ou refus). Toujours
// sur Redis, jamais en cache local : deux clics, deux onglets ou deux
// instances de l'API ne l'exécutent pas deux fois.
func (s *proposals) Claim(ctx context.Context, token string) error {
	ok, err := s.rdb.SetNX(ctx, claimKey(token), "1", claimTTL).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrProposalHandled
	}
	return nil
}

func (s *proposals) Release(ctx context.Context, token string) {
	_ = s.rdb.Del(ctx, claimKey(token)).Err()
}

func (s *proposals) Drop(ctx context.Context, token string) {
	_ = s.rdb.Del(ctx, proposalKey(token)).Err()
}
