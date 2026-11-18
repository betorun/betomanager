package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/archive"
	"github.com/thekrauss/kubemanager/internal/cli/mcp"
	buildsdomain "github.com/thekrauss/kubemanager/internal/modules/builds/domain"
)

// Outils MCP : ce qu'un agent de code (Claude Code, Cursor, Windsurf…) peut
// faire sur betorun, avec les identifiants de la personne (betorun login, ou
// BETORUN_TOKEN / BETORUN_KEY_FILE). Les droits sont ceux de l'API : rien de
// plus que la CLI. Les actions sensibles (suppression, accès, clés) ne sont
// pas des outils : elles restent à la personne.

const mcpInstructions = `betorun hosts containers built from code, and managed Postgres databases. Typical flow: call deploy with the project directory ` +
	`(no Dockerfile needed for Next.js, Vite/React/Vue/Astro, Node, Django, FastAPI, Flask, Streamlit or static sites); ` +
	`it builds, deploys and returns the public URL. If the build fails, the result contains the decisive log lines: fix the ` +
	`code and call deploy again. If the app needs a database, call create_database with attach_to set to the container: it gets DATABASE_URL. ` +
	`To investigate a running container, call diagnose_workload. Never put secrets in env: ` +
	`tell the person to set them with "betorun workloads secrets" or in the console.`

// mcpEnv : ce dont les outils ont besoin (remplaçable dans les tests).
type mcpEnv struct {
	settings func() (settingsLike, error)
	client   func(apiURL string) *api.Client
	now      func() time.Time
	// Délais : un déploiement complet tient en général en quelques minutes.
	buildTimeout, workloadTimeout, poll time.Duration
}

type settingsLike struct {
	APIURL    string
	ProjectID string
}

func defaultMCPEnv() mcpEnv {
	return mcpEnv{
		settings: func() (settingsLike, error) {
			s, err := settings()
			return settingsLike{APIURL: s.APIURL, ProjectID: s.ProjectID}, err
		},
		client: func(apiURL string) *api.Client {
			c := api.New(apiURL)
			src, err := credentialSource(apiURL)
			if err != nil {
				c.Source = failingSource{err}
			} else {
				c.Source = src
			}
			// Pas de double authentification interactive : stdin est le protocole.
			return c
		},
		now:             time.Now,
		buildTimeout:    15 * time.Minute,
		workloadTimeout: 5 * time.Minute,
		poll:            3 * time.Second,
	}
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if len(required) == 0 {
		req = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `,"additionalProperties":false}`)
}

const projectProp = `"project":{"type":"string","description":"Project ID (optional: the configured project, or the only one you have)"}`

func readOnly(title string) mcp.Annotations {
	return mcp.Annotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true}
}

func (e mcpEnv) tools() []mcp.Tool {
	tools := e.toolList()
	// Toute erreur d'identifiants, d'où qu'elle vienne, dit comment se connecter.
	for i := range tools {
		h := tools[i].Handler
		tools[i].Handler = func(ctx context.Context, c mcp.Call) (mcp.Result, error) {
			res, err := h(ctx, c)
			if err != nil {
				return res, signInHint(err)
			}
			return res, nil
		}
	}
	return tools
}

