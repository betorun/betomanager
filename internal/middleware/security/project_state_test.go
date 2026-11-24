package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/iam"
)

type fakeProjectState struct{ pending map[uuid.UUID]bool }

func (f fakeProjectState) PendingDeletion(_ context.Context, p uuid.UUID) (bool, error) {
	return f.pending[p], nil
}

// Projet en suppression programmée : écritures refusées (409), lectures,
// restauration et nouvelle demande de suppression permises.
func TestRequire_ProjectPendingDeletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, pending, live := uuid.New(), uuid.New(), uuid.New()
	authz := &fakeAuthorizer{allowed: map[string]bool{}}
	for _, p := range []uuid.UUID{pending, live} {
		for _, perm := range []iam.Permission{iam.WorkloadsCreate, iam.WorkloadsGet, iam.ProjectsUndelete, iam.ProjectsDelete} {
			authz.allowed[key(iam.UserPrincipal(u), perm, iam.ProjectName(p))] = true
		}
	}
	m, _ := newTestManager(t, &fakeAuthRepo{})
	m.SetAuthorizer(authz)
	m.SetProjectState(fakeProjectState{pending: map[uuid.UUID]bool{pending: true}})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", u.String()) })
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.POST("/p/:id/workloads", m.Require(iam.WorkloadsCreate, ProjectParam("id")), ok)
	r.GET("/p/:id/workloads", m.Require(iam.WorkloadsGet, ProjectParam("id")), ok)
	r.POST("/p/:id/restore", m.Require(iam.ProjectsUndelete, ProjectParam("id")), ok)
	r.DELETE("/p/:id", m.Require(iam.ProjectsDelete, ProjectParam("id")), ok)

	do := func(method, url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
		return rec
	}
	rec := do(http.MethodPost, "/p/"+pending.String()+"/workloads")
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "PROJECT_PENDING_DELETION")
	assert.Equal(t, http.StatusOK, do(http.MethodPost, "/p/"+live.String()+"/workloads").Code, "autre projet intact")
	assert.Equal(t, http.StatusOK, do(http.MethodGet, "/p/"+pending.String()+"/workloads").Code, "lecture permise")
	assert.Equal(t, http.StatusOK, do(http.MethodPost, "/p/"+pending.String()+"/restore").Code, "restauration permise")
	assert.Equal(t, http.StatusOK, do(http.MethodDelete, "/p/"+pending.String()).Code, "suppression idempotente")
}
