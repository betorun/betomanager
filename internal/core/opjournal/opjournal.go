// Package opjournal : recopie du journal de l'Opérateur dans l'audit de la
// plateforme (ADR 0016 §1), format partagé par l'Opérateur (qui signe) et
// l'API (qui vérifie et importe).
//
// L'Opérateur signe chaque lot avec sa clé Ed25519 ; l'API ne connaît que
// les clés publiques. Message signé :
//
//	betorun-operator-journal-v1\n<horodatage unix>\n<sha256 hex du corps>
//
// L'horodatage doit être à moins de MaxSkew de l'horloge de l'API ; chaque
// entrée porte un UID : un lot rejoué n'importe rien deux fois.
package opjournal

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// En-têtes de la requête.
const (
	HeaderKey       = "X-Operator-Key"
	HeaderTimestamp = "X-Operator-Timestamp"
	HeaderSignature = "X-Operator-Signature"
)

// Bornes.
const (
	Path           = "/admin/operator/journal"
	MaxSkew        = 5 * time.Minute
	MaxEntries     = 200
	MaxBody        = 1 << 20
	signingContext = "betorun-operator-journal-v1"
)

// Entry : une entrée du journal du serveur de gestion.
type Entry struct {
	UID             uuid.UUID      `json:"uid"`
	At              time.Time      `json:"at"`
	Actor           string         `json:"actor"`
	Kind            string         `json:"kind"`
	InvestigationID *uuid.UUID     `json:"investigation_id,omitempty"`
	Detail          map[string]any `json:"detail,omitempty"`
}

// Batch : corps de la requête.
type Batch struct {
	Entries []Entry `json:"entries"`
}

// Result : réponse de l'API. Imported : entrées nouvelles ; les autres
// étaient déjà importées (rejeu, reprise après coupure).
type Result struct {
	Accepted int `json:"accepted"`
	Imported int `json:"imported"`
}

// ErrBadSignature : clé inconnue, horodatage hors fenêtre ou signature fausse.
var ErrBadSignature = errors.New("operator journal: bad signature")

// KeyID : identifiant public d'une clé (16 caractères hex du sha256).
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

func message(ts int64, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(signingContext + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(sum[:]))
}

// Sign : en-têtes d'une requête (clé, horodatage, signature).
func Sign(priv ed25519.PrivateKey, now time.Time, body []byte) map[string]string {
	ts := now.Unix()
	pub := priv.Public().(ed25519.PublicKey)
	return map[string]string{
		HeaderKey:       KeyID(pub),
		HeaderTimestamp: strconv.FormatInt(ts, 10),
		HeaderSignature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message(ts, body))),
	}
}

// Verify : la requête vient d'une des clés connues, dans la fenêtre.
func Verify(keys map[string]ed25519.PublicKey, keyID, timestamp, signature string, body []byte, now time.Time) error {
	pub, ok := keys[keyID]
	if !ok {
		return ErrBadSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || !ed25519.Verify(pub, message(ts, body), sig) {
		return ErrBadSignature
	}
	return nil
}

// ParsePublicKeys : clés publiques en base64 (32 octets), par identifiant.
// Deux clés permettent une rotation sans coupure.
func ParsePublicKeys(encoded []string) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, s := range encoded {
		if s == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, errors.New("operator journal: a public key must be 32 bytes in base64")
		}
		pub := ed25519.PublicKey(raw)
		keys[KeyID(pub)] = pub
	}
	return keys, nil
}

// ParsePrivateKey : graine Ed25519 (32 octets) en base64.
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("operator journal: the private key must be a 32-byte seed in base64")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// Encode : le corps d'un lot.
func Encode(b Batch) ([]byte, error) { return json.Marshal(b) }
