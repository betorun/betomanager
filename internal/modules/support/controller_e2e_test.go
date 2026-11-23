package support

// Envoi et téléchargement d'une pièce jointe par HTTP (routes hors tonic), sur
// un vrai Postgres. Ignoré sans IAM_E2E (voir repository/repository_e2e_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
	"github.com/thekrauss/kubemanager/internal/modules/support/repository"
	"github.com/thekrauss/kubemanager/internal/modules/support/service"
)

type memFiles map[string][]byte

func (f memFiles) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	f[key] = b
	return err
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func (f memFiles) Get(_ context.Context, key string) (objectstore.Object, objectstore.ObjectInfo, error) {
	b, ok := f[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return nopCloser{bytes.NewReader(b)}, objectstore.ObjectInfo{Size: int64(len(b))}, nil
}

func TestE2E_AttachmentOverHTTP(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(supportE2EDSN()),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	org, user := uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e support http')`, org).Error)
	// Le routeur branche sa table d'erreurs (router.writeClassifiedError) ; ici, l'essentiel.
	h := &Controller{Service: &service.Service{Store: &repository.GormStore{DB: db}, Files: memFiles{}},
		WriteError: func(c *gin.Context, err error) {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, domain.ErrAttachmentType):
				status = http.StatusBadRequest
			case errors.Is(err, domain.ErrTicketNotFound):
				status = http.StatusNotFound
			}
			c.AbortWithStatusJSON(status, gin.H{"code": err.Error()})
		}}
	v, err := h.Service.Create(context.Background(), org, service.ActorOf("user:"+user.String()),
		service.CreateInput{Subject: "Capture", Description: "Voir la pièce jointe"})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/organizations/:id/support/tickets/:ticketID/attachments", func(c *gin.Context) {
		h.Attach(c, &TicketPath{OrganizationID: c.Param("id"), TicketID: c.Param("ticketID")})
	})
	r.GET("/organizations/:id/support/tickets/:ticketID/attachments/:attachmentID", func(c *gin.Context) {
		h.Download(c, &AttachmentPath{OrganizationID: c.Param("id"), TicketID: c.Param("ticketID"), AttachmentID: c.Param("attachmentID")})
	})
	upload := func(name string, content []byte) *httptest.ResponseRecorder {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		fw, _ := w.CreateFormFile(AttachmentFormField, name)
		_, _ = fw.Write(content)
		require.NoError(t, w.Close())
		req := httptest.NewRequest(http.MethodPost, "/organizations/"+org.String()+"/support/tickets/"+v.Ticket.ID.String()+"/attachments", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := upload("page.html", []byte("<html><script>x</script></html>"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	rec = upload("écran \"1\".png", png)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out service.TicketView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Attachments, 1)
	a := out.Attachments[0]
	assert.Equal(t, "image/png", a.MimeType)

	req := httptest.NewRequest(http.MethodGet, "/kmanager/v1"+a.URL, nil)
	req.URL.Path = a.URL
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, png, rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// Autre organisation : introuvable.
	req = httptest.NewRequest(http.MethodGet, "/organizations/"+uuid.NewString()+"/support/tickets/"+v.Ticket.ID.String()+"/attachments/"+a.ID.String(), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// supportE2EDSN : base des tests (IAM_E2E_DSN pour en viser une autre).
func supportE2EDSN() string {
	if d := os.Getenv("IAM_E2E_DSN"); d != "" {
		return d
	}
	return "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"
}
