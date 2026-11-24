package operatoraccess

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
)

type minter struct{ got sts.Delegation }

func (m *minter) MintDelegated(d sts.Delegation) (string, time.Time, error) {
	m.got = d
	return "tok", time.Unix(1, 0), nil
}

type authz struct{ grant []iam.Permission }

func (a authz) TestPermissions(_ context.Context, _ iam.Principal, r iam.ResourceName, _ []iam.Permission) ([]iam.Permission, error) {
	if r != iam.Platform {
		return nil, errors.New("ressource inattendue")
	}
	return a.grant, nil
}

func ctx(set map[string]string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	for k, v := range set {
		c.Set(k, v)
	}
	return c
}

func TestToken(t *testing.T) {
	m := &minter{}
	h := &Controller{STS: m}
	user, session := uuid.New(), uuid.New()
	out, err := h.Token(ctx(map[string]string{"authMethod": "bearer", "internalUserID": user.String(), "sessionID": session.String()}))
	if err != nil || out.Token != "tok" {
		t.Fatal(out, err)
	}
	if m.got.Actor != sts.ActorOperator || m.got.Scope != sts.ScopeRead || m.got.UserID != user || m.got.SessionID != session || m.got.TTL != TokenTTL {
		t.Fatalf("délégation = %+v", m.got)
	}
	// Un jeton délégué (ou une clé) n'obtient pas de jeton pour l'Opérateur.
	for _, method := range []string{"delegated", "apikey"} {
		if _, err := h.Token(ctx(map[string]string{"authMethod": method, "internalUserID": user.String(), "sessionID": session.String()})); err == nil {
			t.Fatalf("%s accepté", method)
		}
	}
}

func TestMe(t *testing.T) {
	user := uuid.New()
	base := map[string]string{"authMethod": "delegated", "delegationActor": sts.ActorOperator, "delegationScope": "read",
		"internalUserID": user.String(), "iamPrincipal": string(iam.UserPrincipal(user)), "email": "awa@betorun.test"}
	h := &Controller{IAM: authz{grant: []iam.Permission{iam.PlatformOperatorUse, iam.PlatformSupportManage}}}
	out, err := h.Me(ctx(base))
	if err != nil {
		t.Fatal(err)
	}
	if out.UserID != user || out.Email != "awa@betorun.test" || out.Capabilities != (Capabilities{Support: true}) {
		t.Fatalf("%+v", out)
	}

	copilot := map[string]string{}
	for k, v := range base {
		copilot[k] = v
	}
	copilot["delegationActor"] = sts.ActorCopilot
	if _, err := h.Me(ctx(copilot)); err == nil {
		t.Fatal("jeton du Copilot accepté")
	}
	session := map[string]string{"authMethod": "bearer", "internalUserID": user.String(), "iamPrincipal": string(iam.UserPrincipal(user))}
	if _, err := h.Me(ctx(session)); err == nil {
		t.Fatal("session accepté sans délégation")
	}
	withdrawn := &Controller{IAM: authz{grant: []iam.Permission{iam.PlatformSupportManage}}}
	if _, err := withdrawn.Me(ctx(base)); err == nil {
		t.Fatal("accès retiré accepté")
	}
}
