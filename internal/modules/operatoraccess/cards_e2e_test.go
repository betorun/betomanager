package operatoraccess

// Cartes de l'Opérateur sur un vrai Postgres, ignoré sans IAM_E2E
// (bash scripts/iam_e2e_db.sh).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

const cardSpec = `{"paths":{
 "/kmanager/v1/admin/support/tickets/{ticketID}":{"get":{"summary":"Un ticket","description":"Un ticket","parameters":[{"name":"ticketID","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}},
 "/kmanager/v1/admin/support/tickets/{ticketID}/notes":{"post":{"summary":"Note interne","description":"Ajoute une note interne au ticket.","parameters":[{"name":"ticketID","in":"path","required":true,"schema":{"type":"string"}}],
   "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"body":{"type":"string"}},"required":["body"]}}}},"responses":{"201":{"description":"ok"}}}},
 "/kmanager/v1/admin/billing/organizations/{id}/credits":{"post":{"summary":"Crédit","description":"Accorde un crédit.","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
   "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"amount":{"type":"integer"}}}}}},"responses":{"201":{"description":"ok"}}}}
},"components":{"schemas":{}}}`

// routeRecorder : le moteur HTTP de l'API, réduit aux trois routes.
type routeRecorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	// stepUp : la route du crédit exige une confirmation d'identité récente.
	stepUp bool
}

func (r *routeRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Clone(req.Context()))
	stepUp := r.stepUp
	r.mu.Unlock()
	switch {
	case req.Method == "GET":
		_, _ = io.WriteString(w, `{"id":"4f2","subject":"Site en panne"}`)
	case strings.HasSuffix(req.URL.Path, "/credits") && stepUp:
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"code":"STEP_UP_REQUIRED","max_age":300}`)
	default:
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":"note-1"}`)
	}
}

func cardCatalog(t *testing.T, h http.Handler) *Catalog {
	t.Helper()
	cat, err := tools.Build([]tools.Route{
		{Name: "get_ticket", Method: "GET", Path: "/kmanager/v1/admin/support/tickets/:ticketID"},
		{Name: "add_ticket_note", Method: "POST", Path: "/kmanager/v1/admin/support/tickets/:ticketID/notes", Write: true, Risk: tools.RiskLow, TargetTool: "get_ticket"},
		{Name: "grant_credit", Method: "POST", Path: "/kmanager/v1/admin/billing/organizations/:id/credits", Write: true, Risk: tools.RiskHigh, Strong: true},
	}, []byte(cardSpec))
	if err != nil {
		t.Fatal(err)
	}
	return &Catalog{Tools: cat, Handler: h, Perms: map[string]iam.Permission{
		"get_ticket": iam.PlatformSupportGet, "add_ticket_note": iam.PlatformSupportManage, "grant_credit": iam.PlatformBillingAccountsUpdate}}
}

func ginCtx(method, path, body string, set map[string]string, header map[string]string, params ...gin.Param) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	for k, v := range header {
		c.Request.Header.Set(k, v)
	}
	for k, v := range set {
		c.Set(k, v)
	}
	c.Params = params
	return c, rec
}

