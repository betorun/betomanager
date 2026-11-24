package operator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// fakeCardsAPI : catalogue avec une carte, et les routes des cartes de l'API.
type fakeCardsAPI struct {
	mu       sync.Mutex
	status   map[string]string // carte → statut
	proposed []map[string]any
	reasons  []string
	rejects  []string
}

func (f *fakeCardsAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/kmanager/v1")
		switch {
		case r.Method == "GET" && p == "/admin/operator/tools":
			_, _ = io.WriteString(w, `{"items":[],"cards":[{"name":"add_ticket_note","description":"Ajoute une note interne.",
				"parameters":{"type":"object","properties":{"ticketID":{"type":"string"},"body":{"type":"string"}},"required":["ticketID","body"]},
				"risk":"low","strong":false}]}`)
		case r.Method == "POST" && p == "/admin/operator/cards":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.proposed = append(f.proposed, in)
			f.reasons = append(f.reasons, r.Header.Get("X-Betorun-Justification"))
			id := uuid.NewString()
			f.status[id] = "proposed"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "tool": in["tool"], "summary": "Ajoute une note interne.",
				"target": "Site en panne", "risk": "low", "strong": false, "status": "proposed", "details": map[string]any{"body": "vu"}})
		case r.Method == "GET" && strings.HasPrefix(p, "/admin/operator/cards/"):
			id := strings.TrimPrefix(p, "/admin/operator/cards/")
			st, ok := f.status[id]
			if !ok {
				w.WriteHeader(404)
				return
			}
			out := map[string]any{"id": id, "tool": "add_ticket_note", "status": st, "decided_by": "awa@betorun.test"}
			if st == "executed" {
				out["http_status"], out["result"] = 201, `{"id":"note-1"}`
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && strings.HasSuffix(p, "/reject"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/admin/operator/cards/"), "/reject")
			if f.status[id] != "proposed" {
				w.WriteHeader(409)
				return
			}
			var in struct{ Note string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.status[id] = "rejected"
			f.rejects = append(f.rejects, in.Note)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "rejected"})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPICards(t *testing.T) {
	rationale := "Le client attend une réponse depuis deux jours ; la note trace le diagnostic pour l'équipe."
	model := llmtest.New("claude",
		llmtest.Call("c1", "propose_api_add_ticket_note", `{"ticketID":"4f2","body":"vu","rationale":"`+rationale+`"}`),
		llmtest.Say("J'ai proposé une note."),
		llmtest.Call("c2", "propose_api_add_ticket_note", `{"ticketID":"4f2","body":"autre","rationale":"`+rationale+`"}`),
		llmtest.Say("Deuxième note proposée."),
		llmtest.Say("Les deux issues sont connues."))
	fake := &fakeCardsAPI{status: map[string]string{}}
	api := fake.server(t)
	iv, m := setup(model)
	cards := &memCards{now: time.Now()}
	s := &Server{Store: m, Auth: fakeAuth{"a": withToken(admin, "tok")}, Investigator: iv, Cards: cards,
		API: &APITools{BaseURL: api.URL + "/kmanager/v1"}, Model: "m", Origins: []string{"http://localhost:3001"}, Logger: quietLogger()}
	srv := httptestServer(t, s)
	m.inv = store.Investigation{ID: uuid.New(), SubjectKind: store.SubjectTicket, Reason: "ticket 4f2 : site en panne",
		ActorUserID: admin.UserID.String(), ActorEmail: admin.Email, Status: store.StatusOpen}
	path := "/v1/investigations/" + m.inv.ID.String()
	do := func(method, p, body string) (int, string) {
		r, b := call(t, srv, method, p, "a", body)
		return r.StatusCode, b
	}

	code, body := do("POST", path+"/messages", `{"message":"Note le diagnostic sur le ticket"}`)
	if code != 200 || !strings.Contains(body, "event: card") || !strings.Contains(body, `"executor":"api"`) || !strings.Contains(body, `"api_card":"`) ||
		!strings.Contains(body, `"details":{"body":"vu"}`) {
		t.Fatalf("flux : %s", body)
	}
	if len(fake.proposed) != 1 || fake.proposed[0]["tool"] != "add_ticket_note" || fake.reasons[0] != "ticket 4f2 : site en panne" ||
		fake.proposed[0]["investigation_id"] != m.inv.ID.String() {
		t.Fatalf("proposition à l'API : %+v %v", fake.proposed, fake.reasons)
	}
	if args, _ := fake.proposed[0]["arguments"].(map[string]any); args["rationale"] != nil || args["body"] != "vu" {
		t.Fatalf("arguments : %v", args)
	}
	first := cards.cards[0]
	if first.Target != "Site en panne" || first.Risk != "low" {
		t.Fatalf("carte : %+v", first)
	}

	// L'Opérateur ne valide pas une carte de l'API : il renvoie à l'API.
	if code, body := do("POST", path+"/cards/"+first.ID.String()+"/approve", `{}`); code != 409 || !strings.Contains(body, "APPROVE_AT_API") {
		t.Fatalf("validation par l'Opérateur : %d %s", code, body)
	}

	// Validée dans la console, auprès de l'API : la relecture du dossier l'inscrit.
	var apiID string
	for id := range fake.status {
		apiID = id
	}
	fake.mu.Lock()
	fake.status[apiID] = "executed"
	fake.mu.Unlock()
	code, body = do("GET", path, "")
	if code != 200 || !strings.Contains(body, `"status":"executed"`) || !strings.Contains(body, "validée par awa@betorun.test auprès de l'API : exécutée") {
		t.Fatalf("dossier : %s", body)
	}
	if k := journalKinds(m); !strings.Contains(k, "card.proposed") || !strings.Contains(k, "card.executed") {
		t.Fatalf("journal : %s", k)
	}

	// Une deuxième carte, refusée depuis l'Opérateur : relayé à l'API.
	if code, _ := do("POST", path+"/messages", `{"message":"Ajoute une autre note"}`); code != 200 {
		t.Fatal(code)
	}
	second := cards.cards[1]
	code, body = do("POST", path+"/cards/"+second.ID.String()+"/reject", `{"note":"doublon"}`)
	if code != 200 || !strings.Contains(body, `"status":"rejected"`) || len(fake.rejects) != 1 || fake.rejects[0] != "doublon" {
		t.Fatalf("refus : %d %s %v", code, body, fake.rejects)
	}

	// Le tour suivant reçoit les deux issues.
	if code, _ := do("POST", path+"/messages", `{"message":"Bilan ?"}`); code != 200 {
		t.Fatal(code)
	}
	reqs := model.Requests()
	msgs := reqs[len(reqs)-1].Messages
	if q := msgs[len(msgs)-1].Text; !strings.Contains(q, "refusée par admin@betorun.test") || !strings.Contains(q, "doublon") {
		t.Fatalf("question : %q", q)
	}
}

func TestAPICardsNotForRescue(t *testing.T) {
	if (&APITools{BaseURL: "http://x"}).APICards(Identity{Rescue: true}) != nil {
		t.Fatal("cartes de l'API pour une session de secours")
	}
}

func withToken(id Identity, token string) Identity {
	id.Token = token
	return id
}

// Validée à temps auprès de l'API, relue après l'expiration locale : l'issue
// est inscrite ; une carte expirée auprès de l'API est close ici aussi.
func TestAPICardsAfterLocalExpiry(t *testing.T) {
	rationale := "Le client attend une réponse depuis deux jours ; la note trace le diagnostic pour l'équipe."
	model := llmtest.New("claude",
		llmtest.Call("c1", "propose_api_add_ticket_note", `{"ticketID":"4f2","body":"vu","rationale":"`+rationale+`"}`),
		llmtest.Call("c2", "propose_api_add_ticket_note", `{"ticketID":"4f2","body":"autre","rationale":"`+rationale+`"}`),
		llmtest.Say("Deux notes proposées."))
	fake := &fakeCardsAPI{status: map[string]string{}}
	api := fake.server(t)
	iv, m := setup(model)
	cards := &memCards{now: time.Now()}
	s := &Server{Store: m, Auth: fakeAuth{"a": withToken(admin, "tok")}, Investigator: iv, Cards: cards,
		API: &APITools{BaseURL: api.URL + "/kmanager/v1"}, Model: "m", Origins: []string{"http://localhost:3001"}, Logger: quietLogger()}
	srv := httptestServer(t, s)
	m.inv = store.Investigation{ID: uuid.New(), SubjectKind: store.SubjectTicket, Reason: "ticket 4f2",
		ActorUserID: admin.UserID.String(), ActorEmail: admin.Email, Status: store.StatusOpen}
	path := "/v1/investigations/" + m.inv.ID.String()
	if r, b := call(t, srv, "POST", path+"/messages", "a", `{"message":"Note le diagnostic"}`); r.StatusCode != 200 || len(cards.cards) != 2 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	apiOf := func(c store.Card) string {
		var ref apiCardRef
		_ = json.Unmarshal(c.Params, &ref)
		return ref.Card.String()
	}
	fake.mu.Lock()
	fake.status[apiOf(cards.cards[0])] = "executed"
	fake.status[apiOf(cards.cards[1])] = "expired"
	fake.mu.Unlock()
	cards.mu.Lock()
	cards.now = cards.now.Add(store.CardTTL + time.Minute) // expirée ici
	cards.mu.Unlock()

	if r, b := call(t, srv, "GET", path, "a", ""); r.StatusCode != 200 || !strings.Contains(b, "auprès de l'API : exécutée") {
		t.Fatalf("dossier : %s", b)
	}
	if st := cards.cards[0].Status; st != store.CardExecuted {
		t.Fatalf("première carte : %s", st)
	}
	if st := cards.cards[1].Status; st != store.CardExpired {
		t.Fatalf("deuxième carte (expirée auprès de l'API) : %s", st)
	}
}
