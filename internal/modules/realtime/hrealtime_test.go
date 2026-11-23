package realtime

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
)

// allowOnly : n'autorise que ce principal.
type allowOnly struct{ p iam.Principal }

func (a allowOnly) Check(_ context.Context, p iam.Principal, _ iam.Permission, _ iam.ResourceName) (iam.Decision, error) {
	return iam.Decision{Allowed: p == a.p}, nil
}

type fakeSigner struct{}

func (fakeSigner) GenerateConnectionToken(context.Context, string, time.Time, map[string]any) (string, error) {
	return "conn", nil
}
func (fakeSigner) GenerateSubscriptionToken(context.Context, string, string, time.Time) (string, error) {
	return "sub", nil
}

// Une personne connectée par session porte deux identifiants : celui de
// l'annuaire (« userID ») et l'identifiant interne (principal IAM). Les
// rôles sont rattachés au second : c'est lui qui doit être vérifié.
func TestGetSubscriptionToken_UsesIAMPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	internal := iam.UserPrincipal(uuid.New())
	directoryID := uuid.NewString()
	project := uuid.New()
	ctrl := &Controller{signer: fakeSigner{}, authz: allowOnly{p: internal}}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("userID", directoryID)
	security.SetAuditPrincipal(c, internal)

	res, err := ctrl.GetSubscriptionToken(c, &SubscribeRequest{Channel: "project:" + project.String() + ":events"})
	if err != nil {
		t.Fatalf("abonnement refusé au propriétaire du projet : %v", err)
	}
	if res.Token != "sub" {
		t.Fatalf("jeton inattendu : %q", res.Token)
	}

	// Un autre principal reste refusé.
	other, _ := gin.CreateTestContext(httptest.NewRecorder())
	other.Request = httptest.NewRequest("GET", "/", nil)
	other.Set("userID", directoryID)
	security.SetAuditPrincipal(other, iam.UserPrincipal(uuid.New()))
	if _, err := ctrl.GetSubscriptionToken(other, &SubscribeRequest{Channel: "project:" + project.String() + ":events"}); err == nil {
		t.Fatal("abonnement accordé à un non-membre")
	}
}

// permsOnly : n'accorde que ces permissions.
type permsOnly map[iam.Permission]bool

func (p permsOnly) Check(_ context.Context, _ iam.Principal, perm iam.Permission, _ iam.ResourceName) (iam.Decision, error) {
	return iam.Decision{Allowed: p[perm]}, nil
}

// Le canal d'un VPS porte l'objet complet : membre du projet ne suffit pas.
func TestGetSubscriptionToken_VPSChannelNeedsVPSGet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	project, vps := uuid.New(), uuid.New()
	ask := func(perms permsOnly, channel string) error {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Set("userID", uuid.NewString())
		security.SetAuditPrincipal(c, iam.UserPrincipal(uuid.New()))
		_, err := (&Controller{signer: fakeSigner{}, authz: perms}).GetSubscriptionToken(c, &SubscribeRequest{Channel: channel})
		return err
	}
	vpsChannel := "project:" + project.String() + ":vps:" + vps.String()
	if err := ask(permsOnly{iam.ProjectsGet: true}, vpsChannel); err == nil {
		t.Fatal("canal du VPS ouvert sans compute.vps.get")
	}
	if err := ask(permsOnly{iam.ProjectsGet: true, iam.VPSGet: true}, vpsChannel); err != nil {
		t.Fatalf("canal du VPS refusé avec le droit : %v", err)
	}
	if err := ask(permsOnly{iam.ProjectsGet: true}, "project:"+project.String()+":events"); err != nil {
		t.Fatalf("canal du projet refusé à un membre : %v", err)
	}
}
