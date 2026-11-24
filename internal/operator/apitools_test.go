package operator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/operator/kube"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// fakeAdminAPI : /admin/operator/tools et /admin/operator/tools/:name.
func fakeAdminAPI(t *testing.T, seen *[]*http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Clone(context.Background()))
		if r.Header.Get("Authorization") == "Bearer expired" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/kmanager/v1/admin/operator/tools":
			_, _ = io.WriteString(w, `{"items":[{"name":"get_ticket","description":"Un ticket","parameters":{"type":"object","properties":{"ticketID":{"type":"string"}},"required":["ticketID"]}}]}`)
		case r.Method == "POST" && r.URL.Path == "/kmanager/v1/admin/operator/tools/get_ticket":
			var in struct {
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if r.Header.Get("X-Betorun-Justification") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"content": "HTTP 400 : justification required", "is_error": true})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"content": `{"subject":"Site en panne","id":"` + in.Arguments["ticketID"] + `"}`})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPITools(t *testing.T) {
	var seen []*http.Request
	api := &APITools{BaseURL: fakeAdminAPI(t, &seen).URL + "/kmanager/v1"}
	ctx := context.Background()
	r, err := api.For(ctx, Identity{Token: "tok"}, "ticket 4f2 : site en panne")
	if err != nil || len(r.Definitions()) != 1 || r.Definitions()[0].Name != "get_ticket" {
		t.Fatalf("%v %v", r, err)
	}
	res := r.Run(ctx, "get_ticket", json.RawMessage(`{"ticketID":"4f2"}`))
	if res.IsError || !strings.Contains(res.Content, "Site en panne") || !strings.Contains(res.Content, "4f2") {
		t.Fatalf("%+v", res)
	}
	last := seen[len(seen)-1]
	if last.Header.Get("Authorization") != "Bearer tok" || last.Header.Get("X-Betorun-Justification") != "ticket 4f2 : site en panne" {
		t.Fatalf("en-têtes = %v", last.Header)
	}

	// Sans motif : l'API refuse la lecture des données du client.
	r2, _ := api.For(ctx, Identity{Token: "tok"}, "")
	if res := r2.Run(ctx, "get_ticket", json.RawMessage(`{"ticketID":"4f2"}`)); !res.IsError {
		t.Fatal("lecture sans motif acceptée")
	}
	if _, err := api.For(ctx, Identity{Token: "expired"}, ""); err == nil {
		t.Fatal("catalogue avec jeton expiré")
	}
	if res := (&apiRunner{api: api, token: "expired"}).Run(ctx, "get_ticket", nil); !res.IsError || !strings.Contains(res.Content, "expiré") {
		t.Fatalf("%+v", res)
	}
	if _, err := (&APITools{}).For(ctx, Identity{}, ""); err == nil {
		t.Fatal("sans API")
	}
}

func TestCombineRejectsDuplicates(t *testing.T) {
	k := kube.New(nil)
	if _, err := Combine(k, k); err == nil {
		t.Fatal("doublon accepté")
	}
	set, err := Combine(nil, k)
	if err != nil || len(set.Definitions()) != len(k.Definitions()) {
		t.Fatal(err)
	}
	if res := set.Run(context.Background(), "inconnu", nil); !res.IsError {
		t.Fatal("outil inconnu")
	}
}

// Le support reçoit les outils de l'API (selon ses droits), pas le chemin
// de secours ; l'administrateur reçoit les deux.
func TestServerCombinesAPIAndInfrastructure(t *testing.T) {
	var seen []*http.Request
	model := llmtest.New("claude", llmtest.Say("ok"), llmtest.Say("ok"))
	iv, m := setup(model)
	s := &Server{Store: m, Auth: fakeAuth{"a": admin, "s": support}, Investigator: iv, Infrastructure: clusters,
		API: &APITools{BaseURL: fakeAdminAPI(t, &seen).URL + "/kmanager/v1"}, Logger: quietLogger()}
	h := httptest.NewServer(s.Handler())
	defer h.Close()

	for i, who := range []struct {
		token string
		id    Identity
		infra bool
	}{{"s", support, false}, {"a", admin, true}} {
		m.inv = store.Investigation{ID: m.inv.ID, ActorUserID: who.id.UserID.String(), Status: store.StatusOpen, Reason: "ticket 4f2 : site en panne"}
		r, body := call(t, h, "POST", "/v1/investigations/"+m.inv.ID.String()+"/messages", who.token, `{"message":"État ?"}`)
		if r.StatusCode != 200 || !strings.Contains(body, "event: done") {
			t.Fatalf("%s : %d %s", who.token, r.StatusCode, body)
		}
		names := map[string]bool{}
		for _, d := range model.Requests()[i].Tools {
			names[d.Name] = true
		}
		if !names["get_ticket"] || names["k8s_pods"] != who.infra {
			t.Fatalf("%s : outils %v", who.token, names)
		}
	}
}
