package insights

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/insights/domain"
	"github.com/thekrauss/kubemanager/internal/modules/insights/service"
)

type fakeIAM struct {
	granted []iam.Permission
	asked   iam.ResourceName
}

func (f *fakeIAM) TestPermissions(_ context.Context, _ iam.Principal, r iam.ResourceName, _ []iam.Permission) ([]iam.Permission, error) {
	f.asked = r
	return f.granted, nil
}

func ctxFor(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("userID", uuid.NewString())
	return c
}

// Droits : chaque source suit sa permission ; les domaines exigent aussi la
// liste de leurs porteurs.
func TestAccessFromPermissions(t *testing.T) {
	project := uuid.New()
	f := &fakeIAM{granted: []iam.Permission{iam.WorkloadsList, iam.SiteDomainsList, iam.DatabasesList}}
	h := &Controller{Service: &service.Service{}, IAM: f}
	a, err := h.access(ctxFor(t), project)
	if err != nil {
		t.Fatal(err)
	}
	want := service.Access{Workloads: true, Databases: true}
	if a != want {
		t.Fatalf("accès = %+v", a)
	}
	if f.asked != iam.ProjectName(project) {
		t.Fatalf("droits lus sur %q", f.asked)
	}
	f.granted = []iam.Permission{iam.WorkloadsList, iam.WorkloadDomainsList, iam.BillingAccountsGet}
	if a, _ = h.access(ctxFor(t), project); !a.WorkloadDomains || !a.Budget || a.SiteDomains {
		t.Fatalf("accès = %+v", a)
	}
	// Sans moteur IAM : refus, jamais tout.
	h.IAM = nil
	if _, err := h.access(ctxFor(t), project); err == nil {
		t.Fatal("sans IAM : refus attendu")
	}
}

func TestGet(t *testing.T) {
	h := &Controller{Service: &service.Service{}, IAM: &fakeIAM{}}
	l, err := h.Get(ctxFor(t), &domain.ProjectPath{ProjectID: uuid.NewString()})
	if err != nil || l.Insights == nil {
		t.Fatalf("liste : %+v %v", l, err)
	}
	if _, err := h.Get(ctxFor(t), &domain.ProjectPath{ProjectID: "x"}); err == nil {
		t.Fatal("identifiant invalide accepté")
	}
}
