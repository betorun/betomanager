package llm

import (
	"context"
	"errors"
	"iter"
)

// Pricing : prix d'un modèle, en micro-dollars par million de jetons.
// Un modèle auto-hébergé (vLLM) a un prix nul : seul le plafond de jetons
// s'applique alors… et rien n'est compté. Lui donner un prix interne permet de
// garder un plafond par organisation.
type Pricing struct {
	InputMicroUSDPerMTok  int64
	OutputMicroUSDPerMTok int64
}

// Cost : coût en micro-dollars, arrondi au supérieur.
func (p Pricing) Cost(inputTokens, outputTokens int) int64 {
	micro := int64(inputTokens)*p.InputMicroUSDPerMTok + int64(outputTokens)*p.OutputMicroUSDPerMTok
	return (micro + 999_999) / 1_000_000
}

// Ledger : registre des dépenses d'IA par organisation.
//
// Reserve bloque un montant avant l'appel, et échoue (KindBudgetExceeded) si
// le plafond serait dépassé. La réservation se règle ensuite au coût réel :
// le trop-réservé est rendu. Un registre injoignable refuse
// (KindBudgetUnavailable) : sans compteur, pas de plafond.
type Ledger interface {
	Reserve(ctx context.Context, orgID string, microUSD int64) (Reservation, error)
}

type Reservation interface {
	// Settle : coût réel. Un Settle sur une réservation déjà réglée est sans effet.
	Settle(ctx context.Context, actualMicroUSD int64) error
}

// bytesPerToken : estimation PRUDENTE (basse) du nombre d'octets par jeton,
// pour borner le coût d'entrée sans tokenizer. 3 octets par jeton surestime
// le français et l'anglais (≈ 4) : on réserve un peu trop, rendu au règlement.
const bytesPerToken = 3

// EstimateInputTokens : borne haute du nombre de jetons d'entrée de req.
func EstimateInputTokens(req Request) int {
	n := len(req.System)
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	for _, m := range req.Messages {
		n += len(m.Text) + 8 // rôle et séparateurs
		for _, c := range m.ToolCalls {
			n += len(c.ID) + len(c.Name) + len(c.Arguments)
		}
		if m.ToolResult != nil {
			n += len(m.ToolResult.CallID) + len(m.ToolResult.Content)
		}
	}
	return n/bytesPerToken + 1
}

// Budget : plafonne la dépense de chaque organisation. Avant l'appel, réserve
// le coût maximal (entrée estimée + MaxOutputTokens) ; après, règle le coût
// réel. Placé autour d'UN fournisseur, car le prix dépend du modèle.
type Budget struct {
	next    Provider
	ledger  Ledger
	pricing Pricing
}

func WithBudget(p Provider, ledger Ledger, pricing Pricing) *Budget {
	return &Budget{next: p, ledger: ledger, pricing: pricing}
}

func (b *Budget) Name() string { return b.next.Name() }

func (b *Budget) reserve(ctx context.Context, req Request) (Reservation, error) {
	if req.Caller.OrgID == "" {
		return nil, &Error{Kind: KindInvalidRequest, Provider: b.Name(), Message: "Caller.OrgID est obligatoire (budget)"}
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	max := b.pricing.Cost(EstimateInputTokens(req), req.MaxOutputTokens)
	r, err := b.ledger.Reserve(ctx, req.Caller.OrgID, max)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			e.Provider = b.Name()
			return nil, e
		}
		return nil, &Error{Kind: KindBudgetUnavailable, Provider: b.Name(), Err: err}
	}
	return r, nil
}

// settle : un règlement raté ne fait pas échouer la réponse déjà obtenue ;
// la réservation reste alors comptée en entier (côté prudent). Contexte
// détaché : l'appelant a pu partir, la dépense a bien eu lieu.
func (b *Budget) settle(ctx context.Context, r Reservation, u Usage) {
	if !u.Known {
		return // on garde la réservation entière
	}
	_ = r.Settle(context.WithoutCancel(ctx), b.pricing.Cost(u.InputTokens, u.OutputTokens))
}

func (b *Budget) Generate(ctx context.Context, req Request) (*Response, error) {
	r, err := b.reserve(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := b.next.Generate(ctx, req)
	if err != nil {
		// Échec avant toute génération : rien n'a été consommé, sauf si le
		// fournisseur a coupé en route (on ne le sait pas) : prudence, on ne
		// rend que pour les erreurs de requête ou de connexion.
		if refundable(err) {
			_ = r.Settle(context.WithoutCancel(ctx), 0)
		}
		return nil, err
	}
	b.settle(ctx, r, resp.Usage)
	return resp, nil
}

func (b *Budget) Stream(ctx context.Context, req Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		r, err := b.reserve(ctx, req)
		if err != nil {
			yield(Event{}, err)
			return
		}
		started, done := false, false
		defer func() {
			if !done && !started {
				_ = r.Settle(context.WithoutCancel(ctx), 0)
			}
			// started sans done : flux coupé, réservation gardée en entier.
		}()
		for ev, err := range b.next.Stream(ctx, req) {
			if err != nil {
				if !started && refundable(err) {
					_ = r.Settle(context.WithoutCancel(ctx), 0)
					done = true
				}
				started = true // ne plus rendre dans le defer
				yield(Event{}, err)
				return
			}
			started = true
			if ev.Kind == EventDone && ev.Response != nil {
				b.settle(ctx, r, ev.Response.Usage)
				done = true
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// refundable : erreurs certaines de n'avoir rien coûté (requête refusée,
// fournisseur injoignable, disjoncteur).
func refundable(err error) bool {
	switch KindOf(err) {
	case KindInvalidRequest, KindContextTooLong, KindAuth, KindRateLimited, KindCircuitOpen, KindUnavailable:
		return true
	}
	return false
}
