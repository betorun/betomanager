package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StepAction : issue d'une carte (validée, exécutée, refusée), gardée dans le
// dossier : l'agent la relit au tour suivant.
const StepAction StepKind = "action"

// CardStatus : vie d'une carte.
type CardStatus string

const (
	CardProposed CardStatus = "proposed"
	CardApproved CardStatus = "approved" // en cours d'exécution
	CardRejected CardStatus = "rejected"
	CardExecuted CardStatus = "executed"
	CardFailed   CardStatus = "failed"
	CardExpired  CardStatus = "expired"
)

// Card : une action proposée par l'agent.
type Card struct {
	ID              uuid.UUID
	InvestigationID uuid.UUID
	Kind            string
	Params          json.RawMessage
	Target          string
	Summary         string
	State           string
	Rationale       string
	Risk            string
	Confirm         string
	Status          CardStatus
	ProposedAt      time.Time
	ExpiresAt       time.Time
	DecidedBy       string
	DecidedAt       *time.Time
	DecisionNote    string
	Result          string
	ExecutedAt      *time.Time
}

// Erreurs des cartes.
var (
	ErrCardNotFound = errors.New("operator: carte introuvable")
	// ErrCardDecided : déjà validée, refusée ou expirée.
	ErrCardDecided = errors.New("operator: carte déjà décidée")
)

// CardTTL : durée de validité d'une proposition (l'état observé vieillit).
const CardTTL = 30 * time.Minute

// MaxOpenCards : propositions en attente par dossier.
const MaxOpenCards = 5

// ErrTooManyCards : trop de propositions en attente dans le dossier.
var ErrTooManyCards = errors.New("operator: trop de cartes en attente dans ce dossier")

// ProposeCard : enregistre une proposition dans un dossier ouvert. Une
// proposition identique (même action, mêmes paramètres) encore en attente
// est rendue telle quelle (dup = true).
func (s *Store) ProposeCard(ctx context.Context, c Card) (Card, bool, error) {
	var dup bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status Status
		if err := tx.QueryRow(ctx, `SELECT status FROM investigations WHERE id=$1 FOR UPDATE`, c.InvestigationID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status != StatusOpen {
			return ErrClosed
		}
		now := s.now()
		existing, err := scanCard(tx.QueryRow(ctx, `SELECT `+cardColumns+` FROM cards
			WHERE investigation_id=$1 AND kind=$2 AND params=$3::jsonb AND status='proposed' AND expires_at>$4`,
			c.InvestigationID, c.Kind, []byte(c.Params), now))
		if err == nil {
			c, dup = existing, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM cards WHERE investigation_id=$1 AND status='proposed' AND expires_at>$2`,
			c.InvestigationID, now).Scan(&open); err != nil {
			return err
		}
		if open >= MaxOpenCards {
			return ErrTooManyCards
		}
		c.ID, c.Status, c.ProposedAt, c.ExpiresAt = uuid.New(), CardProposed, now, now.Add(CardTTL)
		_, err = tx.Exec(ctx, `INSERT INTO cards (id, investigation_id, kind, params, target, summary, state, rationale, risk,
			confirm, status, proposed_at, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			c.ID, c.InvestigationID, c.Kind, []byte(c.Params), c.Target, c.Summary, c.State, c.Rationale, c.Risk,
			c.Confirm, c.Status, c.ProposedAt, c.ExpiresAt)
		return err
	})
	return c, dup, err
}

// DecideCard : proposée → validée ou refusée, une seule fois, avant
// l'expiration. La carte rendue porte la décision.
func (s *Store) DecideCard(ctx context.Context, investigation, id uuid.UUID, approve bool, by, note string) (Card, error) {
	to := CardRejected
	if approve {
		to = CardApproved
	}
	now := s.now()
	c, err := scanCard(s.pool.QueryRow(ctx, `UPDATE cards SET status=$4, decided_by=$5, decided_at=$6, decision_note=$7
		WHERE id=$1 AND investigation_id=$2 AND status='proposed' AND expires_at>$3
		RETURNING `+cardColumns, id, investigation, now, to, by, now, note))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.card(ctx, investigation, id); gerr != nil {
			return Card{}, gerr
		}
		return Card{}, ErrCardDecided
	}
	return c, err
}

