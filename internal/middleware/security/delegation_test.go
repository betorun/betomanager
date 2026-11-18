package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
)

func delegatedCtx(scope string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if scope != "" {
		c.Set("authMethod", AuthMethodDelegated)
		c.Set(ctxActor, sts.ActorCopilot)
		c.Set(ctxDelegationScope, scope)
	} else {
		c.Set("authMethod", "bearer")
	}
	return c, w
}

func TestCheckDelegated(t *testing.T) {
	cases := []struct {
		scope string
		perm  iam.Permission
		want  bool
	}{
		{"", iam.ProjectsDelete, true}, // personne : géré par checkMFA, pas ici
		{sts.ScopeRead, iam.ProjectsGet, true},
		{sts.ScopeRead, iam.Permission("workloads.workloads.update"), false},
		{sts.ScopeWrite, iam.Permission("workloads.workloads.update"), true},
		{sts.ScopeWrite, iam.ProjectsDelete, false},       // sensible : jamais par délégation
		{sts.ScopeWrite, iam.ProjectsSetIamPolicy, false}, // sensible
	}
	for _, tc := range cases {
		c, w := delegatedCtx(tc.scope)
		if got := checkDelegated(c, tc.perm); got != tc.want {
			t.Errorf("scope=%q perm=%s : %v, attendu %v", tc.scope, tc.perm, got, tc.want)
		}
		if !tc.want && w.Code != http.StatusForbidden {
			t.Errorf("refus attendu en 403, eu %d", w.Code)
		}
	}
}

func TestRefuseDelegated(t *testing.T) {
	c, w := delegatedCtx(sts.ScopeWrite)
	RefuseDelegated(c)
	if !c.IsAborted() || w.Code != http.StatusForbidden {
		t.Fatalf("route Authenticated : jeton délégué refusé attendu (code %d)", w.Code)
	}
	c, _ = delegatedCtx("")
	RefuseDelegated(c)
	if c.IsAborted() {
		t.Fatal("une personne passe")
	}
}
