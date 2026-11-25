package clusters

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// orgAccess : l'appelant ne voit que l'organisation mine.
type orgAccess struct{ mine uuid.UUID }

func (a orgAccess) Check(_ context.Context, _ iam.Principal, perm iam.Permission, r iam.ResourceName) (iam.Decision, error) {
	return iam.Decision{Allowed: perm == iam.OrganizationsGet && r == iam.OrganizationName(a.mine)}, nil
}

func TestSuggestionCountry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mine, theirs := uuid.New(), uuid.New()
	countries := map[uuid.UUID]string{mine: "GA", theirs: "CG"}
	ctrl := &Controller{Access: orgAccess{mine: mine}, OrgCountry: func(_ context.Context, id uuid.UUID) (string, error) {
		return countries[id], nil
	}}
	ctx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/regions", nil)
		c.Set("userID", uuid.NewString())
		return c
	}

	cc, err := ctrl.suggestionCountry(ctx(), mine.String())
	require.NoError(t, err)
	assert.Equal(t, "GA", cc, "son organisation : son pays")

	cc, err = ctrl.suggestionCountry(ctx(), theirs.String())
	require.NoError(t, err)
	assert.Empty(t, cc, "organisation d'autrui : aucune suggestion, rien de révélé")

	cc, err = ctrl.suggestionCountry(ctx(), "")
	require.NoError(t, err)
	assert.Empty(t, cc, "sans organization_id")

	_, err = ctrl.suggestionCountry(ctx(), "pas-un-uuid")
	assert.Error(t, err)

	anonymous, _ := gin.CreateTestContext(httptest.NewRecorder())
	anonymous.Request = httptest.NewRequest("GET", "/regions", nil)
	cc, err = ctrl.suggestionCountry(anonymous, mine.String())
	require.NoError(t, err)
	assert.Empty(t, cc, "sans appelant identifié")
}