func (e mcpEnv) toolList() []mcp.Tool {
	return []mcp.Tool{
		{Name: "whoami", Description: "Who you are signed in as on betorun, and the default project.",
			InputSchema: schema(""), Annotations: readOnly("Who am I"), Handler: e.whoami},
		{Name: "list_projects", Description: "Projects you can access.",
			InputSchema: schema(""), Annotations: readOnly("List projects"), Handler: e.listProjects},
		{Name: "list_workloads", Description: "Containers of a project, with their state and public URL.",
			InputSchema: schema(projectProp), Annotations: readOnly("List containers"), Handler: e.listWorkloads},
		{Name: "get_logs", Description: "Last lines of a container's logs (current instance, or the previous one after a restart).",
			InputSchema: schema(`"workload":{"type":"string","description":"Container ID or name"},`+projectProp+
				`,"tail":{"type":"integer","maximum":500,"description":"Number of lines (default 200)"},"previous":{"type":"boolean","description":"Logs of the previous instance"}`, "workload"),
			Annotations: readOnly("Container logs"), Handler: e.getLogs},
		{Name: "diagnose_workload", Description: "Why a container is down, crashing or unreachable: its state, recent events and the end of its logs, in one call.",
			InputSchema: schema(`"workload":{"type":"string","description":"Container ID or name"},`+projectProp, "workload"),
			Annotations: readOnly("Diagnose a container"), Handler: e.diagnose},
		{Name: "list_builds", Description: "Recent builds of a project.",
			InputSchema: schema(projectProp), Annotations: readOnly("List builds"), Handler: e.listBuilds},
		{Name: "get_build_logs", Description: "Log of a build (the end of it for a long log).",
			InputSchema: schema(`"build":{"type":"string","description":"Build ID"},`+projectProp, "build"),
			Annotations: readOnly("Build log"), Handler: e.buildLogs},
		{Name: "deploy", Title: "Deploy to betorun",
			Description: "Build a local project directory and put it online: packs the code (respecting .dockerignore; .env files and keys are excluded by default), " +
				"builds it on betorun (no Dockerfile needed for common frameworks), creates or updates the container of that name, waits until it runs " +
				"and returns its public URL. On a build failure, returns the decisive log lines to fix the code. " +
				"kind=worker runs a long-lived process without a port or URL (queue consumer, bot); kind=cron runs the command on a schedule " +
				"(one run at a time). The kind of an existing container cannot change.",
			InputSchema: schema(`"path":{"type":"string","description":"Project directory (absolute, or relative to the current directory)"},`+
				`"name":{"type":"string","description":"Container name, 3-30 lowercase characters (default: the directory name)"},`+projectProp+
				`,"port":{"type":"integer","description":"Port the app listens on (default 8080, which betorun builds use)"}`+
				`,"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Plain environment variables (never secrets)"}`+
				`,"kind":{"type":"string","enum":["web","worker","cron"],"description":"web (default, HTTP service with a URL), worker (long-running, no port) or cron (scheduled task)"}`+
				`,"command":{"type":"array","items":{"type":"string"},"description":"Start command replacing the image's, e.g. [\"node\",\"queue.js\"] (optional)"}`+
				`,"schedule":{"type":"string","description":"cron only: 5 fields (\"0 2 * * *\") or @hourly, @daily, @weekly, @monthly"}`+
				`,"time_zone":{"type":"string","description":"cron only: IANA time zone of the schedule, e.g. Africa/Dakar (default UTC)"}`, "path"),
			Annotations: mcp.Annotations{Title: "Deploy", OpenWorldHint: true}, Handler: e.deploy},
		{Name: "list_databases", Description: "Managed Postgres databases of a project, with their state.",
			InputSchema: schema(projectProp), Annotations: readOnly("List databases"), Handler: e.listDatabases},
		{Name: "create_database", Title: "Create a Postgres database",
			Description: "Create a managed Postgres database in the project (reachable only from its containers), wait until it is ready, " +
				"and optionally attach it to a container: its connection URI becomes the SECRET variable DATABASE_URL and the container is redeployed. " +
				"The password never appears in the result.",
			InputSchema: schema(`"name":{"type":"string","description":"Database name, 3-30 lowercase letters, digits, hyphens"},`+projectProp+
				`,"storage_size":{"type":"string","enum":["1Gi","2Gi","5Gi","10Gi","20Gi"],"description":"Disk size (default 1Gi)"}`+
				`,"attach_to":{"type":"string","description":"Container ID or name to attach it to (optional)"}`, "name"),
			Annotations: mcp.Annotations{Title: "Create a database", OpenWorldHint: true}, Handler: e.createDatabase},
		{Name: "attach_database", Description: "Give a container access to a database: sets its connection URI as a SECRET variable (DATABASE_URL by default) and redeploys the container.",
			InputSchema: schema(`"database":{"type":"string","description":"Database ID or name"},"workload":{"type":"string","description":"Container ID or name"},`+projectProp+
				`,"variable":{"type":"string","description":"Variable name (default DATABASE_URL)"}`, "database", "workload"),
			Annotations: mcp.Annotations{Title: "Attach a database", IdempotentHint: true, OpenWorldHint: true}, Handler: e.attachDatabase},
		{Name: "restore_database", Title: "Restore a database",
			Description: "Restore a database into a NEW database from its backups (latest state, or target_time). The source is never changed; " +
				"waits until the copy is ready. Then attach_database switches a container to it.",
			InputSchema: schema(`"database":{"type":"string","description":"Source database ID or name"},"name":{"type":"string","description":"Name of the new database"},`+projectProp+
				`,"target_time":{"type":"string","description":"Instant to restore, RFC 3339 (default: latest)"}`, "database", "name"),
			Annotations: mcp.Annotations{Title: "Restore a database", OpenWorldHint: true}, Handler: e.restoreDatabase},
		{Name: "restart_workload", Description: "Restart a container (rolling replacement, no downtime with several replicas). For a cron container: run it now, outside its schedule.",
			InputSchema: schema(`"workload":{"type":"string","description":"Container ID or name"},`+projectProp, "workload"),
			Annotations: mcp.Annotations{Title: "Restart a container", IdempotentHint: true, OpenWorldHint: true}, Handler: e.restart},
	}
}

