package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind : nature d'une erreur, qui décide de la bascule vers un autre
// fournisseur et de l'ouverture du disjoncteur.
type Kind string

const (
	// Pannes du fournisseur : un autre peut réussir.
	KindUnavailable     Kind = "unavailable"      // 5xx, connexion refusée
	KindTimeout         Kind = "timeout"          // délai dépassé côté fournisseur
	KindRateLimited     Kind = "rate_limited"     // 429 du fournisseur
	KindAuth            Kind = "auth"             // clé refusée : configuration de CE fournisseur
	KindMalformedOutput Kind = "malformed_output" // appel d'outil sans nom, inconnu, arguments invalides
	KindCircuitOpen     Kind = "circuit_open"     // disjoncteur ouvert

	// Erreurs de la requête ou de l'appelant : aucun autre ne fera mieux.
	KindInvalidRequest    Kind = "invalid_request"
	KindContextTooLong    Kind = "context_too_long"
	KindContentFiltered   Kind = "content_filtered"
	KindBudgetExceeded    Kind = "budget_exceeded"    // plafond de l'organisation atteint
	KindBudgetUnavailable Kind = "budget_unavailable" // registre des dépenses injoignable (on refuse)
	KindCanceled          Kind = "canceled"           // l'appelant a abandonné
)

// Error : erreur d'un Provider. Les adaptateurs ne rendent que ce type, ou une
// erreur de contexte.
type Error struct {
	Kind     Kind
	Provider string
	// Status : code HTTP du fournisseur, 0 si sans objet.
	Status int
	// RetryAfter : délai demandé par le fournisseur (429), 0 sinon.
	RetryAfter time.Duration
	// Message : explication lisible, sans secret (jamais la clé, jamais le prompt).
	Message string
	Err     error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("llm %s: %s", e.Provider, e.Kind)
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Failover : un autre fournisseur peut-il réussir là où celui-ci a échoué ?
func (e *Error) Failover() bool {
	switch e.Kind {
	case KindUnavailable, KindTimeout, KindRateLimited, KindAuth, KindMalformedOutput, KindCircuitOpen:
		return true
	}
	return false
}

// countsAsFailure : l'erreur témoigne-t-elle d'un fournisseur en mauvaise
// santé ? (disjoncteur). Un 429 ou une clé refusée, oui ; une requête trop
// longue ou un appelant pressé, non.
func (e *Error) countsAsFailure() bool {
	switch e.Kind {
	case KindUnavailable, KindTimeout, KindRateLimited, KindAuth, KindMalformedOutput:
		return true
	}
	return false
}

// KindOf : nature de n'importe quelle erreur rendue par un Provider.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, context.Canceled) {
		return KindCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindUnavailable
}

// CanFailover : version de Error.Failover pour toute erreur. L'abandon par
// l'appelant (contexte annulé ou expiré) n'est jamais rattrapé : le contexte
// ne vaut plus rien pour le fournisseur suivant.
func CanFailover(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Failover()
	}
	return false
}

// asError : convertit une erreur quelconque en *Error attribuée à provider.
func asError(provider string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: KindOf(err), Provider: provider, Err: err}
}
