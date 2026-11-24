package operatoraccess

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

const spec = `{"paths":{
 "/kmanager/v1/admin/support/tickets/{ticketID}":{"get":{"summary":"Un ticket","description":"Un ticket","parameters":[{"name":"ticketID","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}},
 "/kmanager/v1/admin/billing/revenue":{"get":{"summary":"Recettes","description":"Recettes","responses":{"200":{"description":"ok"}}}}
},"components":{"schemas":{}}}`

func catalog(t *testing.T, h http.Handler) *Catalog {
	t.Helper()
	cat, err := tools.Build([]tools.Route{
		{Name: "get_ticket", Method: "GET", Path: "/kmanager/v1/admin/support/tickets/:ticketID"},
		{Name: "get_revenue", Method: "GET", Path: "/kmanager/v1/admin/billing/revenue"},
	}, []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	return &Catalog{Tools: cat, Handler: h, Perms: map[string]iam.Permission{
		"get_ticket": iam.PlatformSupportGet, "get_revenue": iam.PlatformBillingAccountsGet}}
}

func operatorCtx(extra map[string]string) map[string]string {
	user := uuid.New()
	m := map[string]string{"authMethod": "delegated", "delegationActor": sts.ActorOperator, "delegationScope": "read",
		"internalUserID": user.String(), "iamPrincipal": string(iam.UserPrincipal(user))}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestToolsFilteredByPermission(t *testing.T) {
	h := &Controller{IAM: authz{grant: []iam.Permission{iam.PlatformOperatorUse, iam.PlatformSupportGet}}}
	if _, err := h.Tools(ctx(operatorCtx(nil))); err == nil {
		t.Fatal("catalogue absent : erreur attendue")
	}
	h.Bind(catalog(t, nil))
	out, err := h.Tools(ctx(operatorCtx(nil)))
	if err != nil || len(out.Items) != 1 || out.Items[0].Name != "get_ticket" || !strings.Contains(string(out.Items[0].Parameters), "ticketID") {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := (&Controller{IAM: authz{grant: []iam.Permission{iam.PlatformSupportGet}}}).Tools(ctx(operatorCtx(nil))); err == nil {
		t.Fatal("accès retiré accepté")
	}
	if _, err := h.Tools(ctx(operatorCtx(map[string]string{"delegationActor": sts.ActorCopilot}))); err == nil {
		t.Fatal("jeton du Copilot accepté")
	}
}

func TestCallReplaysWithTokenAndJustification(t *testing.T) {
	var got *http.Request
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"subject": "Site en panne"})
	})
	h := &Controller{}
	h.Bind(catalog(t, api))
	c := ctx(operatorCtx(nil))
	c.Request = httptest.NewRequest("POST", "/kmanager/v1/admin/operator/tools/get_ticket", nil)
	c.Request.Header.Set("Authorization", "Bearer deleg-tok")
	c.Request.Header.Set("X-Betorun-Justification", "ticket 4f2 : site en panne")
	ticket := uuid.NewString()
	out, err := h.Call(c, &CallInput{Name: "get_ticket", Arguments: json.RawMessage(`{"ticketID":"` + ticket + `"}`)})
	if err != nil || out.IsError || !strings.Contains(out.Content, "Site en panne") {
		t.Fatalf("%+v %v", out, err)
	}
	if got.URL.Path != "/kmanager/v1/admin/support/tickets/"+ticket || got.Method != "GET" ||
		got.Header.Get("Authorization") != "Bearer deleg-tok" || got.Header.Get("X-Betorun-Justification") != "ticket 4f2 : site en panne" {
		t.Fatalf("rejeu = %s %s %v", got.Method, got.URL, got.Header)
	}
	if _, err := h.Call(c, &CallInput{Name: "delete_everything"}); err == nil {
		t.Fatal("outil inconnu accepté")
	}
	if _, err := h.Call(ctx(map[string]string{"authMethod": "bearer"}), &CallInput{Name: "get_ticket"}); err == nil {
		t.Fatal("session acceptée")
	}
}
