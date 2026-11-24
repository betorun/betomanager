package operatoraccess

// Outils et cartes de l'Opérateur sur les ressources d'un client (ADR 0017
// §5) : ouverts par un accès d'assistance actif, contrôlés par la route
// (jamais par une permission sur platform). Les cartes : vrai Postgres,
// ignoré sans IAM_E2E (bash scripts/iam_e2e_db.sh).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

const customerSpec = `{"paths":{
 "/kmanager/v1/admin/support/tickets/{ticketID}":{"get":{"summary":"Un ticket","description":"Un ticket","parameters":[{"name":"ticketID","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}},
 "/kmanager/v1/workloads/{id}":{"get":{"summary":"Un conteneur","description":"Un conteneur","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}},
 "/kmanager/v1/workloads/{id}/restart":{"post":{"summary":"Redémarrer","description":"Redémarre le conteneur.","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}}
},"components":{"schemas":{}}}`

// assistanceStub : accès d'assistance actif ou non.
type assistanceStub struct{ active bool }

func (a assistanceStub) HasActive(context.Context, uuid.UUID) (bool, error) { return a.active, nil }

// customerAPI : la route du client accepte seulement les jetons des
// personnes qui ont un accès d'assistance (ici : la liste allowed).
type customerAPI struct {
	mu      sync.Mutex
	allowed map[string]bool
	reqs    []*http.Request
}

