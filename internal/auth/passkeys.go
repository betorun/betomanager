package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Passkeys (WebAuthn) : second facteur, vérification avant une action
// sensible, ou connexion sans mot de passe. La vérification de l'utilisateur
// (empreinte, visage, code de l'appareil) est toujours exigée : une passkey
// vaut donc deux facteurs (possession + biométrie ou code).

// AMRPasskey : connexion ou vérification par passkey (claim amr).
const AMRPasskey = "passkey"

// MethodPasskey : méthode de vérification (journal, e-mails).
const MethodPasskey = "passkey"

// MaxPasskeys : passkeys par compte.
const MaxPasskeys = 10

var (
	ErrPasskeyUnavailable = errors.New("passkeys are not available on this platform")
	ErrPasskeyInvalid     = errors.New("this passkey could not be verified")
	ErrPasskeyNotFound    = errors.New("passkey not found")
	ErrPasskeyLimit       = errors.New("too many passkeys: remove one first")
	ErrPasskeyName        = errors.New("invalid passkey name: 1 to 60 characters")
)

type passkeyRow struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	UserID       uuid.UUID `gorm:"type:uuid"`
	CredentialID []byte
	Credential   []byte `gorm:"type:jsonb"`
	Name         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

func (passkeyRow) TableName() string { return "auth_passkeys" }

// Passkey : vue d'une passkey (jamais la clé publique).
type Passkey struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// Synced : passkey synchronisée entre appareils (trousseau, gestionnaire).
	Synced bool `json:"synced"`
}

// Passkeys : enregistrement et vérification.
type Passkeys struct {
	DB *gorm.DB
	// WebAuthn : nil si la console n'a pas d'adresse configurée.
	WebAuthn *webauthn.WebAuthn
	Now      func() time.Time
}

// NewWebAuthn : partie de confiance = la console (son domaine et son origine).
// appOrigins : origines en plus, celles de l'application mobile du
// back-office (Android : « android:apk-key-hash:<empreinte du certificat> » ;
// iOS signe avec l'origine HTTPS du domaine, déjà acceptée).
func NewWebAuthn(consoleURL string, appOrigins ...string) (*webauthn.WebAuthn, error) {
	consoleURL = strings.TrimRight(strings.TrimSpace(consoleURL), "/")
	host := consoleURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, ":/"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return nil, ErrPasskeyUnavailable
	}
	return webauthn.New(&webauthn.Config{
		RPID: host, RPDisplayName: "betorun", RPOrigins: passkeyOrigins(consoleURL, appOrigins),
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification: protocol.VerificationRequired,
		},
	})
}

func passkeyOrigins(consoleURL string, appOrigins []string) []string {
	out := []string{consoleURL}
	for _, o := range appOrigins {
		if o = strings.TrimSpace(o); o != "" && o != consoleURL {
			out = append(out, o)
		}
	}
	return out
}

func (p *Passkeys) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Passkeys) ready() error {
	if p == nil || p.DB == nil || p.WebAuthn == nil {
		return ErrPasskeyUnavailable
	}
	return nil
}

// passkeyUser : compte vu par WebAuthn. L'identifiant (user handle) est
// l'identifiant interne du compte : ni e-mail ni nom.
type passkeyUser struct {
	id          uuid.UUID
	name        string
	displayName string
	creds       []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.id[:] }
func (u *passkeyUser) WebAuthnName() string                       { return u.name }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (p *Passkeys) rows(ctx context.Context, userID uuid.UUID) ([]passkeyRow, error) {
	var rows []passkeyRow
	err := p.DB.WithContext(ctx).Where("user_id = ?", userID).Order("created_at").Find(&rows).Error
	return rows, err
}

func (p *Passkeys) user(ctx context.Context, userID uuid.UUID, name, display string) (*passkeyUser, []passkeyRow, error) {
	rows, err := p.rows(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	u := &passkeyUser{id: userID, name: name, displayName: display}
	for _, r := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(r.Credential, &c); err != nil {
			return nil, nil, err
		}
		u.creds = append(u.creds, c)
	}
	return u, rows, nil
}

// Count : passkeys du compte.
func (p *Passkeys) Count(ctx context.Context, userID uuid.UUID) (int, error) {
	if p == nil || p.DB == nil {
		return 0, nil
	}
	var n int64
	err := p.DB.WithContext(ctx).Model(&passkeyRow{}).Where("user_id = ?", userID).Count(&n).Error
	return int(n), err
}

// List : passkeys du compte, de la plus ancienne à la plus récente.
func (p *Passkeys) List(ctx context.Context, userID uuid.UUID) ([]Passkey, error) {
	out := []Passkey{}
	if p == nil || p.DB == nil {
		return out, nil
	}
	rows, err := p.rows(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, view(r))
	}
	return out, nil
}

func view(r passkeyRow) Passkey {
	var c webauthn.Credential
	_ = json.Unmarshal(r.Credential, &c)
	return Passkey{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, Synced: c.Flags.BackupEligible}
}

