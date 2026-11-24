// Package store : la base propre de l'Opérateur (ADR 0016 §1 et §5).
//
// Elle vit sur le serveur de gestion et fonctionne quand Postgres est tombé
// sur le serveur 1. Elle garde les dossiers d'investigation : sujet, motif,
// personne, étapes (appels d'outils avec condensé et résultat complet),
// conclusion.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Sujet d'une investigation.
type SubjectKind string

const (
	SubjectTicket       SubjectKind = "ticket"
	SubjectCluster      SubjectKind = "cluster"
	SubjectOrganization SubjectKind = "organization"
	SubjectIncident     SubjectKind = "incident"
	SubjectQuestion     SubjectKind = "question"
)

// Status d'une investigation.
type Status string

const (
	StatusOpen      Status = "open"
	StatusConcluded Status = "concluded"
	StatusAbandoned Status = "abandoned"
)

// StepKind : nature d'une étape.
type StepKind string

const (
	StepQuestion StepKind = "question" // message de la personne
	StepAnswer   StepKind = "answer"   // réponse de l'agent
	StepTool     StepKind = "tool"     // appel d'outil
)

// Investigation : un dossier.
type Investigation struct {
	ID          uuid.UUID
	SubjectKind SubjectKind
	SubjectRef  string
	// Reason : motif de la session, envoyé comme justification à chaque
	// lecture de données d'un client. Vide = aucune donnée de client.
	Reason       string
	ActorUserID  string
	ActorEmail   string
	Model        string
	Status       Status
	Conclusion   string
	InputTokens  int64
	OutputTokens int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ConcludedAt  *time.Time
}

// Step : une étape du dossier.
type Step struct {
	Seq        int
	Kind       StepKind
	Tool       string
	Input      json.RawMessage
	Digest     string
	Result     *string // nil une fois purgé
	IsError    bool
	DurationMs int
	CreatedAt  time.Time
}

// Erreurs.
var (
	ErrNotFound = errors.New("operator: investigation introuvable")
	ErrClosed   = errors.New("operator: investigation close")
)

// Store : accès Postgres.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Open : connexion et migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if err := Migrate(dsn); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("operator: connexion à la base : %w", err)
	}
	return &Store{pool: pool, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Migrate : applique les migrations embarquées.
func Migrate(dsn string) error {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return err
	}
	target, err := migrateURL(dsn)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, target)
	if err != nil {
		return fmt.Errorf("operator: migrations : %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("operator: migrations : %w", err)
	}
	return nil
}

// migrateURL : golang-migrate choisit son pilote par le schéma de l'URL.
func migrateURL(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return dsn, nil
	}
	return "", errors.New("operator: la base doit être une URL postgres://")
}

// Close : ferme la connexion.
func (s *Store) Close() { s.pool.Close() }

// Ping : santé de la base.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Create : ouvre un dossier.
func (s *Store) Create(ctx context.Context, in Investigation) (Investigation, error) {
	now := s.now()
	in.ID, in.Status, in.CreatedAt, in.UpdatedAt = uuid.New(), StatusOpen, now, now
	_, err := s.pool.Exec(ctx, `INSERT INTO investigations
		(id, subject_kind, subject_ref, reason, actor_user_id, actor_email, model, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`,
		in.ID, in.SubjectKind, in.SubjectRef, in.Reason, in.ActorUserID, in.ActorEmail, in.Model, in.Status, now)
	return in, err
}

// AddStep : ajoute une étape à un dossier ouvert ; renvoie son numéro.
// La ligne du dossier est verrouillée : les numéros se suivent sans trou.
func (s *Store) AddStep(ctx context.Context, id uuid.UUID, st Step) (int, error) {
	var seq int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status Status
		if err := tx.QueryRow(ctx, `SELECT status FROM investigations WHERE id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status != StatusOpen {
			return ErrClosed
		}
		now := s.now()
		var input any
		if len(st.Input) > 0 {
			input = st.Input
		}
		if err := tx.QueryRow(ctx, `INSERT INTO investigation_steps
			(investigation_id, seq, kind, tool, input, digest, result, is_error, duration_ms, created_at)
			SELECT $1, COALESCE(MAX(seq),0)+1, $2,$3,$4,$5,$6,$7,$8,$9 FROM investigation_steps WHERE investigation_id=$1
			RETURNING seq`,
			id, st.Kind, st.Tool, input, st.Digest, st.Result, st.IsError, st.DurationMs, now).Scan(&seq); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE investigations SET updated_at=$2 WHERE id=$1`, id, now)
		return err
	})
	return seq, err
}

