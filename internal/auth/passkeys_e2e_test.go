package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// softKey : authentificateur logiciel minimal (attestation « none », ES256),
// assez pour dérouler les cérémonies WebAuthn sans navigateur.
type softKey struct {
	t      *testing.T
	rpID   string
	origin string
	key    *ecdsa.PrivateKey
	id     []byte
	handle []byte
	count  uint32
	uv     bool
}

func newSoftKey(t *testing.T, rpID, origin string) *softKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return &softKey{t: t, rpID: rpID, origin: origin, key: k, id: id, uv: true}
}

var b64 = base64.RawURLEncoding

func (k *softKey) authData(attested bool) []byte {
	rp := sha256.Sum256([]byte(k.rpID))
	flags := byte(0x01 | 0x08 | 0x10) // UP, BE, BS
	if k.uv {
		flags |= 0x04
	}
	if attested {
		flags |= 0x40
	}
	out := append([]byte{}, rp[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, k.count)
	if attested {
		out = append(out, make([]byte, 16)...) // AAGUID
		out = binary.BigEndian.AppendUint16(out, uint16(len(k.id)))
		out = append(out, k.id...)
		x, y := k.key.X.FillBytes(make([]byte, 32)), k.key.Y.FillBytes(make([]byte, 32))
		cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
		require.NoError(k.t, err)
		out = append(out, cose...)
	}
	return out
}

func (k *softKey) clientData(typ, challenge string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return raw
}

func challengeOf(t *testing.T, options json.RawMessage) string {
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(options, &o))
	return o.PublicKey.Challenge
}

// create : réponse à navigator.credentials.create().
func (k *softKey) create(options json.RawMessage) []byte {
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	require.NoError(k.t, json.Unmarshal(options, &o))
	k.handle, _ = b64.DecodeString(o.PublicKey.User.ID)
	att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": k.authData(true)})
	require.NoError(k.t, err)
	body, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(k.clientData("webauthn.create", o.PublicKey.Challenge)),
			"attestationObject": b64.EncodeToString(att),
		},
	})
	return body
}

// get : réponse à navigator.credentials.get().
func (k *softKey) get(options json.RawMessage) []byte {
	k.count++
	ad := k.authData(false)
	cd := k.clientData("webauthn.get", challengeOf(k.t, options))
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	require.NoError(k.t, err)
	body, _ := json.Marshal(map[string]any{
		"id": b64.EncodeToString(k.id), "rawId": b64.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(cd), "authenticatorData": b64.EncodeToString(ad),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(k.handle),
		},
	})
	return body
}

func TestE2E_Passkeys(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	user, other := e2eUser(t, db), e2eUser(t, db)
	wa, err := NewWebAuthn("http://localhost:3000")
	require.NoError(t, err)
	p := &Passkeys{DB: db, WebAuthn: wa}
	f := &Factors{DB: db, Pepper: []byte("pepper"), Passkeys: p}
	key := newSoftKey(t, "localhost", "http://localhost:3000")

	// Enregistrement.
	opts, state, err := p.BeginRegistration(ctx, user, "awa@ex.sn", "Awa")
	require.NoError(t, err)
	_, err = p.FinishRegistration(ctx, other, state, key.create(opts), "Téléphone")
	assert.ErrorIs(t, err, ErrPasskeyInvalid, "cérémonie d'un autre compte")
	pk, err := p.FinishRegistration(ctx, user, state, key.create(opts), "Téléphone")
	require.NoError(t, err)
	assert.True(t, pk.Synced)
	has, err := f.HasFactor(ctx, user)
	require.NoError(t, err)
	assert.True(t, has, "une passkey est un second facteur")
	codes, err := f.EnsureRecovery(ctx, user)
	require.NoError(t, err)
	assert.Len(t, codes, RecoveryCodeCount)
	again, err := f.EnsureRecovery(ctx, user)
	require.NoError(t, err)
	assert.Nil(t, again, "codes déjà remis")
	method, err := f.Verify(ctx, user, codes[0])
	require.NoError(t, err, "code de secours sans application")
	assert.Equal(t, MethodRecovery, method)

	// Connexion sans e-mail (passkey choisie).
	opts, state, err = p.BeginLogin(ctx, uuid.Nil)
	require.NoError(t, err)
	got, err := p.FinishLogin(ctx, uuid.Nil, state, key.get(opts))
	require.NoError(t, err)
	assert.Equal(t, user, got)

	// Second facteur du compte : refusé pour un autre compte.
	opts, state, err = p.BeginLogin(ctx, user)
	require.NoError(t, err)
	_, err = p.FinishLogin(ctx, other, state, key.get(opts))
	assert.ErrorIs(t, err, ErrPasskeyInvalid)
	opts, state, err = p.BeginLogin(ctx, user)
	require.NoError(t, err)
	got, err = p.FinishLogin(ctx, user, state, key.get(opts))
	require.NoError(t, err)
	assert.Equal(t, user, got)

	// Réponse rejouée (même cérémonie) : compteur déjà vu.
	opts, state, err = p.BeginLogin(ctx, user)
	require.NoError(t, err)
	resp := key.get(opts)
	_, err = p.FinishLogin(ctx, user, state, resp)
	require.NoError(t, err)
	_, err = p.FinishLogin(ctx, user, state, resp)
	assert.ErrorIs(t, err, ErrPasskeyInvalid, "compteur revenu en arrière")

	// Sans vérification de l'utilisateur : refusé.
	key.uv = false
	opts, state, err = p.BeginLogin(ctx, user)
	require.NoError(t, err)
	_, err = p.FinishLogin(ctx, user, state, key.get(opts))
	assert.ErrorIs(t, err, ErrPasskeyInvalid)
	key.uv = true

	// Même passkey une seconde fois : exclue côté navigateur, refusée ici.
	opts, state, err = p.BeginRegistration(ctx, user, "awa@ex.sn", "Awa")
	require.NoError(t, err)
	_, err = p.FinishRegistration(ctx, user, state, key.create(opts), "Doublon")
	assert.Error(t, err)

	list, err := p.List(ctx, user)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotNil(t, list[0].LastUsedAt)
	require.NoError(t, p.Rename(ctx, user, list[0].ID, "Ordinateur"))
	assert.ErrorIs(t, p.Rename(ctx, other, list[0].ID, "x"), ErrPasskeyNotFound)

	// Retrait du dernier facteur : plus de codes de secours.
	assert.ErrorIs(t, f.RemovePasskey(ctx, other, list[0].ID), ErrPasskeyNotFound)
	require.NoError(t, f.RemovePasskey(ctx, user, list[0].ID))
	st, err := f.Status(ctx, user)
	require.NoError(t, err)
	assert.Equal(t, 0, st.Passkeys)
	assert.Equal(t, 0, st.RecoveryRemaining)
	opts, state, err = p.BeginLogin(ctx, uuid.Nil)
	require.NoError(t, err)
	_, err = p.FinishLogin(ctx, uuid.Nil, state, key.get(opts))
	assert.ErrorIs(t, err, ErrPasskeyInvalid, "passkey retirée")
}
