package databases

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/service"
)

// Brancher une base exige AUSSI le droit sur les variables du conteneur.
func TestAttachChecksWorkloadPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Refus avant tout appel au service.
	h := &Controller{Service: &service.Service{}, IAM: refuseAll{}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Set("userID", uuid.NewString())
	_, err := h.Attach(c, &domain.AttachRequest{ProjectID: uuid.NewString(), DatabaseID: uuid.NewString(), WorkloadID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "workloads.variables.update") {
		t.Fatalf("refus attendu : %v", err)
	}
}

// refuseAll : IAM qui refuse tout.
type refuseAll struct{}

func (refuseAll) Check(context.Context, iam.Principal, iam.Permission, iam.ResourceName) (iam.Decision, error) {
	return iam.Decision{}, nil
}
