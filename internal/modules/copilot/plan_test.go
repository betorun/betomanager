package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// memPlans : PlanStore en mémoire (copie JSON : la goroutine du plan et le
// test ne partagent rien).
type memPlans struct {
	mu     sync.Mutex
	items  map[string][]byte
	paused map[string]bool
}

func newMemPlans() *memPlans { return &memPlans{items: map[string][]byte{}, paused: map[string]bool{}} }

func (m *memPlans) Save(_ context.Context, rec *service.PlanRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[rec.State.Token] = raw
	return nil
}

func (m *memPlans) Load(_ context.Context, token, user string) (*service.PlanRecord, error) {
	m.mu.Lock()
	raw, ok := m.items[token]
	m.mu.Unlock()
	var rec service.PlanRecord
	if !ok || json.Unmarshal(raw, &rec) != nil || rec.UserID != user {
		return nil, service.ErrPlanNotFound
	}
	return &rec, nil
}

func (m *memPlans) RequestPause(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused[token] = true
	return nil
}

func (m *memPlans) PauseRequested(_ context.Context, token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused[token]
}

// seqMinter : un jeton différent à chaque émission.
type seqMinter struct {
	mu sync.Mutex
	n  int
}

func (m *seqMinter) MintDelegated(d sts.Delegation) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return fmt.Sprintf("tok-%s-%d", d.Scope, m.n), time.Now().Add(d.TTL), nil
}

// launchAPI : faux moteur HTTP de l'API pour le parcours « mettre en ligne ».
type launchAPI struct {
	mu   sync.Mutex
	reqs []string
	// buildStatus : statut final du build ; gate : la création de la base
	// attend ce canal (test de la pause).
	buildStatus string
	gate        chan struct{}
	dbPolls     int
}

func (a *launchAPI) writes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.reqs {
		if !strings.HasPrefix(r, "GET") {
			out = append(out, r)
		}
	}
	return out
}

