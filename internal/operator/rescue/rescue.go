// Package rescue : accès de secours de l'équipe à l'Opérateur (ADR 0016 §1).
//
// Quand l'API ou l'annuaire ne répondent plus, l'équipe se connecte avec un
// compte local nominatif du serveur de gestion : mot de passe ET code TOTP,
// toujours. Une session de secours n'ouvre que le chemin de secours
// (infrastructure), jamais les données des clients, et tout entre au
// journal du serveur de gestion. Les comptes se créent en ligne de commande
// sur le serveur (operator rescue add), jamais par l'API.
package rescue

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/thekrauss/kubemanager/internal/core/totp"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// Réglages.
const (
	MinPassword = 16
	MaxFailures = 5
	LockFor     = 15 * time.Minute
	SessionTTL  = 2 * time.Hour
	// TokenPrefix : distingue un jeton de secours d'un jeton délégué de l'API.
	TokenPrefix = "rsc_"
	issuer      = "betorun-operateur"
)

// Erreurs (le message ne dit jamais lequel du compte, du mot de passe ou du
// code est faux).
var (
	ErrBadCredentials = errors.New("operator: identifiants de secours refusés")
	ErrLocked         = errors.New("operator: trop de tentatives, réessayer plus tard")
	ErrWeakPassword   = fmt.Errorf("operator: mot de passe de secours trop court (%d caractères au moins)", MinPassword)
	ErrInvalidEmail   = errors.New("operator: e-mail invalide")
	ErrBadCode        = errors.New("operator: code TOTP refusé")
	ErrInvalidKey     = errors.New("operator: la clé de secours doit faire 32 octets, en base64")
	ErrReason         = errors.New("operator: motif de connexion de secours obligatoire (10 à 300 caractères)")
)

// Alerter : prévient toute l'équipe, hors du serveur 1 (webhook). Chaque
// connexion de secours est un événement que tout le monde doit voir.
type Alerter interface {
	Alert(ctx context.Context, text string)
}

// Store : la base de l'Opérateur.
type Store interface {
	CreateRescueAccount(ctx context.Context, a store.RescueAccount) error
	RescueAccountByEmail(ctx context.Context, email string) (store.RescueAccount, error)
	RescueFailure(ctx context.Context, id uuid.UUID, max int, lock time.Duration) (bool, error)
	RescueSuccess(ctx context.Context, id uuid.UUID, step int64) (bool, error)
	CreateRescueSession(ctx context.Context, token string, account uuid.UUID, ttl time.Duration) (time.Time, error)
	RescueSession(ctx context.Context, token string) (store.RescueAccount, error)
	RevokeRescueSession(ctx context.Context, token string) error
	Journal(ctx context.Context, actor, kind string, investigation *uuid.UUID, detail map[string]any) error
}

// Service : comptes et sessions de secours.
type Service struct {
	Store Store
	key   cipher.AEAD
	Now   func() time.Time
	// Alert : nil = journal seulement.
	Alert Alerter
}

// New : keyBase64 est la clé de chiffrement des secrets TOTP (32 octets),
// lue hors base (fichier).
func New(st Store, keyBase64 string) (*Service, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyBase64))
	if err != nil || len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Service{Store: st, key: aead}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// seal / open : le secret est lié à son compte (données associées).
func (s *Service) seal(id uuid.UUID, plain []byte) ([]byte, error) {
	nonce := make([]byte, s.key.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.key.Seal(nonce, nonce, plain, id[:]), nil
}

func (s *Service) open(id uuid.UUID, sealed []byte) ([]byte, error) {
	n := s.key.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("secret illisible")
	}
	return s.key.Open(nil, sealed[:n], sealed[n:], id[:])
}

// Pending : compte prêt, en attente du premier code (preuve que
// l'application d'authentification est réglée).
type Pending struct {
	account store.RescueAccount
	secret  []byte
	// URI : à scanner (otpauth://) ; Secret : saisie manuelle.
	URI    string
	Secret string
}

// Prepare : vérifie l'e-mail et le mot de passe, tire le secret TOTP.
func (s *Service) Prepare(email, password string) (*Pending, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return nil, ErrInvalidEmail
	}
	if utf8.RuneCountInString(password) < MinPassword {
		return nil, ErrWeakPassword
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	sealed, err := s.seal(id, secret)
	if err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	return &Pending{account: store.RescueAccount{ID: id, Email: email, PasswordHash: hash, TOTPSecret: sealed},
		secret: secret, URI: totp.URI(secret, issuer, email), Secret: totp.Format(secret)}, nil
}

