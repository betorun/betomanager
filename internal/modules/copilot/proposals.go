package copilot

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
}

// view : détails = arguments hors identifiants de chemin (déjà résumés par Target).
func (p *Proposal) view(pathParams map[string]bool) View {
	details := map[string]any{}
	for k, v := range p.Request.Args {
		if !pathParams[k] {
			details[k] = v
		}
	}
	if len(details) == 0 {
		details = nil
	}
	return View{Token: p.Token, Tool: p.Request.Tool, Risk: p.Risk, Target: p.Target,
		Details: details, ExpiresAt: p.ExpiresAt, Undo: p.UndoOf != ""}
}

var (
	errProposalNotFound = errors.New("proposal not found or expired")
	errProposalHandled  = errors.New("proposal already handled")
)

// ProposalStore : stockage des propositions (Redis en production ; une
// version en mémoire sert aux tests).
type ProposalStore interface {
	save(ctx context.Context, p *Proposal, ttl time.Duration) error
	load(ctx context.Context, token, userID string) (*Proposal, error)
	claim(ctx context.Context, token string) error
	drop(ctx context.Context, token string)
}

// proposals : stockage Redis des propositions.
type proposals struct {
	rdb redis.UniversalClient
}

func proposalKey(token string) string { return "betorun:copilot:action:" + token }
func claimKey(token string) string    { return "betorun:copilot:action:" + token + ":claim" }

// newToken : 32 octets aléatoires (le jeton est la seule preuve de la carte,
// avec l'identité de la personne).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *proposals) save(ctx context.Context, p *Proposal, ttl time.Duration) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, proposalKey(p.Token), raw, ttl).Err()
}

// load : la proposition, seulement si elle appartient à userID (sinon
// « introuvable » : on ne révèle pas l'existence d'une carte d'autrui).
func (s *proposals) load(ctx context.Context, token, userID string) (*Proposal, error) {
	if len(token) != 64 {
		return nil, errProposalNotFound
	}
	raw, err := s.rdb.Get(ctx, proposalKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errProposalNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Proposal
	if json.Unmarshal(raw, &p) != nil || p.UserID != userID {
		return nil, errProposalNotFound
	}
	return &p, nil
}

// claim : réserve la carte pour UN traitement (validation ou refus). Toujours
// sur Redis, jamais en cache local : deux clics, deux onglets ou deux
// instances de l'API ne l'exécutent pas deux fois.
func (s *proposals) claim(ctx context.Context, token string) error {
	ok, err := s.rdb.SetNX(ctx, claimKey(token), "1", claimTTL).Result()
	if err != nil {
		return err
	}
	if !ok {
		return errProposalHandled
	}
	return nil
}

func (s *proposals) drop(ctx context.Context, token string) {
	_ = s.rdb.Del(ctx, proposalKey(token)).Err()
}
