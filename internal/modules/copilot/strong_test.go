package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// strongAPI : DELETE exige une session confirmée (jeton « person-mfa ») ;
// sinon 401 STEP_UP_REQUIRED, comme le garde IAM d'une action sensible.
type strongAPI struct {
	mu   sync.Mutex
	reqs []string
}

func (a *strongAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	a.mu.Unlock()
	switch {
	case r.Method == http.MethodGet:
		fmt.Fprint(w, `{"name":"api-prod"}`)
	case r.Header.Get("Authorization") != "Bearer person-mfa":
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":"STEP_UP_REQUIRED","message":"recent two-factor authentication required","max_age":300}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

const strongSpec = `{"paths":{
 "/w/{id}":{"get":{"summary":"W","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/status":{"get":{"summary":"S","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/events":{"get":{"summary":"E","parameters":[{"name":"id","in":"path","required":true}]}},
 "/w/{id}/logs":{"get":{"summary":"L","parameters":[{"name":"id","in":"path","required":true}]}},
 "/workloads/{id}":{"get":{"summary":"Get","parameters":[{"name":"id","in":"path","required":true}]},
   "delete":{"summary":"Delete","parameters":[{"name":"id","in":"path","required":true}]}}}}`

func strongService(t *testing.T) (*service.Service, *strongAPI, *fakeMinter) {
	t.Helper()
	a, m := &strongAPI{}, &fakeMinter{}
	s := &service.Service{LLM: llmtest.New("p"), STS: m, Proposals: newMem(), Logger: zap.NewNop().Sugar()}
	err := s.Bind(a, []tools.Route{
		{Name: "get_workload_x", Method: "GET", Path: "/workloads/:id"},
		{Name: tools.ToolGetWorkload, Method: "GET", Path: "/w/:id"},
		{Name: tools.ToolGetWorkloadStatus, Method: "GET", Path: "/w/:id/status"},
		{Name: tools.ToolGetWorkloadEvents, Method: "GET", Path: "/w/:id/events"},
		{Name: tools.ToolGetWorkloadLogs, Method: "GET", Path: "/w/:id/logs"},
		{Name: "delete_workload", Method: "DELETE", Path: "/workloads/:id", Write: true, Risk: tools.RiskHigh,
			Strong: true, ConfirmName: true, TargetTool: "get_workload_x"},
	}, []byte(strongSpec), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, a, m
}

func approve(s *service.Service, token string, user, session uuid.UUID, bearer, confirm string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]string{"confirm": confirm})
	c.Request = httptest.NewRequest(http.MethodPost, "/copilot/actions/"+token+"/approve", bytes.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer "+bearer)
	c.Params = gin.Params{{Key: "token", Value: token}}
	c.Set("authMethod", "bearer")
	c.Set("internalUserID", user.String())
	c.Set("sessionID", session.String())
	NewController(s).Approve(c)
	return w
}

// Carte sensible : nom à retaper, puis double authentification ; jouée avec
// la session de la personne, jamais un jeton délégué.
func TestStrongCard(t *testing.T) {
	s, a, m := strongService(t)
	user, session := uuid.New(), uuid.New()
	var views []service.View
	r := runner(s, user.String(), &views)
	if res := r.Run(context.Background(), "delete_workload", json.RawMessage(`{"id":"w1"}`)); res.IsError {
		t.Fatalf("proposition : %s", res.Content)
	}
	if len(views) != 1 || !views[0].Strong || !views[0].ConfirmName || views[0].Target != "api-prod" || views[0].Risk != tools.RiskHigh {
		t.Fatalf("carte : %+v", views)
	}
	token := views[0].Token

	// Mauvais nom : refusé, la carte reste validable, rien n'est joué.
	if w := approve(s, token, user, session, "person", "api"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "CONFIRMATION_MISMATCH") {
		t.Fatalf("nom faux : %d %s", w.Code, w.Body)
	}
	// Bon nom, session sans confirmation récente : STEP_UP_REQUIRED rendu tel quel.
	w := approve(s, token, user, session, "person", "api-prod")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"STEP_UP_REQUIRED"`) || !strings.Contains(w.Body.String(), `"max_age":300`) {
		t.Fatalf("confirmation demandée : %d %s", w.Code, w.Body)
	}
	// Après la double authentification (nouveau jeton) : faite, une fois.
	w = approve(s, token, user, session, "person-mfa", "api-prod")
	var out ActionResult
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || out.Status != "done" {
		t.Fatalf("après confirmation : %d %s", w.Code, w.Body)
	}
	if w := approve(s, token, user, session, "person-mfa", "api-prod"); w.Code == http.StatusOK {
		t.Fatal("carte jouée deux fois")
	}
	for _, sc := range m.scopes {
		if sc == "write" {
			t.Fatal("jeton délégué en écriture pour une carte sensible")
		}
	}
	deletes := 0
	for _, req := range a.reqs {
		if strings.HasPrefix(req, "DELETE") {
			deletes++
			if !strings.HasSuffix(req, "Bearer person") && !strings.HasSuffix(req, "Bearer person-mfa") {
				t.Fatalf("suppression jouée sans la session de la personne : %s", req)
			}
		}
	}
	if deletes != 2 {
		t.Fatalf("suppressions jouées : %d (refusée puis faite)", deletes)
	}
}
