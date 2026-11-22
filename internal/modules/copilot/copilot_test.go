package copilot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

const spec = `{"paths":{
 "/w/{id}":{"get":{"summary":"W","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/status":{"get":{"summary":"S","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/events":{"get":{"summary":"E","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/logs":{"get":{"summary":"L","parameters":[{"name":"id","in":"path","required":true}]}}}}`

func enabledService(t *testing.T) *service.Service {
	t.Helper()
	s := &service.Service{LLM: llmtest.New("p")}
	err := s.Bind(http.NotFoundHandler(), []tools.Route{
		{Name: tools.ToolGetWorkload, Method: "GET", Path: "/w/:id"},
		{Name: tools.ToolGetWorkloadStatus, Method: "GET", Path: "/w/:id/status"},
		{Name: tools.ToolGetWorkloadEvents, Method: "GET", Path: "/w/:id/events"},
		{Name: tools.ToolGetWorkloadLogs, Method: "GET", Path: "/w/:id/logs"},
	}, []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func chat(s *service.Service, body string, setup func(c *gin.Context)) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/copilot/chat", strings.NewReader(body))
	if setup != nil {
		setup(c)
	}
	NewController(s).Chat(c)
	return w
}

func person(c *gin.Context) {
	c.Set("authMethod", "bearer")
	c.Set("internalUserID", uuid.NewString())
	c.Set("sessionID", uuid.NewString())
}

func TestChatRefusals(t *testing.T) {
	if w := chat(&service.Service{}, `{"message":"x"}`, person); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "COPILOT_DISABLED") {
		t.Fatalf("désactivé : %d %s", w.Code, w.Body)
	}
	s := enabledService(t)
	// Une clé API, un compte de service ou un jeton délégué ne parlent pas au Copilot.
	for _, method := range []string{"api_key", "sts", "delegated"} {
		w := chat(s, `{"message":"x"}`, func(c *gin.Context) {
			person(c)
			c.Set("authMethod", method)
		})
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "COPILOT_PERSON_ONLY") {
			t.Fatalf("%s : %d %s", method, w.Code, w.Body)
		}
	}
	for _, body := range []string{`{"message":"   "}`, `{"message":"` + strings.Repeat("a", 4001) + `"}`, `pas du json`} {
		if w := chat(s, body, person); w.Code != http.StatusBadRequest {
			t.Fatalf("corps %q : %d", body[:min(len(body), 20)], w.Code)
		}
	}
}

func TestSSEFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	e := newSSE(c)
	e.Text("Bonjour\n")
	e.ToolStart("c1", "diagnose_workload")
	e.ToolEnd("c1", "diagnose_workload", false)
	got := w.Body.String()
	want := "event: delta\ndata: {\"text\":\"Bonjour\\n\"}\n\n" +
		"event: tool\ndata: {\"id\":\"c1\",\"name\":\"diagnose_workload\",\"status\":\"running\"}\n\n" +
		"event: tool\ndata: {\"id\":\"c1\",\"name\":\"diagnose_workload\",\"status\":\"failed\"}\n\n"
	if got != want || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("flux = %q", got)
	}
}