// Conclude : clôt un dossier (conclu ou abandonné) avec la consommation du modèle.
func (s *Store) Conclude(ctx context.Context, id uuid.UUID, status Status, conclusion string, in, out int64) error {
	if status != StatusConcluded && status != StatusAbandoned {
		return fmt.Errorf("operator: statut de clôture invalide : %s", status)
	}
	now := s.now()
	tag, err := s.pool.Exec(ctx, `UPDATE investigations SET status=$2, conclusion=$3,
		input_tokens=input_tokens+$4, output_tokens=output_tokens+$5, updated_at=$6, concluded_at=$6
		WHERE id=$1 AND status='open'`, id, status, conclusion, in, out, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, _, err := s.Get(ctx, id); err != nil {
			return err
		}
		return ErrClosed
	}
	return nil
}

// AddUsage : ajoute la consommation d'un tour au dossier.
func (s *Store) AddUsage(ctx context.Context, id uuid.UUID, in, out int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE investigations SET input_tokens=input_tokens+$2, output_tokens=output_tokens+$3,
		updated_at=$4 WHERE id=$1`, id, in, out, s.now())
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

const columns = `id, subject_kind, subject_ref, reason, actor_user_id, actor_email, model, status,
	conclusion, input_tokens, output_tokens, created_at, updated_at, concluded_at`

func scan(row pgx.Row) (Investigation, error) {
	var i Investigation
	err := row.Scan(&i.ID, &i.SubjectKind, &i.SubjectRef, &i.Reason, &i.ActorUserID, &i.ActorEmail, &i.Model, &i.Status,
		&i.Conclusion, &i.InputTokens, &i.OutputTokens, &i.CreatedAt, &i.UpdatedAt, &i.ConcludedAt)
	return i, err
}

// Get : un dossier et ses étapes.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Investigation, []Step, error) {
	inv, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM investigations WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Investigation{}, nil, ErrNotFound
	}
	if err != nil {
		return Investigation{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, kind, tool, input, digest, result, is_error, duration_ms, created_at
		FROM investigation_steps WHERE investigation_id=$1 ORDER BY seq`, id)
	if err != nil {
		return Investigation{}, nil, err
	}
	steps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Step, error) {
		var st Step
		var input []byte
		err := r.Scan(&st.Seq, &st.Kind, &st.Tool, &input, &st.Digest, &st.Result, &st.IsError, &st.DurationMs, &st.CreatedAt)
		if len(input) > 0 {
			st.Input = input
		}
		return st, err
	})
	return inv, steps, err
}

// Conversation : un dossier et ses seuls échanges (questions, réponses,
// issues des cartes), sans les résultats d'outils : de quoi reprendre un tour
// sans charger des mégaoctets de preuves que le modèle ne relit pas.
func (s *Store) Conversation(ctx context.Context, id uuid.UUID) (Investigation, []Step, error) {
	inv, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM investigations WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Investigation{}, nil, ErrNotFound
	}
	if err != nil {
		return Investigation{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, kind, tool, digest, is_error, created_at
		FROM investigation_steps WHERE investigation_id=$1 AND kind IN ('question', 'answer', 'action') ORDER BY seq`, id)
	if err != nil {
		return Investigation{}, nil, err
	}
	steps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Step, error) {
		var st Step
		err := r.Scan(&st.Seq, &st.Kind, &st.Tool, &st.Digest, &st.IsError, &st.CreatedAt)
		return st, err
	})
	return inv, steps, err
}

// List : les dossiers les plus récents d'abord.
func (s *Store) List(ctx context.Context, limit int) ([]Investigation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM investigations ORDER BY created_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Investigation, error) { return scan(r) })
}

// PurgeResults : efface les résultats complets plus anciens que before ;
// le condensé reste. Renvoie le nombre d'étapes purgées.
func (s *Store) PurgeResults(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE investigation_steps SET result=NULL WHERE result IS NOT NULL AND created_at < $1`, before)
	return tag.RowsAffected(), err
}