func (a *launchAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.reqs = append(a.reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Idempotency-Key"))
	a.mu.Unlock()
	p := r.URL.Path
	reply := func(code int, body string) {
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
	switch {
	case r.Method == "GET" && p == "/projects/p1/github/installations":
		reply(200, `[{"installation_id":7,"account_login":"acme","account_type":"Organization","suspended":false}]`)
	case r.Method == "GET" && p == "/projects/p1/github/installations/7/repositories":
		reply(200, `{"repositories":[{"id":42,"full_name":"acme/shop","private":true,"default_branch":"main"}],"has_more":false}`)
	case r.Method == "GET" && p == "/projects/p1/github/installations/7/repositories/42/branches":
		reply(200, `{"branches":["main","dev"],"default":"main"}`)
	case r.Method == "POST" && p == "/projects/p1/databases":
		if a.gate != nil {
			<-a.gate
		}
		reply(201, `{"id":"db1","name":"shop-db","status":"PROVISIONING"}`)
	case r.Method == "GET" && p == "/projects/p1/databases/db1":
		a.mu.Lock()
		a.dbPolls++
		n := a.dbPolls
		a.mu.Unlock()
		if n < 2 {
			reply(200, `{"id":"db1","status":"PROVISIONING"}`)
			return
		}
		reply(200, `{"id":"db1","status":"READY"}`)
	case r.Method == "POST" && p == "/projects/p1/addons":
		reply(201, `{"id":"ad1","name":"cache","status":"STARTING"}`)
	case r.Method == "GET" && p == "/projects/p1/addons/ad1":
		reply(200, `{"id":"ad1","status":"RUNNING"}`)
	case r.Method == "POST" && p == "/projects/p1/github/workloads":
		reply(202, `{"workload_id":"w1","status":"BUILDING","build":{"id":"b1","status":"QUEUED"}}`)
	case r.Method == "GET" && p == "/projects/p1/builds/b1":
		if a.buildStatus == "FAILED" {
			reply(200, `{"id":"b1","status":"FAILED","failure":"NO_BUILD_PLAN"}`)
			return
		}
		reply(200, `{"id":"b1","status":"SUCCEEDED"}`)
	case r.Method == "POST" && (p == "/projects/p1/databases/db1/attach" || p == "/projects/p1/addons/ad1/attach"):
		reply(200, `{"workload_id":"w1","variable":"DATABASE_URL","redeploy":true}`)
	case r.Method == "POST" && p == "/workloads/w1/domains":
		reply(201, `{"id":"dom1","hostname":"shop.example.com","records":[{"type":"TXT","name":"_betorun-challenge.shop.example.com","host":"_betorun-challenge.shop","value":"proof"},{"type":"CNAME","name":"shop.example.com","host":"shop","value":"edge.betorun.test"}]}`)
	case r.Method == "GET" && p == "/workloads/w1":
		reply(200, `{"ID":"w1","Name":"shop","Status":"RUNNING"}`)
	default:
		reply(404, `{"code":"NOT_FOUND","message":"not found"}`)
	}
}

func pathParam(name string) string {
	return `{"name":"` + name + `","in":"path","required":true}`
}

func body(props ...string) string {
	var ps []string
	for _, p := range props {
		ps = append(ps, `"`+p+`":{"type":"string"}`)
	}
	return `,"requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{` + strings.Join(ps, ",") + `}}}}}`
}

var launchSpec = `{"paths":{
 "/w/{id}/status":{"get":{"summary":"S","parameters":[` + pathParam("id") + `]}},
 "/w/{id}/events":{"get":{"summary":"E","parameters":[` + pathParam("id") + `]}},
 "/w/{id}/logs":{"get":{"summary":"L","parameters":[` + pathParam("id") + `]}},
 "/workloads/{id}":{"get":{"summary":"Get","parameters":[` + pathParam("id") + `]}},
 "/workloads/{id}/domains":{"post":{"summary":"Domain","parameters":[` + pathParam("id") + `]` + body("hostname", "path") + `}},
 "/projects/{id}/github/installations":{"get":{"summary":"Accounts","parameters":[` + pathParam("id") + `]}},
 "/projects/{id}/github/installations/{installationID}/repositories":{"get":{"summary":"Repos","parameters":[` + pathParam("id") + `,` + pathParam("installationID") +
	`,{"name":"q","in":"query"},{"name":"page","in":"query","schema":{"type":"integer"}},{"name":"per_page","in":"query","schema":{"type":"integer"}}]}},
 "/projects/{id}/github/installations/{installationID}/repositories/{repoID}/branches":{"get":{"summary":"Branches","parameters":[` + pathParam("id") + `,` + pathParam("installationID") + `,` + pathParam("repoID") + `]}},
 "/projects/{id}/github/workloads":{"post":{"summary":"Create","parameters":[` + pathParam("id") + `]` +
	body("name", "installation_id", "repo_id", "branch", "context_dir", "dockerfile", "target_port", "kind") + `}},
 "/projects/{id}/builds/{buildID}":{"get":{"summary":"Build","parameters":[` + pathParam("id") + `,` + pathParam("buildID") + `]}},
 "/projects/{id}/databases":{"post":{"summary":"DB","parameters":[` + pathParam("id") + `]` + body("name", "storage_size") + `}},
 "/projects/{id}/databases/{databaseID}":{"get":{"summary":"GetDB","parameters":[` + pathParam("id") + `,` + pathParam("databaseID") + `]}},
 "/projects/{id}/databases/{databaseID}/attach":{"post":{"summary":"AttachDB","parameters":[` + pathParam("id") + `,` + pathParam("databaseID") + `]` + body("workload_id", "variable") + `}},
 "/projects/{id}/addons":{"post":{"summary":"Addon","parameters":[` + pathParam("id") + `]` + body("template", "name") + `}},
 "/projects/{id}/addons/{addonID}":{"get":{"summary":"GetAddon","parameters":[` + pathParam("id") + `,` + pathParam("addonID") + `]}},
 "/projects/{id}/addons/{addonID}/attach":{"post":{"summary":"AttachAddon","parameters":[` + pathParam("id") + `,` + pathParam("addonID") + `]` + body("workload_id", "variable") + `}}
}}`

func launchService(t *testing.T, a *launchAPI) (*service.Service, *seqMinter, *memPlans) {
	t.Helper()
	m, plans := &seqMinter{}, newMemPlans()
	s := &service.Service{LLM: llmtest.New("p"), STS: m, Proposals: newMem(), Plans: plans,
		PlanTimings: service.PlanTimings{Poll: time.Millisecond}, Logger: zap.NewNop().Sugar()}
	w := func(name, method, path string, risk tools.Risk) tools.Route {
		return tools.Route{Name: name, Method: method, Path: path, Write: true, Risk: risk}
	}
	err := s.Bind(a, []tools.Route{
		{Name: tools.ToolGetWorkload, Method: "GET", Path: "/workloads/:id"},
		{Name: tools.ToolGetWorkloadStatus, Method: "GET", Path: "/w/:id/status"},
		{Name: tools.ToolGetWorkloadEvents, Method: "GET", Path: "/w/:id/events"},
		{Name: tools.ToolGetWorkloadLogs, Method: "GET", Path: "/w/:id/logs"},
		{Name: tools.ToolListGitHubAccounts, Method: "GET", Path: "/projects/:id/github/installations"},
		{Name: tools.ToolListGitHubRepositories, Method: "GET", Path: "/projects/:id/github/installations/:installationID/repositories"},
		{Name: tools.ToolListGitHubBranches, Method: "GET", Path: "/projects/:id/github/installations/:installationID/repositories/:repoID/branches"},
		{Name: tools.ToolGetBuild, Method: "GET", Path: "/projects/:id/builds/:buildID"},
		{Name: tools.ToolGetDatabase, Method: "GET", Path: "/projects/:id/databases/:databaseID"},
		{Name: tools.ToolGetAddon, Method: "GET", Path: "/projects/:id/addons/:addonID"},
		w(tools.ToolCreateWorkloadGitHub, "POST", "/projects/:id/github/workloads", tools.RiskLow),
		w(tools.ToolCreateDatabase, "POST", "/projects/:id/databases", tools.RiskLow),
		w(tools.ToolAttachDatabase, "POST", "/projects/:id/databases/:databaseID/attach", tools.RiskMedium),
		w(tools.ToolCreateAddon, "POST", "/projects/:id/addons", tools.RiskLow),
		w(tools.ToolAttachAddon, "POST", "/projects/:id/addons/:addonID/attach", tools.RiskMedium),
		w(tools.ToolAddWorkloadDomain, "POST", "/workloads/:id/domains", tools.RiskLow),
	}, []byte(launchSpec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Catalog.Lookup(tools.ToolProposeLaunchPlan); !ok {
		t.Fatal("propose_launch_plan absent du catalogue")
	}
	return s, m, plans
}

const fullPlan = `{"project_id":"p1","name":"shop","github":{"installation_id":7,"repository":"acme/shop","branch":"main"},
 "target_port":3000,"database":{"name":"shop-db"},"addon":{"template":"redis","name":"cache"},"domain":{"hostname":"shop.example.com"}}`

func proposePlan(t *testing.T, s *service.Service, user uuid.UUID, args string) service.View {
	t.Helper()
	var views []service.View
	res := runner(s, user.String(), &views).Run(context.Background(), tools.ToolProposeLaunchPlan, json.RawMessage(args))
	if res.IsError || len(views) != 1 {
		t.Fatalf("proposition : %s (%d cartes)", res.Content, len(views))
	}
	return views[0]
}

func planCall(t *testing.T, s *service.Service, method, path, token string, user uuid.UUID) (*httptest.ResponseRecorder, service.PlanState) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Params = gin.Params{{Key: "token", Value: token}}
	c.Set("authMethod", "bearer")
	c.Set("internalUserID", user.String())
	c.Set("sessionID", uuid.NewString())
	h := NewController(s)
	if strings.HasSuffix(path, "/pause") {
		h.PausePlan(c)
	} else {
		h.GetPlan(c)
	}
	var st service.PlanState
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	return w, st
}

// waitPlan : relit le plan jusqu'à la fin de son exécution.
func waitPlan(t *testing.T, s *service.Service, token string, user uuid.UUID) service.PlanState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, st := planCall(t, s, http.MethodGet, "/copilot/plans/"+token, token, user)
		if st.Status != service.PlanRunning {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("le plan ne se termine pas")
	return service.PlanState{}
}

func stepStatuses(st service.PlanState) string {
	var out []string
	for _, s := range st.Steps {
		out = append(out, s.Key+"="+s.Status)
	}
	return strings.Join(out, " ")
}

func TestLaunchPlanRefusedForUnknownRepository(t *testing.T) {
	a := &launchAPI{}
	s, _, _ := launchService(t, a)
	for name, args := range map[string]string{
		"dépôt introuvable":   `{"project_id":"p1","name":"shop","github":{"installation_id":7,"repository":"acme/nope","branch":"main"}}`,
		"identifiant inconnu": `{"project_id":"p1","name":"shop","github":{"installation_id":7,"repository":99,"branch":"main"}}`,
		"compte non relié":    `{"project_id":"p1","name":"shop","github":{"installation_id":8,"repository":"acme/shop","branch":"main"}}`,
		"branche inconnue":    `{"project_id":"p1","name":"shop","github":{"installation_id":7,"repository":"acme/shop","branch":"nope"}}`,
		"champ inconnu":       `{"project_id":"p1","name":"shop","github":{"installation_id":7,"repository":"acme/shop","branch":"main"},"image":"x"}`,
		"champ inconnu dans":  `{"project_id":"p1","name":"shop","github":{"installation_id":7,"repository":"acme/shop","branch":"main","token":"x"}}`,
		"worker et domaine":   `{"project_id":"p1","name":"shop","kind":"worker","github":{"installation_id":7,"repository":"acme/shop","branch":"main"},"domain":{"hostname":"a.example.com"}}`,
	} {
		var views []service.View
		res := runner(s, uuid.NewString(), &views).Run(context.Background(), tools.ToolProposeLaunchPlan, json.RawMessage(args))
		if !res.IsError || len(views) != 0 {
			t.Fatalf("%s : carte créée (%s)", name, res.Content)
		}
	}
	if w := a.writes(); len(w) != 0 {
		t.Fatalf("écriture jouée à la proposition : %v", w)
	}
}

func TestLaunchPlanFullRun(t *testing.T) {
	a := &launchAPI{}
	s, m, _ := launchService(t, a)
	user, session := uuid.New(), uuid.New()
	v := proposePlan(t, s, user, fullPlan)
	if v.Tool != tools.ToolProposeLaunchPlan || v.Risk != tools.RiskMedium || v.Target != "shop" || v.Plan == nil {
		t.Fatalf("carte : %+v", v)
	}
	if v.Plan.Repository != "acme/shop" || v.Plan.Branch != "main" || v.Plan.Title != "shop" {
		t.Fatalf("plan : %+v", v.Plan)
	}
	var kinds []string
	for _, st := range v.Plan.Steps {
		kinds = append(kinds, string(st.Kind))
	}
	want := "create_database wait_database create_addon wait_addon create_workload wait_build attach_database attach_addon add_domain wait_running"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("étapes : %v", kinds)
	}
	if v.Plan.Steps[4].Params["port"] != "3000" || v.Plan.Steps[4].Params["repository"] != "acme/shop" {
		t.Fatalf("paramètres : %+v", v.Plan.Steps[4].Params)
	}
	// Une deuxième carte dans le même message est refusée (même compteur).
	var views []service.View
	r := runner(s, user.String(), &views)
	r.Run(context.Background(), tools.ToolProposeLaunchPlan, json.RawMessage(fullPlan))
	if res := r.Run(context.Background(), "create_database", json.RawMessage(`{"id":"p1","name":"x-db"}`)); !res.IsError {
		t.Fatal("deuxième carte acceptée après un plan")
	}

	w, out := call(t, s, "approve", v.Token, user, session)
	if w.Code != 200 || out.Status != "running" || out.Plan == nil || out.Plan.Status != "running" || len(out.Plan.Steps) != 10 {
		t.Fatalf("validation : %d %s", w.Code, w.Body)
	}
	st := waitPlan(t, s, v.Token, user)
	if st.Status != service.PlanDone || st.EndedAt == nil {
		t.Fatalf("plan : %s %s", st.Status, stepStatuses(st))
	}
	for _, step := range st.Steps {
		if step.Status != service.StepDone || step.StartedAt == nil || step.EndedAt == nil {
			t.Fatalf("étape %s : %+v", step.Key, step)
		}
	}
	if r := st.Resources; r.WorkloadID != "w1" || r.BuildID != "b1" || r.DatabaseID != "db1" || r.AddonID != "ad1" {
		t.Fatalf("ressources : %+v", r)
	}
	if dns := st.Steps[8].DNS; dns == nil || dns.Type != "TXT" || dns.Name != "_betorun-challenge.shop" || dns.Value != "proof" {
		t.Fatalf("enregistrement DNS : %+v", st.Steps[8].DNS)
	}
	// Jeton délégué « write » NEUF par étape d'écriture, clé d'idempotence
	// = jeton du plan + clé d'étape.
	writes := a.writes()
	if len(writes) != 6 {
		t.Fatalf("écritures : %v", writes)
	}
	seen := map[string]bool{}
	for _, req := range writes {
		f := strings.Fields(req)
		if len(f) != 5 || !strings.HasPrefix(f[3], "tok-write-") || !strings.HasPrefix(f[4], v.Token+":") {
			t.Fatalf("écriture : %q", req)
		}
		if seen[f[3]] {
			t.Fatalf("jeton d'écriture réutilisé : %q", req)
		}
		seen[f[3]] = true
	}
	if !strings.HasSuffix(writes[0], v.Token+":create_database") || !strings.HasSuffix(writes[5], v.Token+":add_domain") {
		t.Fatalf("ordre ou clés : %v", writes)
	}
	// Les attentes relisent avec des jetons « read ».
	for _, req := range a.reqs {
		if strings.HasPrefix(req, "GET /projects/p1/builds/b1") && !strings.Contains(req, "tok-read-") {
			t.Fatalf("relecture sans jeton read : %q", req)
		}
	}
	if m.n == 0 {
		t.Fatal("aucun jeton émis")
	}
	// La carte est consommée.
	if w, _ := call(t, s, "approve", v.Token, user, session); w.Code == 200 {
		t.Fatal("plan validé deux fois")
	}
}

func TestLaunchPlanBuildFailureSkipsTheRest(t *testing.T) {
	a := &launchAPI{buildStatus: "FAILED"}
	s, _, _ := launchService(t, a)
	user, session := uuid.New(), uuid.New()
	v := proposePlan(t, s, user, fullPlan)
	if w, _ := call(t, s, "approve", v.Token, user, session); w.Code != 200 {
		t.Fatalf("validation : %d %s", w.Code, w.Body)
	}
	st := waitPlan(t, s, v.Token, user)
	want := "create_database=done wait_database=done create_addon=done wait_addon=done create_workload=done wait_build=failed " +
		"attach_database=skipped attach_addon=skipped add_domain=skipped wait_running=skipped"
	if st.Status != service.PlanFailed || stepStatuses(st) != want {
		t.Fatalf("plan : %s %s", st.Status, stepStatuses(st))
	}
	if !strings.Contains(st.Steps[5].Message, "NO_BUILD_PLAN") {
		t.Fatalf("message : %q", st.Steps[5].Message)
	}
	for _, req := range a.writes() {
		if strings.Contains(req, "attach") || strings.Contains(req, "domains") {
			t.Fatalf("étape jouée après l'échec : %s", req)
		}
	}
}

func TestLaunchPlanPause(t *testing.T) {
	a := &launchAPI{gate: make(chan struct{})}
	s, _, _ := launchService(t, a)
	user, session := uuid.New(), uuid.New()
	v := proposePlan(t, s, user, fullPlan)
	if w, _ := call(t, s, "approve", v.Token, user, session); w.Code != 200 {
		t.Fatalf("validation : %d %s", w.Code, w.Body)
	}
	// La création de la base est en cours (bloquée par gate).
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, st := planCall(t, s, http.MethodGet, "/copilot/plans/"+v.Token, v.Token, user); st.Steps[0].Status == service.StepRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("la première étape ne démarre pas")
		}
	}
	// Autre personne : introuvable, y compris pour l'arrêter.
	if w, _ := planCall(t, s, http.MethodPost, "/copilot/plans/"+v.Token+"/pause", v.Token, uuid.New()); w.Code != http.StatusNotFound {
		t.Fatalf("arrêt par autrui : %d", w.Code)
	}
	w, st := planCall(t, s, http.MethodPost, "/copilot/plans/"+v.Token+"/pause", v.Token, user)
	if w.Code != http.StatusAccepted || st.Token != v.Token {
		t.Fatalf("arrêt : %d %s", w.Code, w.Body)
	}
	close(a.gate) // l'étape en cours (création de la base) se termine
	st = waitPlan(t, s, v.Token, user)
	if st.Status != service.PlanPaused || st.Steps[0].Status != service.StepDone {
		t.Fatalf("plan : %s %s", st.Status, stepStatuses(st))
	}
	for _, step := range st.Steps[1:] {
		if step.Status != service.StepSkipped {
			t.Fatalf("étape après l'arrêt : %s", stepStatuses(st))
		}
	}
	if len(a.writes()) != 1 {
		t.Fatalf("écritures après l'arrêt : %v", a.writes())
	}
}

