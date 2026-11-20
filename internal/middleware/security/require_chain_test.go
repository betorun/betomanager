package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// Garde composée (PermAll) : le handler ne doit s'exécuter qu'après le
// dernier contrôle. Require ne doit donc pas appeler c.Next() lui-même.
func TestRequire_ComposedGuardChecksAllBeforeHandler(t *testing.T) {
	gc := newGuardCase() // l'utilisateur a WorkloadsCreate, pas WorkloadsUpdate
	gin.SetMode(gin.TestMode)
	m, _ := newTestManager(t, &fakeAuthRepo{})
	m.SetAuthorizer(gc.authz)
	checks := []gin.HandlerFunc{m.Require(iam.WorkloadsCreate, ProjectParam("id")), m.Require(iam.WorkloadsUpdate, ProjectParam("id"))}
	composed := func(c *gin.Context) {
		for _, check := range checks {
			if check(c); c.IsAborted() {
				return
			}
		}
	}
	ran := false
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", gc.user.String()) })
	r.POST("/x/:id", composed, func(c *gin.Context) { ran = true; c.Status(http.StatusOK) })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x/"+gc.project.String(), nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, ran, "le handler ne doit pas s'exécuter sans la seconde permission")
}
