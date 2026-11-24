package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RescueAccount : compte de secours d'un membre de l'équipe.
type RescueAccount struct {
	ID             uuid.UUID
	Email          string
	PasswordHash   string
	TOTPSecret     []byte // chiffré
	TOTPLastStep   int64
	FailedAttempts int
	LockedUntil    *time.Time
	DisabledAt     *time.Time
	CreatedAt      time.Time
	LastLoginAt    *time.Time
}

// ErrExists : compte déjà présent.
var ErrExists = errors.New("operator: compte de secours déjà présent")

const rescueColumns = `id, email, password_hash, totp_secret, totp_last_step, failed_attempts, locked_until, disabled_at, created_at, last_login_at`

func scanRescue(row pgx.Row) (RescueAccount, error) {
	var a RescueAccount
	err := row.Scan(&a.ID, &a.Email, &a.PasswordHash, &a.TOTPSecret, &a.TOTPLastStep, &a.FailedAttempts,
		&a.LockedUntil, &a.DisabledAt, &a.CreatedAt, &a.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// CreateRescueAccount : nouveau compte (id fourni : il lie le secret chiffré).
func (s *Store) CreateRescueAccount(ctx context.Context, a RescueAccount) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO rescue_accounts (id, email, password_hash, totp_secret, created_at)
		VALUES ($1,$2,$3,$4,$5)`, a.ID, a.Email, a.PasswordHash, a.TOTPSecret, s.now())
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrExists
	}
	return err
}

// RescueAccountByEmail : un compte par son e-mail.
func (s *Store) RescueAccountByEmail(ctx context.Context, email string) (RescueAccount, error) {
	return scanRescue(s.pool.QueryRow(ctx, `SELECT `+rescueColumns+` FROM rescue_accounts WHERE email=$1`, email))
}

// RescueAccounts : tous les comptes.
func (s *Store) RescueAccounts(ctx context.Context) ([]RescueAccount, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+rescueColumns+` FROM rescue_accounts ORDER BY email`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RescueAccount, error) { return scanRescue(r) })
}

// DisableRescueAccount : désactive un compte et révoque ses sessions.
func (s *Store) DisableRescueAccount(ctx context.Context, email string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		now := s.now()
		err := tx.QueryRow(ctx, `UPDATE rescue_accounts SET disabled_at=COALESCE(disabled_at,$2) WHERE email=$1 RETURNING id`, email, now).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE rescue_sessions SET revoked_at=$2 WHERE account_id=$1 AND revoked_at IS NULL`, id, now)
		return err
	})
}

// RescueFailure : un échec de connexion ; au-delà de max échecs, le compte
// est verrouillé pour lock (et le compteur repart de zéro).
func (s *Store) RescueFailure(ctx context.Context, id uuid.UUID, max int, lock time.Duration) (locked bool, err error) {
	now := s.now()
	err = s.pool.QueryRow(ctx, `UPDATE rescue_accounts SET
		failed_attempts = CASE WHEN failed_attempts+1 >= $2 THEN 0 ELSE failed_attempts+1 END,
		locked_until    = CASE WHEN failed_attempts+1 >= $2 THEN $3::timestamptz ELSE locked_until END
		WHERE id=$1 RETURNING locked_until IS NOT NULL AND locked_until > $4`,
		id, max, now.Add(lock), now).Scan(&locked)
	return locked, err
}

// RescueSuccess : connexion réussie. Le pas TOTP utilisé est enregistré
// atomiquement : un même code ne sert qu'une fois (false : rejoué).
func (s *Store) RescueSuccess(ctx context.Context, id uuid.UUID, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE rescue_accounts SET totp_last_step=$2, failed_attempts=0, locked_until=NULL,
		last_login_at=$3 WHERE id=$1 AND totp_last_step < $2`, id, step, s.now())
	return tag.RowsAffected() == 1, err
}

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CreateRescueSession : session (seul le condensé du jeton est gardé).
func (s *Store) CreateRescueSession(ctx context.Context, token string, account uuid.UUID, ttl time.Duration) (time.Time, error) {
	now := s.now()
	exp := now.Add(ttl)
	_, err := s.pool.Exec(ctx, `INSERT INTO rescue_sessions (token_hash, account_id, created_at, expires_at) VALUES ($1,$2,$3,$4)`,
		tokenHash(token), account, now, exp)
	return exp, err
}

// RescueSession : le compte d'une session valide (ni expirée, ni révoquée,
// compte actif).
func (s *Store) RescueSession(ctx context.Context, token string) (RescueAccount, error) {
	return scanRescue(s.pool.QueryRow(ctx, `SELECT `+prefixed("a.", rescueColumns)+`
		FROM rescue_sessions r JOIN rescue_accounts a ON a.id = r.account_id
		WHERE r.token_hash=$1 AND r.revoked_at IS NULL AND r.expires_at > $2 AND a.disabled_at IS NULL`, tokenHash(token), s.now()))
}

// RevokeRescueSession : déconnexion.
func (s *Store) RevokeRescueSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `UPDATE rescue_sessions SET revoked_at=$2 WHERE token_hash=$1 AND revoked_at IS NULL`, tokenHash(token), s.now())
	return err
}

// prefixed : colonnes qualifiées par une table (« a.id, a.email… »).
func prefixed(p, cols string) string { return p + strings.ReplaceAll(cols, ", ", ", "+p) }

// Entry : une ligne du journal.
type Entry struct {
	ID              int64
	UID             uuid.UUID
	At              time.Time
	Actor           string
	Kind            string
	InvestigationID *uuid.UUID
	Detail          map[string]any
}

// Journal : inscrit une entrée.
func (s *Store) Journal(ctx context.Context, actor, kind string, investigation *uuid.UUID, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO journal (at, actor, kind, investigation_id, detail) VALUES ($1,$2,$3,$4,$5)`,
		s.now(), actor, kind, investigation, b)
	return err
}

// JournalEntries : les plus récentes d'abord.
func (s *Store) JournalEntries(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.journalRows(ctx, `SELECT id, uid, at, actor, kind, investigation_id, detail FROM journal ORDER BY id DESC LIMIT $1`, limit)
}

// UnsyncedJournal : les plus anciennes entrées pas encore recopiées dans
// l'audit de la plateforme.
func (s *Store) UnsyncedJournal(ctx context.Context, limit int) ([]Entry, error) {
	return s.journalRows(ctx, `SELECT id, uid, at, actor, kind, investigation_id, detail FROM journal
		WHERE synced_at IS NULL ORDER BY id LIMIT $1`, limit)
}

// MarkSynced : entrées recopiées (acceptées par l'API).
func (s *Store) MarkSynced(ctx context.Context, ids []int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE journal SET synced_at=$2 WHERE id = ANY($1) AND synced_at IS NULL`, ids, s.now())
	return err
}

func (s *Store) journalRows(ctx context.Context, query string, limit int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		var raw []byte
		err := r.Scan(&e.ID, &e.UID, &e.At, &e.Actor, &e.Kind, &e.InvestigationID, &raw)
		if err == nil {
			err = json.Unmarshal(raw, &e.Detail)
		}
		return e, err
	})
}