// BeginRegistration : options pour navigator.credentials.create() et état
// de la cérémonie (à garder côté serveur).
func (p *Passkeys) BeginRegistration(ctx context.Context, userID uuid.UUID, name, display string) (json.RawMessage, []byte, error) {
	if err := p.ready(); err != nil {
		return nil, nil, err
	}
	u, rows, err := p.user(ctx, userID, name, display)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) >= MaxPasskeys {
		return nil, nil, ErrPasskeyLimit
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for _, c := range u.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := p.WebAuthn.BeginRegistration(u, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, nil, err
	}
	return marshalPair(creation, session)
}

// FinishRegistration : vérifie la réponse du navigateur et enregistre la passkey.
func (p *Passkeys) FinishRegistration(ctx context.Context, userID uuid.UUID, state []byte, response []byte, label string) (*Passkey, error) {
	if err := p.ready(); err != nil {
		return nil, err
	}
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > 60 {
		return nil, ErrPasskeyName
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(state, &session); err != nil {
		return nil, ErrPasskeyInvalid
	}
	if !bytes.Equal(session.UserID, userID[:]) {
		return nil, ErrPasskeyInvalid
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return nil, ErrPasskeyInvalid
	}
	u, rows, err := p.user(ctx, userID, "", "")
	if err != nil {
		return nil, err
	}
	if len(rows) >= MaxPasskeys {
		return nil, ErrPasskeyLimit
	}
	cred, err := p.WebAuthn.CreateCredential(u, session, parsed)
	if err != nil || !cred.Flags.UserVerified {
		return nil, ErrPasskeyInvalid
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	row := passkeyRow{UserID: userID, CredentialID: cred.ID, Credential: raw, Name: label, CreatedAt: p.now()}
	if err := p.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	v := view(row)
	return &v, nil
}

// BeginLogin : options pour navigator.credentials.get(). userID nul :
// connexion sans e-mail (passkey choisie par la personne) ; sinon, seules
// les passkeys du compte sont proposées.
func (p *Passkeys) BeginLogin(ctx context.Context, userID uuid.UUID) (json.RawMessage, []byte, error) {
	if err := p.ready(); err != nil {
		return nil, nil, err
	}
	if userID == uuid.Nil {
		assertion, session, err := p.WebAuthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return nil, nil, err
		}
		return marshalPair(assertion, session)
	}
	u, _, err := p.user(ctx, userID, "", "")
	if err != nil {
		return nil, nil, err
	}
	if len(u.creds) == 0 {
		return nil, nil, ErrPasskeyNotFound
	}
	assertion, session, err := p.WebAuthn.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, nil, err
	}
	return marshalPair(assertion, session)
}

// FinishLogin : vérifie la signature ; renvoie le compte. userID non nul :
// la passkey doit appartenir à ce compte.
func (p *Passkeys) FinishLogin(ctx context.Context, userID uuid.UUID, state []byte, response []byte) (uuid.UUID, error) {
	if err := p.ready(); err != nil {
		return uuid.Nil, err
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(state, &session); err != nil {
		return uuid.Nil, ErrPasskeyInvalid
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return uuid.Nil, ErrPasskeyInvalid
	}
	var owner uuid.UUID
	var cred *webauthn.Credential
	if userID == uuid.Nil {
		var u webauthn.User
		u, cred, err = p.WebAuthn.ValidatePasskeyLogin(func(rawID, handle []byte) (webauthn.User, error) {
			id, err := uuid.FromBytes(handle)
			if err != nil {
				return nil, ErrPasskeyInvalid
			}
			pu, _, err := p.user(ctx, id, "", "")
			if err != nil {
				return nil, err
			}
			return pu, nil
		}, session, parsed)
		if err == nil {
			owner = u.(*passkeyUser).id
		}
	} else {
		var pu *passkeyUser
		pu, _, err = p.user(ctx, userID, "", "")
		if err == nil {
			cred, err = p.WebAuthn.ValidateLogin(pu, session, parsed)
			owner = userID
		}
	}
	if err != nil || cred == nil || !cred.Flags.UserVerified {
		return uuid.Nil, ErrPasskeyInvalid
	}
	if cred.Authenticator.CloneWarning {
		// Compteur de signatures revenu en arrière : clé peut-être copiée.
		return uuid.Nil, ErrPasskeyInvalid
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return uuid.Nil, err
	}
	now := p.now()
	res := p.DB.WithContext(ctx).Model(&passkeyRow{}).Where("user_id = ? AND credential_id = ?", owner, cred.ID).
		Updates(map[string]any{"credential": raw, "last_used_at": now})
	if res.Error != nil {
		return uuid.Nil, res.Error
	}
	if res.RowsAffected == 0 {
		return uuid.Nil, ErrPasskeyInvalid // retirée entre-temps
	}
	return owner, nil
}

// Rename : nouveau nom d'une passkey du compte.
func (p *Passkeys) Rename(ctx context.Context, userID, id uuid.UUID, label string) error {
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > 60 {
		return ErrPasskeyName
	}
	res := p.DB.WithContext(ctx).Model(&passkeyRow{}).Where("user_id = ? AND id = ?", userID, id).Update("name", label)
	if res.Error == nil && res.RowsAffected == 0 {
		return ErrPasskeyNotFound
	}
	return res.Error
}

func marshalPair(options any, session *webauthn.SessionData) (json.RawMessage, []byte, error) {
	opts, err := json.Marshal(options)
	if err != nil {
		return nil, nil, err
	}
	state, err := json.Marshal(session)
	if err != nil {
		return nil, nil, err
	}
	return opts, state, nil
}
