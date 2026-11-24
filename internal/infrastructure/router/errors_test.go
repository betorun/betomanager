package router

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/modules/assistance"
	authSvc "github.com/thekrauss/kubemanager/internal/modules/auth/service"
	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	identityKC "github.com/thekrauss/kubemanager/internal/modules/identity/keycloak"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		msg    string
	}{
		{"typed not found", betoerrors.New(betoerrors.CodeNotFound, "site introuvable"), http.StatusNotFound, betoerrors.CodeNotFound, "site introuvable"},
		{"typed conflict wrapped", fmt.Errorf("ctx: %w", betoerrors.New(betoerrors.CodeConflict, "déjà pris")), http.StatusConflict, betoerrors.CodeConflict, "déjà pris"},
		{"typed with cause hides cause", &betoerrors.Error{Code: betoerrors.CodeNotFound, Message: "workload not found", Cause: errors.New("sql: secret detail")}, http.StatusNotFound, betoerrors.CodeNotFound, "workload not found"},
		{"gorm not found", fmt.Errorf("get: %w", gorm.ErrRecordNotFound), http.StatusNotFound, betoerrors.CodeNotFound, "not found"},
		{"sentinel credentials", identityKC.ErrInvalidCredentials, http.StatusUnauthorized, betoerrors.CodeUnauthorized, "invalid credentials"},
		// Garde de facturation illisible : 503 « réessayez », cause tue au client.
		{"billing check unavailable", fmt.Errorf("%w: %w", billingDomain.ErrBillingCheckUnavailable, errors.New("dial tcp 10.0.0.5:5432: connection refused")),
			http.StatusServiceUnavailable, "BILLING_CHECK_UNAVAILABLE", billingDomain.ErrBillingCheckUnavailable.Error()},
		{"unknown → 500 generic", errors.New("dial tcp 10.0.0.5:5432: connection refused"), http.StatusInternalServerError, betoerrors.CodeInternal, "internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := classifyError(tc.err)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.code, body.Code)
			assert.Equal(t, tc.msg, body.Message)
			assert.Equal(t, body.Message, body.Error, "error duplique message pour kweb")
		})
	}
}

func TestClassifyError_UserAPIKeysRetired(t *testing.T) {
	status, body := classifyError(authSvc.ErrUserAPIKeysRetired)
	assert.Equal(t, http.StatusGone, status)
	assert.Contains(t, body.Message, "serviceAccounts", "le message indique la nouvelle API")
}

// Une raison stable traverse le hook (le message, lui, est traduit).
func TestClassifyError_Reason(t *testing.T) {
	err := fmt.Errorf("approve: %w", assistance.ErrNotPending)
	status, body := classifyError(err)
	if status != http.StatusConflict || body.Code != betoerrors.CodeConflict || body.Reason != "ASSISTANCE_NOT_PENDING" {
		t.Fatalf("%d %+v", status, body)
	}
	if _, plain := classifyError(betoerrors.New(betoerrors.CodeConflict, "x")); plain.Reason != "" {
		t.Fatalf("raison inventée : %+v", plain)
	}
}
