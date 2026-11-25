package iamapi

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/core/country"
)

// Pays qui n'est pas un code ISO attribué : refusé avant toute écriture.
func TestUpdateOrganization_InvalidCountry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("PATCH", "/organizations/x", nil)
	h := &Controller{} // sans base : une écriture paniquerait
	for _, bad := range []string{"XX", "GAB", "1", "EU"} {
		_, err := h.UpdateOrganization(c, &UpdateOrganizationRequest{OrgID: uuid.NewString(), Country: &bad})
		assert.ErrorIs(t, err, country.ErrInvalid, bad)
	}
}