func TestLaunchPlanInterruptedAndOwnership(t *testing.T) {
	a := &launchAPI{}
	s, _, plans := launchService(t, a)
	user := uuid.New()
	token := strings.Repeat("ab", 32)
	now := time.Now().UTC()
	_ = plans.Save(context.Background(), &service.PlanRecord{UserID: user.String(), HeartbeatAt: now.Add(-5 * time.Minute),
		State: service.PlanState{Token: token, Status: service.PlanRunning, Title: "shop", StartedAt: now.Add(-6 * time.Minute),
			Steps: []service.StepState{{Key: "create_workload", Kind: service.StepCreateWorkload, Status: service.StepDone},
				{Key: "wait_build", Kind: service.StepWaitBuild, Status: service.StepRunning},
				{Key: "wait_running", Kind: service.StepWaitRunning, Status: service.StepPending}}}})
	w, st := planCall(t, s, http.MethodGet, "/copilot/plans/"+token, token, user)
	if w.Code != 200 || st.Status != service.PlanInterrupted || st.EndedAt == nil ||
		stepStatuses(st) != "create_workload=done wait_build=skipped wait_running=skipped" {
		t.Fatalf("plan interrompu : %d %s", w.Code, w.Body)
	}
	// Battement récent : toujours en cours.
	_ = plans.Save(context.Background(), &service.PlanRecord{UserID: user.String(), HeartbeatAt: now,
		State: service.PlanState{Token: token, Status: service.PlanRunning, Title: "shop", StartedAt: now}})
	if _, st := planCall(t, s, http.MethodGet, "/copilot/plans/"+token, token, user); st.Status != service.PlanRunning {
		t.Fatalf("plan vivant vu comme %s", st.Status)
	}
	// Autre personne : introuvable.
	if w, _ := planCall(t, s, http.MethodGet, "/copilot/plans/"+token, token, uuid.New()); w.Code != http.StatusNotFound {
		t.Fatalf("plan d'autrui : %d", w.Code)
	}
}
