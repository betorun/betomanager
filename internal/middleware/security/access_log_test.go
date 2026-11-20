package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
)

type memWriter struct{ entries []*audit.Entry }

func (w *memWriter) Write(_ context.Context, e *audit.Entry) error {
	w.entries = append(w.entries, e)
	return nil
}

type fixedDataPolicy bool

func (p fixedDataPolicy) DataAccessLogged(context.Context, iam.ResourceName) (bool, error) {
	return bool(p), nil
}

// accessLogRouter : garde factice (ressource posée) puis contrôles.
func accessLogRouter(t *testing.T, policy DataAccessPolicy, resource iam.ResourceName, checks ...gin.HandlerFunc) (*gin.Engine, *memWriter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	m, _ := newTestManager(t, &fakeAuthRepo{})
	if policy != nil {
		m.SetDataAccessPolicy(policy)
	}
	w := &memWriter{}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", uuid.NewString()) }, m.AuditMiddleware(w))
	handlers := []gin.HandlerFunc{func(c *gin.Context) { c.Set("iamResource", resource.String()) }}
	handlers = append(handlers, checks...)
	handlers = append(handlers, func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/:id", handlers...)
	return r, w
}

func get(r *gin.Engine, path string, header map[string]string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

func TestAudit_DataAccessReads(t *testing.T) {
	workload := iam.ProjectName(uuid.New()).Child("workloads", uuid.New())

	// Lecture ordinaire : jamais journalisée.
	r, w := accessLogRouter(t, fixedDataPolicy(true), workload)
	assert.Equal(t, http.StatusOK, get(r, "/x", nil))
	assert.Empty(t, w.entries)

	// Lecture de données, organisation sans journal : rien.
	r, w = accessLogRouter(t, fixedDataPolicy(false), workload, MarkDataAccess)
	assert.Equal(t, http.StatusOK, get(r, "/x", nil))
	assert.Empty(t, w.entries)

	// Organisation avec journal : une entrée marquée.
	r, w = accessLogRouter(t, fixedDataPolicy(true), workload, MarkDataAccess)
	assert.Equal(t, http.StatusOK, get(r, "/x", nil))
	require.Len(t, w.entries, 1)
	assert.Equal(t, true, w.entries[0].Metadata["data_access"])
	assert.Equal(t, workload.String(), w.entries[0].Resource)
}

func TestAudit_StaffAccess(t *testing.T) {
	org := uuid.New()
	r, w := accessLogRouter(t, nil, iam.Platform, StaffAccess(StaffOrganization("id")))

	assert.Equal(t, http.StatusBadRequest, get(r, "/"+org.String(), nil), "motif exigé")
	assert.Equal(t, http.StatusBadRequest, get(r, "/"+org.String(), map[string]string{HeaderJustification: "court"}))
	assert.Empty(t, w.entries)

	motif := "Ticket 4521 : facture contestée"
	assert.Equal(t, http.StatusBadRequest, get(r, "/pas-un-uuid", map[string]string{HeaderJustification: url.QueryEscape(motif)}))
	assert.Equal(t, http.StatusOK, get(r, "/"+org.String(), map[string]string{HeaderJustification: url.QueryEscape(motif)}))
	require.Len(t, w.entries, 1, "lecture du staff : toujours journalisée")
	e := w.entries[0]
	assert.Equal(t, iam.OrganizationName(org).String(), e.Resource, "dans le journal du client")
	assert.Equal(t, true, e.Metadata["staff_access"])
	assert.Equal(t, motif, e.Metadata["justification"], "motif décodé (UTF-8)")
	assert.Equal(t, "platform", e.Metadata["staff_permission_resource"])
}
