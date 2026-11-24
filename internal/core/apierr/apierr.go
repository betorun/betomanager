// Package apierr : raison stable jointe à une erreur de l'API.
//
// Le champ "message" d'une réponse d'erreur est traduit dans la langue de la
// requête : un client ne doit jamais le comparer. Quand deux refus partagent
// le même code (deux CONFLICT, par exemple) et que le client doit les
// distinguer, l'erreur porte une raison : un code machine en majuscules,
// jamais traduit, rendu dans le champ "reason".
package apierr

import "errors"

type reasoned struct {
	err    error
	reason string
}

func (e *reasoned) Error() string  { return e.err.Error() }
func (e *reasoned) Unwrap() error  { return e.err }
func (e *reasoned) Reason() string { return e.reason }

// WithReason : err (en général un betoerrors.New, qui garde code, statut et
// message) avec une raison stable, ex. "ASSISTANCE_ALREADY_OPEN".
func WithReason(err error, reason string) error {
	return &reasoned{err: err, reason: reason}
}

// Reason : la raison portée par err ou une erreur qu'il enveloppe ; "" sinon.
func Reason(err error) string {
	var r interface{ Reason() string }
	if errors.As(err, &r) {
		return r.Reason()
	}
	return ""
}