// ── Contexte commun ────────────────────────────────────────────────────────

type mcpCtx struct {
	cl      *api.Client
	project string
}

func (e mcpEnv) session(ctx context.Context, args json.RawMessage, needProject bool) (*mcpCtx, map[string]any, error) {
	in := map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 {
		if err := json.Unmarshal(args, &in); err != nil || in == nil {
			return nil, nil, errors.New("arguments must be a JSON object")
		}
	}
	s, err := e.settings()
	if err != nil {
		return nil, nil, err
	}
	m := &mcpCtx{cl: e.client(s.APIURL), project: str(in["project"])}
	if m.project == "" {
		m.project = s.ProjectID
	}
	if needProject && m.project == "" {
		projects, err := listProjects(ctx, m.cl)
		if err != nil {
			return nil, nil, signInHint(err)
		}
		switch len(projects) {
		case 0:
			return nil, nil, errors.New("you have no project yet: create one in the betorun console")
		case 1:
			m.project = projects[0].ProjectID
		default:
			names := make([]string, 0, len(projects))
			for _, p := range projects {
				names = append(names, p.Name+" ("+p.ProjectID+")")
			}
			return nil, nil, errors.New("several projects: pass \"project\" (one of: " + strings.Join(names, ", ") + ")")
		}
	}
	return m, in, nil
}

var errNotSignedIn = errors.New(`not signed in to betorun: ask the person to run "betorun login" in a terminal, then retry`)

// signInHint : sans identifiants, dire comment se connecter.
func signInHint(err error) error {
	var ae *api.Error
	if errors.Is(err, api.ErrNotLoggedIn) || (errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) {
		return errNotSignedIn
	}
	return err
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// strList : tableau de chaînes ; nil si absent.
func strList(v any, field string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", field)
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", field)
		}
		out = append(out, s)
	}
	return out, nil
}

func num(v any, def, max int) int {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return def
	}
	if int(f) > max {
		return max
	}
	return int(f)
}

func jsonText(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}

type projectSummary struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
}

func listProjects(ctx context.Context, cl *api.Client) ([]projectSummary, error) {
	var out struct {
		Projects []projectSummary `json:"projects"`
	}
	err := cl.Do(ctx, http.MethodGet, "/projects", nil, &out)
	return out.Projects, err
}

// workload : par identifiant ou par nom dans le projet.
func (m *mcpCtx) workload(ctx context.Context, ref string) (*api.Workload, error) {
	if ref == "" {
		return nil, errors.New(`"workload" is required`)
	}
	if looksLikeUUID(ref) {
		return m.cl.GetWorkload(ctx, ref)
	}
	if m.project == "" {
		return nil, errors.New(`pass "project" to find a container by name`)
	}
	list, err := m.cl.ListWorkloads(ctx, m.project)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("no container named %q in this project", ref)
}

