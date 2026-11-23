package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	supportDomain "github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

// Les routes de fichiers du support (hors tonic) rendent les erreurs au format habituel.
func TestWriteClassifiedError_Support(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		supportDomain.ErrAttachmentType:     {http.StatusBadRequest, "ATTACHMENT_TYPE_NOT_ALLOWED"},
		supportDomain.ErrAttachmentTooLarge: {http.StatusRequestEntityTooLarge, "ATTACHMENT_TOO_LARGE"},
		supportDomain.ErrTicketNotFound:     {http.StatusNotFound, "NOT_FOUND"},
		supportDomain.ErrTicketClosed:       {http.StatusConflict, "TICKET_CLOSED"},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		writeClassifiedError(c, err)
		assert.Equal(t, want.status, rec.Code, err.Error())
		var body ErrorBody
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, want.code, body.Code)
		assert.Equal(t, err.Error(), body.Message)
	}
}