func (a *customerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r.Clone(r.Context()))
	ok := a.allowed[r.Header.Get("Authorization")]
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case !ok:
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"code":"FORBIDDEN","message":"denied"}`)
	case r.Method == "GET":
		_, _ = io.WriteString(w, `{"id":"w-1","name":"boutique-api"}`)
	default:
		_, _ = io.WriteString(w, `{"status":"restarting"}`)
	}
}

func customerCatalog(t *testing.T, h http.Handler) *Catalog {
	t.Helper()
	cat, err := tools.Build([]tools.Route{
		{Name: "get_ticket", Method: "GET", Path: "/kmanager/v1/admin/support/tickets/:ticketID"},
		{Name: "customer_get_workload", Method: "GET", Path: "/kmanager/v1/workloads/:id"},
		{Name: "customer_restart_workload", Method: "POST", Path: "/kmanager/v1/workloads/:id/restart", Write: true, Risk: tools.RiskLow,
			TargetTool: "customer_get_workload"},
	}, []byte(customerSpec))
	if err != nil {
		t.Fatal(err)
	}
	return &Catalog{Tools: cat, Handler: h,
		Perms: map[string]iam.Permission{"get_ticket": iam.PlatformSupportGet,
			"customer_get_workload": iam.WorkloadsGet, "customer_restart_workload": iam.WorkloadsRestart},
		Customer: map[string]bool{"customer_get_workload": true, "customer_restart_workload": true}}
}

func TestToolsCustomerNeedAssistance(t *testing.T) {
	// Aucune permission de client sur platform : seul l'accès d'assistance
	// ouvre les outils du client.
	grant := authz{grant: []iam.Permission{iam.PlatformOperatorUse, iam.PlatformSupportGet}}
	names := func(a AssistanceLookup, withCards bool) (items, cards []string) {
		h := &Controller{IAM: grant, Assistance: a}
		if withCards {
			h.Cards = &Cards{}
		}
		h.Bind(customerCatalog(t, nil))
		out, err := h.Tools(ctx(operatorCtx(nil)))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range out.Items {
			items = append(items, v.Name)
		}
		for _, v := range out.Cards {
			cards = append(cards, v.Name)
		}
		return items, cards
	}
	for _, a := range []AssistanceLookup{nil, assistanceStub{}} {
		if items, cards := names(a, true); strings.Join(items, ",") != "get_ticket" || len(cards) != 0 {
			t.Fatalf("sans accès d'assistance : %v %v", items, cards)
		}
	}
	items, cards := names(assistanceStub{active: true}, true)
	if strings.Join(items, ",") != "customer_get_workload,get_ticket" && strings.Join(items, ",") != "get_ticket,customer_get_workload" {
		t.Fatalf("lectures : %v", items)
	}
	if strings.Join(cards, ",") != "customer_restart_workload" {
		t.Fatalf("cartes : %v", cards)
	}
}

func TestE2E_OperatorCustomerCards(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open("postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	api := &customerAPI{allowed: map[string]bool{"Bearer read-ok": true, "Bearer session-ok": true, "Bearer tok": true}}
	m := &minter{}
	// Seulement l'usage de l'Opérateur sur platform : aucune permission de
	// conteneur. La carte passe quand même : c'est la route qui décide.
	h := &Controller{STS: m, Cards: &Cards{DB: db}, Assistance: assistanceStub{active: true},
		IAM: authz{grant: []iam.Permission{iam.PlatformOperatorUse}}}
	h.Bind(customerCatalog(t, api))
	op := operatorCtx(nil)
	args := json.RawMessage(`{"id":"w-1"}`)

	// Sans accès sur l'organisation de la ressource : la lecture de la cible
	// est refusée, pas de carte, et le modèle sait quoi faire.
	c, _ := ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-no"})
	if _, err := h.Propose(c, &ProposeInput{Tool: "customer_restart_workload", Arguments: args}); err == nil ||
		!strings.Contains(err.Error(), "request_assistance") {
		t.Fatalf("sans accès : %v", err)
	}
	// Opérateur retiré : rien.
	gone := &Controller{STS: m, Cards: h.Cards, Assistance: assistanceStub{active: true}, IAM: authz{}}
	gone.Bind(customerCatalog(t, api))
	c, _ = ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-ok"})
	if _, err := gone.Propose(c, &ProposeInput{Tool: "customer_restart_workload", Arguments: args}); err == nil {
		t.Fatal("carte sans l'usage de l'Opérateur")
	}

	c, _ = ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-ok"})
	card, err := h.Propose(c, &ProposeInput{Tool: "customer_restart_workload", Arguments: args})
	if err != nil || card.Target != "boutique-api" || card.Status != "proposed" {
		t.Fatalf("carte : %+v %v", card, err)
	}
	for _, r := range api.reqs {
		if r.Method != "GET" {
			t.Fatalf("proposer n'exécute rien : %s %s", r.Method, r.URL)
		}
	}

	// Un collègue sans accès d'assistance valide : refusé, la carte reste.
	param := gin.Param{Key: "cardID", Value: card.ID.String()}
	approver := uuid.New()
	person := map[string]string{"authMethod": "bearer", "internalUserID": approver.String(), "sessionID": uuid.NewString(),
		"iamPrincipal": string(iam.UserPrincipal(approver)), "email": "awa@betorun.test"}
	c, rec := ginCtx("POST", "/", "{}", person, map[string]string{"Authorization": "Bearer session-no"}, param)
	h.Approve(c)
	if rec.Code != 403 {
		t.Fatalf("validation sans accès : %d %s", rec.Code, rec.Body)
	}
	c, _ = ginCtx("GET", "/", "", op, nil)
	if got, _ := h.Card(c, &CardInput{CardID: card.ID}); got.Status != "proposed" {
		t.Fatalf("après un refus de validation : %s", got.Status)
	}

	// Avec son accès : jouée une fois, avec un jeton délégué en écriture.
	c, rec = ginCtx("POST", "/", "{}", person, map[string]string{"Authorization": "Bearer session-ok"}, param)
	h.Approve(c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"executed"`) {
		t.Fatalf("validation : %d %s", rec.Code, rec.Body)
	}
	last := api.reqs[len(api.reqs)-1]
	if last.Method != "POST" || last.URL.Path != "/kmanager/v1/workloads/w-1/restart" || last.Header.Get("Authorization") != "Bearer tok" ||
		m.got.Scope != "write" || m.got.UserID != approver {
		t.Fatalf("requête jouée : %s %s %v ; délégation %+v", last.Method, last.URL, last.Header, m.got)
	}

	// Accès retiré entre la proposition et la validation : la route refuse
	// à l'exécution, la carte échoue et rien n'est fait.
	c, _ = ginCtx("POST", "/", "", op, map[string]string{"Authorization": "Bearer read-ok"})
	second, err := h.Propose(c, &ProposeInput{Tool: "customer_restart_workload", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	delete(api.allowed, "Bearer tok")
	api.mu.Unlock()
	c, rec = ginCtx("POST", "/", "{}", person, map[string]string{"Authorization": "Bearer session-ok"},
		gin.Param{Key: "cardID", Value: second.ID.String()})
	h.Approve(c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"failed"`) || !strings.Contains(rec.Body.String(), `"http_status":403`) {
		t.Fatalf("accès retiré : %d %s", rec.Code, rec.Body)
	}
}
