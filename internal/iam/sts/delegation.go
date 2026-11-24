package sts

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// Délégation à un agent de betorun (le Copilot).
//
// Une personne connectée délègue à l'agent un jeton COURT, DÉRIVÉ DE SA
// SESSION : même sujet (ses droits, jamais plus), révoqué avec la session,
// et marqué « act » (RFC 8693) pour que l'audit dise « Awa via Copilot ».
//
// Ce que le jeton délégué ne porte PAS, volontairement :
//   - auth_time et amr : une action sensible (step-up) n'est JAMAIS possible
//     par délégation ; la personne la fait elle-même (security.Require) ;
//   - le droit d'appeler les routes « tout utilisateur authentifié » qui ne
//     l'acceptent pas explicitement (sessions, clés, profil, création de
//     projet…).
//
// L'acr de la session est conservé : il atteste comment la personne s'est
// connectée, et une organisation qui exige la double authentification
// (iam.requireMfa) continue de le vérifier.
const (
	// ActorCopilot : le Copilot de betorun.
	ActorCopilot = "betorun:copilot"
	// ActorOperator : l'Opérateur, agent d'exploitation de l'équipe (ADR 0016).
	ActorOperator = "betorun:operator"

	// ScopeRead : permissions de lecture seulement (iam.Permission.ReadOnly).
	ScopeRead = "read"
	// ScopeWrite : toutes les permissions de la personne, hors actions sensibles.
	ScopeWrite = "write"

	// MaxDelegatedTTL : durée maximale d'un jeton délégué. Un tour de
	// conversation (ou l'exécution d'une action validée) tient largement dedans.
	MaxDelegatedTTL = 15 * time.Minute
)

var knownActors = map[string]bool{ActorCopilot: true, ActorOperator: true}

// Delegation : demande de jeton délégué.
type Delegation struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	ACR       string
	Actor     string
	Scope     string
	TTL       time.Duration
}

// MintDelegated : jeton délégué pour d.Actor au nom de la personne de la
// session d.SessionID. L'appelant garantit que la session est celle de la
// requête en cours (jamais une session lue ailleurs).
func (s *Service) MintDelegated(d Delegation) (string, time.Time, error) {
	if d.UserID == uuid.Nil || d.SessionID == uuid.Nil {
		return "", time.Time{}, errors.New("sts: délégation sans personne ni session")
	}
	if !knownActors[d.Actor] {
		return "", time.Time{}, fmt.Errorf("sts: agent inconnu %q", d.Actor)
	}
	if d.Scope != ScopeRead && d.Scope != ScopeWrite {
		return "", time.Time{}, fmt.Errorf("sts: portée inconnue %q", d.Scope)
	}
	if d.TTL <= 0 || d.TTL > MaxDelegatedTTL {
		d.TTL = MaxDelegatedTTL
	}
	return s.mint(Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: string(iam.UserPrincipal(d.UserID))},
		Method:           MethodDelegated,
		KeyID:            sessionRefPrefix + d.SessionID.String(), // révoqué avec la session
		SessionID:        d.SessionID.String(),
		ACR:              d.ACR,
		Act:              &Actor{Subject: d.Actor},
		Scope:            d.Scope,
	}, d.TTL)
}

// checkDelegation : cohérence des claims de délégation. Un jeton délégué
// porte un agent connu, une portée connue, une session, et n'a ni auth_time
// ni amr ; un autre jeton ne porte ni act ni scope.
func checkDelegation(c Claims) error {
	if c.Method != MethodDelegated {
		if c.Act != nil || c.Scope != "" {
			return ErrInvalidToken
		}
		return nil
	}
	if c.Act == nil || !knownActors[c.Act.Subject] || (c.Scope != ScopeRead && c.Scope != ScopeWrite) ||
		c.SessionID == "" || c.AuthTime != 0 || len(c.AMR) > 0 {
		return ErrInvalidToken
	}
	if c.ExpiresAt == nil || c.IssuedAt == nil || c.ExpiresAt.Sub(c.IssuedAt.Time) > MaxDelegatedTTL {
		return ErrInvalidToken
	}
	return nil
}
