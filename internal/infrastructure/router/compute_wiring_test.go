package router

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/compute"
	computeDomain "github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	computeSvc "github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

type oneVPS struct {
	computeSvc.Store
	v computeDomain.VPS
}

func (s oneVPS) VPS(_ context.Context, id uuid.UUID) (*computeDomain.VPS, error) {
	if id != s.v.ID {
		return nil, computeDomain.ErrVPSNotFound
	}
	return &s.v, nil
}

func TestVPSProjectResolver(t *testing.T) {
	org, project, vps := uuid.New(), uuid.New(), uuid.New()
	r := &compute.Controller{Service: &computeSvc.Service{Store: oneVPS{v: computeDomain.VPS{ID: vps, OrganizationID: org, ProjectID: project}}}}
	resolve := func(orgParam, vpsParam string) (iam.ResourceName, error) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/", nil)
		c.Params = gin.Params{{Key: "id", Value: orgParam}, {Key: "vpsID", Value: vpsParam}}
		return vpsProject(r)(c)
	}
	got, err := resolve(org.String(), vps.String())
	assert.NoError(t, err)
	assert.Equal(t, iam.ProjectName(project), got, "droits sur le projet du VPS")

	other := uuid.New()
	got, _ = resolve(other.String(), vps.String())
	assert.Equal(t, iam.OrganizationName(other), got, "VPS d'une autre organisation : jamais son projet")
	got, _ = resolve(org.String(), uuid.NewString())
	assert.Equal(t, iam.OrganizationName(org), got)
	_, err = resolve(org.String(), "x")
	assert.Error(t, err)
}