func looksLikeUUID(s string) bool {
	return len(s) == 36 && strings.Count(s, "-") == 4
}

// ── Lectures ───────────────────────────────────────────────────────────────

func (e mcpEnv) whoami(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, _, err := e.session(ctx, call.Arguments, false)
	if err != nil {
		return mcp.Result{}, err
	}
	sess, err := m.cl.Session(ctx)
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	out := map[string]any{"session": sess, "default_project": m.project}
	return mcp.Result{Text: jsonText(out), Structured: out}, nil
}

func (e mcpEnv) listProjects(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, _, err := e.session(ctx, call.Arguments, false)
	if err != nil {
		return mcp.Result{}, err
	}
	p, err := listProjects(ctx, m.cl)
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	out := map[string]any{"projects": p}
	return mcp.Result{Text: jsonText(out), Structured: out}, nil
}

func (e mcpEnv) listWorkloads(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, _, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	list, err := m.cl.ListWorkloads(ctx, m.project)
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	type row struct {
		ID, Name, Status, Phase, Image, URL string
	}
	rows := make([]row, 0, len(list))
	for _, w := range list {
		rows = append(rows, row{w.ID.String(), w.Name, w.Status, w.CurrentPhase, w.Image, w.ExternalURL})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	out := map[string]any{"workloads": rows}
	return mcp.Result{Text: jsonText(out), Structured: out}, nil
}

func (e mcpEnv) logs(ctx context.Context, cl *api.Client, id string, tail int, previous bool) string {
	var buf limitedBuffer
	buf.max = 64 << 10
	_ = cl.StreamLogs(ctx, id, api.LogsOptions{Tail: int64(tail), Previous: previous}, &buf)
	return buf.String()
}

func (e mcpEnv) getLogs(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, false)
	if err != nil {
		return mcp.Result{}, err
	}
	w, err := m.workload(ctx, str(in["workload"]))
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	prev, _ := in["previous"].(bool)
	text := e.logs(ctx, m.cl, w.ID.String(), num(in["tail"], 200, 500), prev)
	if text == "" {
		text = "(no log lines)"
	}
	return mcp.Result{Text: text}, nil
}

func (e mcpEnv) diagnose(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, false)
	if err != nil {
		return mcp.Result{}, err
	}
	w, err := m.workload(ctx, str(in["workload"]))
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	id := w.ID.String()
	var status, events json.RawMessage
	_ = m.cl.Do(ctx, http.MethodGet, "/workloads/"+url.PathEscape(id)+"/status", nil, &status)
	_ = m.cl.Do(ctx, http.MethodGet, "/workloads/"+url.PathEscape(id)+"/events", nil, &events)
	var b strings.Builder
	fmt.Fprintf(&b, "## container\nname: %s\nstatus: %s\nphase: %s\nimage: %s\nurl: %s\n\n", w.Name, w.Status, w.CurrentPhase, w.Image, w.ExternalURL)
	fmt.Fprintf(&b, "## live state\n%s\n\n## recent events\n%s\n\n## logs (end)\n%s\n", clipText(string(status), 4<<10), clipText(string(events), 4<<10),
		tailText(e.logs(ctx, m.cl, id, 120, false), 6<<10))
	if prev := e.logs(ctx, m.cl, id, 60, true); strings.TrimSpace(prev) != "" {
		fmt.Fprintf(&b, "\n## logs of the previous instance (end)\n%s\n", tailText(prev, 4<<10))
	}
	return mcp.Result{Text: b.String()}, nil
}

func (e mcpEnv) listBuilds(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, _, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	list, err := m.cl.ListBuilds(ctx, m.project, "")
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	if len(list) > 20 {
		list = list[:20]
	}
	out := map[string]any{"builds": list}
	return mcp.Result{Text: jsonText(out), Structured: out}, nil
}

func (e mcpEnv) buildLog(ctx context.Context, cl *api.Client, project, id string) string {
	var buf limitedBuffer
	buf.max = 256 << 10
	buf.keepTail = true
	_ = cl.StreamBuildLogs(ctx, project, id, false, &buf)
	return buf.String()
}