// Commit : enregistre le compte si le code de l'application est bon.
func (s *Service) Commit(ctx context.Context, p *Pending, code string) error {
	if totp.Match(p.secret, code, s.now(), 0) == 0 {
		return ErrBadCode
	}
	if err := s.Store.CreateRescueAccount(ctx, p.account); err != nil {
		return err
	}
	return s.Store.Journal(ctx, "cli", "rescue.account.added", nil, map[string]any{"email": p.account.Email})
}

// dummyHash : comparé quand le compte n'existe pas (même durée de réponse).
var dummyHash, _ = HashPassword("compte-inexistant-pour-egaliser-le-temps")

// Login : mot de passe et code, toujours vérifiés tous les deux, et un
// motif (comme un accès d'urgence : il est journalisé et envoyé à l'équipe).
// Rend un jeton de session (affiché une fois, seul son condensé est gardé).
func (s *Service) Login(ctx context.Context, email, password, code, reason, remote string) (string, time.Time, store.RescueAccount, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 10 || n > 300 {
		return "", time.Time{}, store.RescueAccount{}, ErrReason
	}
	acc, err := s.Store.RescueAccountByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		_ = VerifyPassword(dummyHash, password)
		s.journal(ctx, email, "rescue.login.failed", map[string]any{"remote": remote, "reason": "unknown"})
		return "", time.Time{}, acc, ErrBadCredentials
	}
	if err != nil {
		return "", time.Time{}, acc, err
	}
	now := s.now()
	if acc.LockedUntil != nil && acc.LockedUntil.After(now) {
		s.journal(ctx, email, "rescue.login.locked", map[string]any{"remote": remote})
		return "", time.Time{}, acc, ErrLocked
	}
	passOK := VerifyPassword(acc.PasswordHash, password)
	var step int64
	if secret, err := s.open(acc.ID, acc.TOTPSecret); err == nil {
		step = totp.Match(secret, code, now, acc.TOTPLastStep)
	}
	if !passOK || step == 0 || acc.DisabledAt != nil {
		locked, err := s.Store.RescueFailure(ctx, acc.ID, MaxFailures, LockFor)
		if err != nil {
			return "", time.Time{}, acc, err
		}
		s.journal(ctx, email, "rescue.login.failed", map[string]any{"remote": remote, "locked": locked})
		if locked {
			s.alert(ctx, fmt.Sprintf("Opérateur : compte de secours %s verrouillé après %d échecs.", email, MaxFailures))
			return "", time.Time{}, acc, ErrLocked
		}
		return "", time.Time{}, acc, ErrBadCredentials
	}
	fresh, err := s.Store.RescueSuccess(ctx, acc.ID, step)
	if err != nil {
		return "", time.Time{}, acc, err
	}
	if !fresh { // code déjà utilisé (connexion concurrente)
		s.journal(ctx, email, "rescue.login.failed", map[string]any{"remote": remote, "reason": "replay"})
		return "", time.Time{}, acc, ErrBadCredentials
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, acc, err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	exp, err := s.Store.CreateRescueSession(ctx, token, acc.ID, SessionTTL)
	if err != nil {
		return "", time.Time{}, acc, err
	}
	s.journal(ctx, email, "rescue.login", map[string]any{"remote": remote, "reason": reason})
	s.alert(ctx, fmt.Sprintf("Opérateur : connexion de SECOURS de %s (2 h). Motif : %s", email, reason))
	return token, exp, acc, nil
}

// Session : le compte d'un jeton de secours valide.
func (s *Service) Session(ctx context.Context, token string) (store.RescueAccount, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return store.RescueAccount{}, ErrBadCredentials
	}
	acc, err := s.Store.RescueSession(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return acc, ErrBadCredentials
	}
	return acc, err
}

// Logout : révoque la session.
func (s *Service) Logout(ctx context.Context, token, email string) error {
	if err := s.Store.RevokeRescueSession(ctx, token); err != nil {
		return err
	}
	s.journal(ctx, email, "rescue.logout", nil)
	return nil
}

// alert : sans attendre ; une alerte perdue n'empêche pas l'accès (le
// journal reste la trace).
func (s *Service) alert(ctx context.Context, text string) {
	if s.Alert == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		s.Alert.Alert(ctx, text)
	}()
}

func (s *Service) journal(ctx context.Context, email, kind string, detail map[string]any) {
	_ = s.Store.Journal(ctx, "rescue:"+email, kind, nil, detail)
}

// Paramètres argon2id (RFC 9106, profil « mémoire réduite ») : 64 Mio,
// 3 passes, 2 fils.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword : $argon2id$v=19$m=…,t=…,p=…$sel$condensé (base64 brut).
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword : comparaison en temps constant ; un condensé mal formé
// est un refus.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 || m > 1<<20 || t > 10 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