func TestE2E_OperatorCards(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open("postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	engine := &routeRecorder{}
	m := &minter{}
	h := &Controller{STS: m, Cards: &Cards{DB: db},
		IAM: authz{grant: []iam.Permission{iam.PlatformOperatorUse, iam.PlatformSupportManage, iam.PlatformBillingAccountsUpdate}}}
	h.Bind(cardCatalog(t, engine))
	inv := uuid.New()

	// L'Opérateur propose (jeton délégué en lecture, motif du dossier).
	op := operatorCtx(nil)
	c, _ := ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-tok", "X-Betorun-Justification": "ticket 4f2 : site en panne"})
	card, err := h.Propose(c, &ProposeInput{Tool: "add_ticket_note", Arguments: json.RawMessage(`{"ticketID":"4f2","body":"Diagnostic : certificat expiré."}`), InvestigationID: &inv})
	if err != nil {
		t.Fatal(err)
	}
	if card.Target != "Site en panne" || card.Risk != "low" || card.Status != "proposed" || card.Details["body"] != "Diagnostic : certificat expiré." {
		t.Fatalf("carte : %+v", card)
	}
	if n := len(engine.reqs); n != 1 || engine.reqs[0].Method != "GET" {
		t.Fatalf("proposer n'exécute rien : %d requêtes", n)
	}
	// Une lecture n'est pas une carte ; une carte inconnue non plus.
	for _, tool := range []string{"get_ticket", "delete_everything"} {
		c, _ := ginCtx("POST", "/", "", op, nil)
		if _, err := h.Propose(c, &ProposeInput{Tool: tool, Arguments: json.RawMessage(`{}`)}); err == nil {
			t.Fatalf("%s accepté", tool)
		}
	}
	// Sans la permission de l'action : pas de carte.
	poor := &Controller{STS: m, Cards: h.Cards, IAM: authz{grant: []iam.Permission{iam.PlatformOperatorUse}}}
	poor.Bind(cardCatalog(t, engine))
	c, _ = ginCtx("POST", "/", "", op, nil)
	if _, err := poor.Propose(c, &ProposeInput{Tool: "add_ticket_note", Arguments: json.RawMessage(`{"ticketID":"4f2","body":"x"}`)}); err == nil {
		t.Fatal("carte sans la permission")
	}

	// L'Opérateur (jeton délégué) ne peut pas valider : il faut une session.
	param := gin.Param{Key: "cardID", Value: card.ID.String()}
	c, rec := ginCtx("POST", "/", "{}", op, nil, param)
	h.Approve(c)
	if rec.Code != 403 {
		t.Fatalf("validation par jeton délégué : %d %s", rec.Code, rec.Body)
	}

	// La personne valide avec sa session : jouée avec un jeton d'écriture
	// émis pour l'occasion, le motif du dossier et une clé d'idempotence.
	approver := uuid.New()
	person := map[string]string{"authMethod": "bearer", "internalUserID": approver.String(), "sessionID": uuid.NewString(),
		"iamPrincipal": string(iam.UserPrincipal(approver)), "email": "moussa@betorun.test"}
	c, rec = ginCtx("POST", "/", "{}", person, map[string]string{"Authorization": "Bearer session-tok"}, param)
	h.Approve(c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"executed"`) {
		t.Fatalf("validation : %d %s", rec.Code, rec.Body)
	}
	last := engine.reqs[len(engine.reqs)-1]
	if last.Method != "POST" || last.Header.Get("Authorization") != "Bearer tok" || last.Header.Get("X-Betorun-Justification") != "ticket 4f2 : site en panne" ||
		last.Header.Get("Idempotency-Key") != card.ID.String() || m.got.Scope != "write" || m.got.UserID != approver {
		t.Fatalf("requête jouée : %v ; délégation %+v", last.Header, m.got)
	}
	body, _ := io.ReadAll(last.Body)
	if !strings.Contains(string(body), "certificat expiré") {
		t.Fatalf("corps : %s", body)
	}
	// Une seule exécution.
	c, rec = ginCtx("POST", "/", "{}", person, nil, param)
	h.Approve(c)
	if rec.Code != 409 {
		t.Fatalf("seconde validation : %d", rec.Code)
	}
	// L'Opérateur relit l'issue.
	c, _ = ginCtx("GET", "/", "", op, nil)
	got, err := h.Card(c, &CardInput{CardID: card.ID})
	if err != nil || got.Status != "executed" || got.DecidedBy != "moussa@betorun.test" || got.HTTPStatus != 201 || got.Result != `{"id":"note-1"}` {
		t.Fatalf("issue : %+v %v", got, err)
	}

	// Carte sensible : la session de la personne, jamais un jeton émis ;
	// confirmation d'identité manquante : rien n'est fait, la carte reste.
	c, _ = ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-tok"})
	strong, err := h.Propose(c, &ProposeInput{Tool: "grant_credit", Arguments: json.RawMessage(`{"id":"org-1","amount":5000}`), InvestigationID: &inv})
	if err != nil || !strong.Strong || strong.Risk != "high" {
		t.Fatalf("carte sensible : %+v %v", strong, err)
	}
	engine.stepUp = true
	sp := gin.Param{Key: "cardID", Value: strong.ID.String()}
	c, rec = ginCtx("POST", "/", "{}", person, map[string]string{"Authorization": "Bearer session-tok"}, sp)
	h.Approve(c)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "STEP_UP_REQUIRED") {
		t.Fatalf("double authentification : %d %s", rec.Code, rec.Body)
	}
	if last := engine.reqs[len(engine.reqs)-1]; last.Header.Get("Authorization") != "Bearer session-tok" {
		t.Fatalf("jeton d'une carte sensible : %s", last.Header.Get("Authorization"))
	}
	c, _ = ginCtx("GET", "/", "", op, nil)
	if got, _ := h.Card(c, &CardInput{CardID: strong.ID}); got.Status != "proposed" {
		t.Fatalf("après la confirmation manquante : %s", got.Status)
	}
	engine.stepUp = false
	c, rec = ginCtx("POST", "/", "{}", person, map[string]string{"Authorization": "Bearer session-tok"}, sp)
	h.Approve(c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"executed"`) {
		t.Fatalf("après confirmation : %d %s", rec.Code, rec.Body)
	}

	// Refus relayé par l'Opérateur ; puis plus rien à valider.
	c, _ = ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-tok"})
	third, err := h.Propose(c, &ProposeInput{Tool: "add_ticket_note", Arguments: json.RawMessage(`{"ticketID":"4f2","body":"doublon"}`)})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = ginCtx("POST", "/", "", op, nil)
	if v, err := h.Reject(c, &RejectInput{CardID: third.ID, Note: "doublon"}); err != nil || v.Status != "rejected" || v.Message != "doublon" {
		t.Fatalf("refus : %+v %v", v, err)
	}
	c, rec = ginCtx("POST", "/", "{}", person, nil, gin.Param{Key: "cardID", Value: third.ID.String()})
	h.Approve(c)
	if rec.Code != 409 {
		t.Fatalf("validation d'une carte refusée : %d", rec.Code)
	}

	// Validation par une personne qui n'a pas le droit : la carte reste.
	c, _ = ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-tok"})
	fourth, _ := h.Propose(c, &ProposeInput{Tool: "add_ticket_note", Arguments: json.RawMessage(`{"ticketID":"4f2","body":"x"}`)})
	c, rec = ginCtx("POST", "/", "{}", person, nil, gin.Param{Key: "cardID", Value: fourth.ID.String()})
	poor.Approve(c)
	c2, _ := ginCtx("GET", "/", "", op, nil)
	if got, _ := h.Card(c2, &CardInput{CardID: fourth.ID}); rec.Code != 403 || got.Status != "proposed" {
		t.Fatalf("sans le droit : %d %s", rec.Code, got.Status)
	}
}

// Carte restée « en cours » (API arrêtée pendant l'action) : passée en échec
// après le délai, pas avant.
func TestE2E_OperatorCardsReaper(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open("postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	h := &Controller{STS: &minter{}, Cards: &Cards{DB: db},
		IAM: authz{grant: []iam.Permission{iam.PlatformOperatorUse, iam.PlatformSupportManage}}}
	h.Bind(cardCatalog(t, &routeRecorder{}))
	c, _ := ginCtx("POST", "/", "", operatorCtx(nil), nil)
	card, err := h.Propose(c, &ProposeInput{Tool: "add_ticket_note", Arguments: json.RawMessage(`{"ticketID":"4f2","body":"reaper"}`)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Exec(`UPDATE operator_cards SET status='executing', decided_at=? WHERE id=?`, now.Add(-time.Minute), card.ID).Error; err != nil {
		t.Fatal(err)
	}
	after := cardExecTimeout + time.Minute
	if n, err := h.Cards.ReapExecuting(t.Context(), after); err != nil || n != 0 {
		t.Fatalf("ramassée trop tôt : %d %v", n, err)
	}
	h.Cards.Now = func() time.Time { return now.Add(after + time.Minute) }
	if n, err := h.Cards.ReapExecuting(t.Context(), after); err != nil || n < 1 {
		t.Fatalf("ramasseur : %d %v", n, err)
	}
	var row struct{ Status, Message string }
	if err := db.Raw(`SELECT status, message FROM operator_cards WHERE id=?`, card.ID).Scan(&row).Error; err != nil ||
		row.Status != "failed" || row.Message != cardInterrupted {
		t.Fatalf("carte : %+v %v", row, err)
	}
}
