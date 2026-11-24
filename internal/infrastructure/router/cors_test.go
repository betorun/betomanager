package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// Le back-office (http://localhost:3001) appelle l'API plateforme d'une autre
// origine avec le motif staff : la pré-vérification doit l'accepter.
func TestCORS_PreflightAllowsJustification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(cors.New(corsConfig([]string{"http://localhost:3001"})))
	engine.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "http://localhost:3001")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization,x-betorun-justification")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code >= 300 {
		t.Fatalf("pré-vérification refusée : %d", w.Code)
	}
	if !strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), "x-betorun-justification") {
		t.Fatalf("motif absent des en-têtes autorisés : %q", w.Header().Get("Access-Control-Allow-Headers"))
	}
}