func (e mcpEnv) buildLogs(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	id := str(in["build"])
	if id == "" {
		return mcp.Result{}, errors.New(`"build" is required`)
	}
	return mcp.Result{Text: tailText(e.buildLog(ctx, m.cl, m.project, id), 12<<10)}, nil
}

// ── Écritures ──────────────────────────────────────────────────────────────

func (e mcpEnv) restart(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, false)
	if err != nil {
		return mcp.Result{}, err
	}
	w, err := m.workload(ctx, str(in["workload"]))
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	if err := m.cl.WorkloadAction(ctx, w.ID.String(), "restart"); err != nil {
		return mcp.Result{}, signInHint(err)
	}
	return mcp.Result{Text: fmt.Sprintf("Restart of %q requested.", w.Name)}, nil
}

// DeployResult : issue d'un déploiement.
type DeployResult struct {
	Status     string   `json:"status"` // running | build_failed | deploy_failed | still_building | still_deploying
	Name       string   `json:"name"`
	Kind       string   `json:"kind,omitempty"`
	WorkloadID string   `json:"workload_id,omitempty"`
	BuildID    string   `json:"build_id,omitempty"`
	Image      string   `json:"image,omitempty"`
	Detected   string   `json:"detected,omitempty"`
	URL        string   `json:"url,omitempty"`
	Failure    string   `json:"failure,omitempty"`
	Log        string   `json:"log,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

func (e mcpEnv) deploy(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	dir, err := deployDir(str(in["path"]))
	if err != nil {
		return mcp.Result{}, err
	}
	name := str(in["name"])
	if name == "" {
		name = defaultName(filepath.Base(dir))
		if len(name) > 30 {
			name = strings.Trim(name[:30], "-")
		}
	}
	if !validWorkloadName(name) {
		return mcp.Result{}, fmt.Errorf("invalid name %q: 3 to 30 characters, lowercase letters, digits and hyphens", name)
	}
	env := map[string]string{}
	if raw, ok := in["env"].(map[string]any); ok {
		for k, v := range raw {
			s, ok := v.(string)
			if !ok {
				return mcp.Result{}, fmt.Errorf("env %q must be a string", k)
			}
			env[k] = s
		}
	}
	port := num(in["port"], defaultTargetPort, 65535)
	kind := strings.ToLower(str(in["kind"]))
	command, err := strList(in["command"], "command")
	if err != nil {
		return mcp.Result{}, err
	}
	schedule, timeZone := strings.TrimSpace(str(in["schedule"])), strings.TrimSpace(str(in["time_zone"]))
	res := DeployResult{Name: name}

	// Sorte et horaire vérifiés avant le build : inutile de construire pour
	// refuser ensuite.
	list, err := m.cl.ListWorkloads(ctx, m.project)
	if err != nil {
		return mcp.Result{}, err
	}
	existingKind := ""
	for i := range list {
		if list[i].Name == name {
			res.WorkloadID, existingKind = list[i].ID.String(), firstNonEmpty(list[i].Kind, kindWeb)
		}
	}
	if res.WorkloadID != "" {
		if kind != "" && kind != existingKind {
			return mcp.Result{}, fmt.Errorf("container %q is a %s container: its kind cannot change (deploy under another name)", name, existingKind)
		}
		kind = existingKind
	}
	kind = firstNonEmpty(kind, kindWeb)
	if kind != kindWeb && kind != kindWorker && kind != kindCron {
		return mcp.Result{}, fmt.Errorf("invalid kind %q: web, worker or cron", kind)
	}
	if kind != kindWeb {
		port = 0 // ni port ni URL hors web
	}
	if kind == kindCron && res.WorkloadID == "" && schedule == "" {
		return mcp.Result{}, errors.New(`kind "cron" needs a schedule, e.g. "0 2 * * *" or "@daily"`)
	}
	if kind != kindCron && (schedule != "" || timeZone != "") {
		return mcp.Result{}, errors.New(`schedule and time_zone only apply to kind "cron"`)
	}
	res.Kind = kind

	// 1. Code → archive → build.
	call.Progress(0, 0, "packing "+dir)
	source, err := archive.TarGz(dir)
	if err != nil {
		return mcp.Result{}, err
	}
	defer os.Remove(source.Path)
	if len(source.Sensitive) > 0 {
		res.Warnings = append(res.Warnings, "files that look like secrets were included (allowed by .dockerignore): "+strings.Join(source.Sensitive, ", "))
	}
	f, err := os.Open(source.Path)
	if err != nil {
		return mcp.Result{}, err
	}
	defer f.Close()
	call.Progress(0, 0, fmt.Sprintf("uploading %d files (%s)", source.Files, archive.HumanSize(source.Size)))
	build, err := m.cl.UploadBuild(ctx, m.project, name, buildsdomain.DefaultDockerfile, f)
	if err != nil {
		return mcp.Result{}, signInHint(err)
	}
	res.BuildID = build.ID.String()

	// 2. Attente du build.
	deadline := e.now().Add(e.buildTimeout)
	for !build.Finished() {
		if e.now().After(deadline) {
			res.Status = "still_building"
			return deployResult(res, "The build is still running. Call list_builds or get_build_logs later.")
		}
		call.Progress(0, 0, "build "+strings.ToLower(build.Status))
		select {
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		case <-time.After(e.poll):
		}
		if build, err = m.cl.GetBuild(ctx, m.project, res.BuildID); err != nil {
			return mcp.Result{}, err
		}
	}
	res.Detected = build.Detected
	if build.Status != buildsdomain.StatusSucceeded {
		res.Status, res.Failure = "build_failed", build.Failure
		res.Log = tailText(e.buildLog(ctx, m.cl, m.project, res.BuildID), 8<<10)
		return deployResult(res, "The build failed. Read the log below, fix the code, then call deploy again.")
	}
	res.Image = build.Image

	// 3. Conteneur : mise à jour s'il existe, création sinon.
	call.Progress(0, 0, "deploying "+name)
	if res.WorkloadID != "" {
		req := api.UpdateWorkloadRequest{Image: build.Image, TargetPort: port, Schedule: schedule}
		if command != nil {
			req.Command = &command
		}
		if len(env) > 0 {
			// env_vars REMPLACE les variables simples du conteneur : on part des
			// existantes pour n'en perdre aucune (les secrets ne sont pas touchés).
			merged, err := plainVariables(ctx, m.cl, res.WorkloadID)
			if err != nil {
				return mcp.Result{}, err
			}
			for k, v := range env {
				merged[k] = v
			}
			req.EnvVars = merged
		}
		if err := m.cl.UpdateWorkload(ctx, res.WorkloadID, req); err != nil {
			return mcp.Result{}, err
		}
	} else {
		created, err := m.cl.CreateWorkload(ctx, api.CreateWorkloadRequest{ProjectID: m.project, Name: name, Image: build.Image,
			EnvVars: env, Replicas: 1, TargetPort: port, CPULimit: defaultCPULimit, MemoryLimit: defaultMemoryLimit,
			Kind: kind, Command: command, Schedule: schedule, TimeZone: timeZone})
		if err != nil {
			return mcp.Result{}, err
		}
		res.WorkloadID = created.WorkloadID
	}

	// 4. Attente de la mise en service.
	deadline = e.now().Add(e.workloadTimeout)
	for {
		w, err := m.cl.GetWorkload(ctx, res.WorkloadID)
		if err != nil {
			return mcp.Result{}, err
		}
		res.URL = w.ExternalURL
		switch {
		case workloadReady[w.Status]:
			res.Status = "running"
			switch kind {
			case kindCron:
				return deployResult(res, "Deployed: the task runs on its schedule. restart_workload runs it now.")
			case kindWorker:
				return deployResult(res, "Deployed and running (no public URL: a worker has no port).")
			}
			return deployResult(res, "Deployed and running.")
		case workloadFailed[w.Status]:
			res.Status, res.Failure = "deploy_failed", w.Status+" "+w.CurrentPhase
			res.Log = tailText(e.logs(ctx, m.cl, res.WorkloadID, 80, false), 6<<10)
			return deployResult(res, "The container did not start. Call diagnose_workload for details.")
		case e.now().After(deadline):
			res.Status = "still_deploying"
			return deployResult(res, "Still starting. Call diagnose_workload in a moment.")
		}
		call.Progress(0, 0, "starting: "+strings.ToLower(firstNonEmpty(w.CurrentPhase, w.Status)))
		select {
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		case <-time.After(e.poll):
		}
	}
}

// plainVariables : variables non secrètes d'un conteneur.
func plainVariables(ctx context.Context, cl *api.Client, id string) (map[string]string, error) {
	vars, err := cl.ListVariables(ctx, id)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, v := range vars.Variables {
		if !v.Secret {
			out[v.Name] = v.Value
		}
	}
	return out, nil
}

func deployResult(res DeployResult, summary string) (mcp.Result, error) {
	var b strings.Builder
	b.WriteString(summary)
	if res.URL != "" {
		b.WriteString("\nURL: " + res.URL)
	}
	b.WriteString("\n\n")
	b.WriteString(jsonText(res))
	failed := res.Status == "build_failed" || res.Status == "deploy_failed"
	return mcp.Result{Text: b.String(), Structured: res, IsError: failed}, nil
}

// deployDir : dossier à déployer. Refuse la racine et le dossier personnel
// entiers : une faute de chemin enverrait tout le disque.
func deployDir(p string) (string, error) {
	if p == "" {
		return "", errors.New(`"path" is required (the project directory)`)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	home, _ := os.UserHomeDir()
	if abs == string(filepath.Separator) || (home != "" && abs == filepath.Clean(home)) {
		return "", fmt.Errorf("refusing to deploy %s as a whole: pass the project directory", abs)
	}
	return abs, nil
}

func validWorkloadName(n string) bool {
	if len(n) < 3 || len(n) > 30 || n[0] == '-' || n[len(n)-1] == '-' {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ── Utilitaires ────────────────────────────────────────────────────────────

// limitedBuffer : garde au plus max octets (le début, ou la fin si keepTail).
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	keepTail bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	if b.buf.Len() > b.max {
		data := b.buf.Bytes()
		if b.keepTail {
			data = append([]byte(nil), data[len(data)-b.max:]...)
		} else {
			data = append([]byte(nil), data[:b.max]...)
		}
		b.buf.Reset()
		b.buf.Write(data)
		if !b.keepTail {
			return len(p), io.ErrShortWrite
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func tailText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 300 {
		s = s[i+1:]
	}
	return "[…]\n" + s
}

func clipText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n[…]"
}

// ── Bases de données ───────────────────────────────────────────────────────

type databaseView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Version     string `json:"version,omitempty"`
	StorageSize string `json:"storage_size"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	User        string `json:"user"`
}

