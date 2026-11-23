package copilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/agent"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/grounding"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

func verifyHarness(t *testing.T, p *llmtest.Provider) (*Controller, *agent.Agent, *service.BoundRunner, *sse, *httptest.ResponseRecorder, *grounding.Evidence) {
	t.Helper()
	s := enabledService(t)
	s.Logger = zap.NewNop().Sugar()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/copilot/chat", nil)
	ev := grounding.NewEvidence()
	ev.Add(`{"address":"203.0.113.7","egress_ip_monthly":7300}`)
	r := &service.BoundRunner{Cat: s.Catalog, Inv: tools.Invoker{Handler: s.Handler}, Svc: s, Evidence: ev}
	return NewController(s), &agent.Agent{LLM: p}, r, newSSE(c), w, ev
}

func TestVerify_CleanAnswerUntouched(t *testing.T) {
	h, ag, r, sse, w, ev := verifyHarness(t, llmtest.New("p"))
	out := h.verify(context.Background(), ag, agent.Turn{Message: "ip ?", Caller: llm.Caller{OrgID: "u"}}, r, sse,
		"Vos conteneurs sortent par 203.0.113.7 (7 300 XAF par mois).", ev, "fr")
	if !strings.Contains(out, "203.0.113.7") || strings.Contains(w.Body.String(), "event: replace") {
		t.Fatalf("réponse prouvée modifiée : %q / %s", out, w.Body)
	}
}

func TestVerify_RevisedByModel(t *testing.T) {
	p := llmtest.New("p", llmtest.Say("Vos conteneurs sortent par 203.0.113.7."))
	h, ag, r, sse, w, ev := verifyHarness(t, p)
	out := h.verify(context.Background(), ag, agent.Turn{Message: "ip ?", Caller: llm.Caller{OrgID: "u"}}, r, sse,
		"Vos conteneurs sortent par 198.51.100.9.", ev, "fr")
	if out != "Vos conteneurs sortent par 203.0.113.7." {
		t.Fatalf("réponse : %q", out)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: revising") || !strings.Contains(body, "event: replace") {
		t.Fatalf("événements : %s", body)
	}
	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("une seule réécriture : %d", len(reqs))
	}
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	if !strings.Contains(last.Text, "198.51.100.9") || !strings.Contains(last.Text, "VERIFICATION FAILED") {
		t.Fatalf("consigne : %q", last.Text)
	}
}

func TestVerify_SanitizedWhenModelInsists(t *testing.T) {
	p := llmtest.New("p", llmtest.Say("Toujours 198.51.100.9, voir [Pare-feu](/firewall)."))
	h, ag, r, sse, _, ev := verifyHarness(t, p)
	out := h.verify(context.Background(), ag, agent.Turn{Message: "ip ?", Caller: llm.Caller{OrgID: "u"}}, r, sse,
		"Vos conteneurs sortent par 198.51.100.9.", ev, "fr")
	if strings.Contains(out, "198.51.100.9") || strings.Contains(out, "](/firewall)") {
		t.Fatalf("invention restée : %q", out)
	}
	if !strings.Contains(out, "(non vérifié)") || !strings.Contains(out, "Pare-feu") {
		t.Fatalf("réponse : %q", out)
	}
}
