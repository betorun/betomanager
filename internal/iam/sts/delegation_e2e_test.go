package sts_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
)

// Jeton délégué au Copilot : sujet = la personne, agent dans « act », sans
// auth_time ni amr (jamais de step-up par délégation), révoqué avec la session.
func TestE2E_DelegatedTokens(t *testing.T) {
	d := db(t)
	ctx := context.Background()
	ring, err := sts.NewKeyRing(d, kek)
	require.NoError(t, err)
	require.NoError(t, ring.Rotate(ctx))
	svc := sts.NewService("http://127.0.0.1/kmanager/v1/iam", ring, d)
	sessions := &auth.Sessions{DB: d, Minter: svc}

	user := uuid.New()
	require.NoError(t, d.Exec(`INSERT INTO users (id, keycloak_user_id, email, full_name) VALUES (?, ?, ?, '')`,
		user, user.String(), user.String()[:8]+"@ex.sn").Error)
	tok, err := sessions.Create(ctx, auth.NewSession{UserID: user, AMR: []string{auth.AMRPassword}})
	require.NoError(t, err)

	raw, exp, err := svc.MintDelegated(sts.Delegation{UserID: user, SessionID: tok.SessionID, ACR: auth.ACRSilver,
		Actor: sts.ActorCopilot, Scope: sts.ScopeRead, TTL: time.Hour})
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(sts.MaxDelegatedTTL), exp, 5*time.Second, "durée bornée à MaxDelegatedTTL")

	v, err := svc.Verify(ctx, raw)
	require.NoError(t, err)
	assert.Equal(t, iam.UserPrincipal(user), v.Principal)
	assert.Equal(t, sts.MethodDelegated, v.Method)
	assert.Equal(t, sts.ActorCopilot, v.Actor)
	assert.Equal(t, sts.ScopeRead, v.Scope)
	assert.True(t, v.AuthTime.IsZero(), "pas d'auth_time : aucune action sensible par délégation")
	assert.Empty(t, v.AMR)

	// Session révoquée : la délégation tombe avec elle.
	require.NoError(t, sessions.Revoke(ctx, user, tok.SessionID, "test"))
	svc.ForgetStatus()
	_, err = svc.Verify(ctx, raw)
	assert.ErrorIs(t, err, sts.ErrInvalidToken)
}