func dbPath(project string) string { return "/projects/" + url.PathEscape(project) + "/databases" }

func (m *mcpCtx) databases(ctx context.Context) ([]databaseView, error) {
	var out []databaseView
	err := m.cl.Do(ctx, http.MethodGet, dbPath(m.project), nil, &out)
	return out, err
}

func (m *mcpCtx) database(ctx context.Context, ref string) (*databaseView, error) {
	if ref == "" {
		return nil, errors.New(`"database" is required`)
	}
	list, err := m.databases(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == ref || list[i].Name == ref {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("no database %q in this project", ref)
}

func (e mcpEnv) listDatabases(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, _, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	list, err := m.databases(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	out := map[string]any{"databases": list}
	return mcp.Result{Text: jsonText(out), Structured: out}, nil
}

func (e mcpEnv) attach(ctx context.Context, m *mcpCtx, db *databaseView, workloadRef, variable string) (string, error) {
	w, err := m.workload(ctx, workloadRef)
	if err != nil {
		return "", err
	}
	var res struct {
		Variable string `json:"variable"`
		Redeploy bool   `json:"redeploy"`
	}
	body := map[string]string{"workload_id": w.ID.String()}
	if variable != "" {
		body["variable"] = variable
	}
	if err := m.cl.Do(ctx, http.MethodPost, dbPath(m.project)+"/"+url.PathEscape(db.ID)+"/attach", body, &res); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Database %q attached to container %q as the secret variable %s.", db.Name, w.Name, res.Variable)
	if res.Redeploy {
		msg += " The container is being redeployed."
	}
	return msg, nil
}

func (e mcpEnv) createDatabase(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	body := map[string]string{"name": str(in["name"])}
	if size := str(in["storage_size"]); size != "" {
		body["storage_size"] = size
	}
	var db databaseView
	if err := m.cl.Do(ctx, http.MethodPost, dbPath(m.project), body, &db); err != nil {
		return mcp.Result{}, err
	}
	deadline := e.now().Add(e.workloadTimeout)
	for db.Status != "READY" {
		if db.Status == "FAILED" {
			return mcp.Result{Text: "The database could not be created.", Structured: db, IsError: true}, nil
		}
		if e.now().After(deadline) {
			return mcp.Result{Text: "The database is still starting; call list_databases in a moment.", Structured: db}, nil
		}
		call.Progress(0, 0, "database "+strings.ToLower(db.Status))
		select {
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		case <-time.After(e.poll):
		}
		if err := m.cl.Do(ctx, http.MethodGet, dbPath(m.project)+"/"+url.PathEscape(db.ID), nil, &db); err != nil {
			return mcp.Result{}, err
		}
	}
	text := fmt.Sprintf("Database %q is ready (Postgres %s, %s). Host %s:%d, database %s, user %s; reachable only from the project's containers.",
		db.Name, firstNonEmpty(db.Version, "?"), db.StorageSize, db.Host, db.Port, db.Database, db.User)
	if target := str(in["attach_to"]); target != "" {
		msg, err := e.attach(ctx, m, &db, target, "")
		if err != nil {
			return mcp.Result{Text: text + "\nAttaching failed: " + err.Error(), Structured: db, IsError: true}, nil
		}
		text += "\n" + msg
	}
	return mcp.Result{Text: text, Structured: db}, nil
}

func (e mcpEnv) attachDatabase(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	db, err := m.database(ctx, str(in["database"]))
	if err != nil {
		return mcp.Result{}, err
	}
	msg, err := e.attach(ctx, m, db, str(in["workload"]), str(in["variable"]))
	if err != nil {
		return mcp.Result{}, err
	}
	return mcp.Result{Text: msg}, nil
}

func (e mcpEnv) restoreDatabase(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	src, err := m.database(ctx, str(in["database"]))
	if err != nil {
		return mcp.Result{}, err
	}
	body := map[string]any{"name": str(in["name"])}
	if t := str(in["target_time"]); t != "" {
		if _, err := time.Parse(time.RFC3339, t); err != nil {
			return mcp.Result{}, errors.New(`"target_time" must be RFC 3339, e.g. 2026-11-18T10:30:00Z`)
		}
		body["target_time"] = t
	}
	var db databaseView
	if err := m.cl.Do(ctx, http.MethodPost, dbPath(m.project)+"/"+url.PathEscape(src.ID)+"/restore", body, &db); err != nil {
		return mcp.Result{}, err
	}
	deadline := e.now().Add(e.buildTimeout) // une restauration peut rejouer beaucoup de journal
	for db.Status != "READY" {
		if db.Status == "FAILED" {
			return mcp.Result{Text: "The restore failed (no usable backup for that time?).", Structured: db, IsError: true}, nil
		}
		if e.now().After(deadline) {
			return mcp.Result{Text: "The restore is still running; call list_databases later.", Structured: db}, nil
		}
		call.Progress(0, 0, "restoring")
		select {
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		case <-time.After(e.poll):
		}
		if err := m.cl.Do(ctx, http.MethodGet, dbPath(m.project)+"/"+url.PathEscape(db.ID), nil, &db); err != nil {
			return mcp.Result{}, err
		}
	}
	return mcp.Result{Text: fmt.Sprintf("Database %q restored from %q and ready. Use attach_database to switch a container to it.", db.Name, src.Name), Structured: db}, nil
}