// SettleCard : issue d'une carte décidée AILLEURS (l'API), inscrite en une
// fois (refusée, exécutée, en échec, ou expirée sans décision), même après
// l'expiration locale : la décision a été prise à temps auprès de l'API,
// l'Opérateur la relit seulement plus tard.
func (s *Store) SettleCard(ctx context.Context, investigation, id uuid.UUID, to CardStatus, by, note, result string) (Card, error) {
	switch to {
	case CardRejected, CardExecuted, CardFailed, CardExpired:
	default:
		return Card{}, errors.New("operator: issue de carte invalide : " + string(to))
	}
	now := s.now()
	var executed *time.Time
	if to == CardExecuted || to == CardFailed {
		executed = &now
	}
	c, err := scanCard(s.pool.QueryRow(ctx, `UPDATE cards SET status=$3, decided_by=$4, decided_at=$5, decision_note=$6, result=$7, executed_at=$8
		WHERE id=$1 AND investigation_id=$2 AND status='proposed' RETURNING `+cardColumns, id, investigation, to, by, now, note, result, executed))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.card(ctx, investigation, id); gerr != nil {
			return Card{}, gerr
		}
		return Card{}, ErrCardDecided
	}
	return c, err
}

// ReapInterrupted : cartes validées restées sans issue depuis plus de
// « after » (processus arrêté pendant l'action) : passées en échec, avec la
// note ; l'état réel est à vérifier. Rend les cartes closes.
func (s *Store) ReapInterrupted(ctx context.Context, after time.Duration, note string) ([]Card, error) {
	now := s.now()
	rows, err := s.pool.Query(ctx, `UPDATE cards SET status='failed', result=$2, executed_at=$3
		WHERE status='approved' AND decided_at < $1 RETURNING `+cardColumns, now.Add(-after), note, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Card, error) { return scanCard(r) })
}

// FinishCard : validée → exécutée ou en échec, avec le compte rendu.
func (s *Store) FinishCard(ctx context.Context, id uuid.UUID, ok bool, result string) error {
	to := CardFailed
	if ok {
		to = CardExecuted
	}
	tag, err := s.pool.Exec(ctx, `UPDATE cards SET status=$2, result=$3, executed_at=$4 WHERE id=$1 AND status='approved'`,
		id, to, result, s.now())
	if err == nil && tag.RowsAffected() == 0 {
		return ErrCardDecided
	}
	return err
}

// Cards : les cartes d'un dossier, dans l'ordre ; une proposition échue est
// rendue « expirée ».
func (s *Store) Cards(ctx context.Context, investigation uuid.UUID) ([]Card, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cardColumns+` FROM cards WHERE investigation_id=$1 ORDER BY proposed_at, id`, investigation)
	if err != nil {
		return nil, err
	}
	cards, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Card, error) { return scanCard(r) })
	now := s.now()
	for i := range cards {
		cards[i] = Expire(cards[i], now)
	}
	return cards, err
}

// Expire : une proposition échue est expirée.
func Expire(c Card, now time.Time) Card {
	if c.Status == CardProposed && !now.Before(c.ExpiresAt) {
		c.Status = CardExpired
	}
	return c
}

func (s *Store) card(ctx context.Context, investigation, id uuid.UUID) (Card, error) {
	c, err := scanCard(s.pool.QueryRow(ctx, `SELECT `+cardColumns+` FROM cards WHERE id=$1 AND investigation_id=$2`, id, investigation))
	if errors.Is(err, pgx.ErrNoRows) {
		return Card{}, ErrCardNotFound
	}
	return c, err
}

const cardColumns = `id, investigation_id, kind, params, target, summary, state, rationale, risk, confirm, status,
	proposed_at, expires_at, decided_by, decided_at, decision_note, result, executed_at`

func scanCard(row pgx.Row) (Card, error) {
	var c Card
	var params []byte
	err := row.Scan(&c.ID, &c.InvestigationID, &c.Kind, &params, &c.Target, &c.Summary, &c.State, &c.Rationale, &c.Risk,
		&c.Confirm, &c.Status, &c.ProposedAt, &c.ExpiresAt, &c.DecidedBy, &c.DecidedAt, &c.DecisionNote, &c.Result, &c.ExecutedAt)
	c.Params = params
	return c, err
}
