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

const mcpInstructions = `betorun hosts containers built from code. Typical flow: call deploy with the project directory ` +
	`(no Dockerfile needed for Next.js, Vite/React/Vue/Astro, Node, Django, FastAPI, Flask, Streamlit or static sites); ` +
	`it builds, deploys and returns the public URL. If the build fails, the result contains the decisive log lines: fix the ` +
	`code and call deploy again. To investigate a running container, call diagnose_workload. Never put secrets in env: ` +
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
				"and returns its public URL. On a build failure, returns the decisive log lines to fix the code.",
			InputSchema: schema(`"path":{"type":"string","description":"Project directory (absolute, or relative to the current directory)"},`+
				`"name":{"type":"string","description":"Container name, 3-30 lowercase characters (default: the directory name)"},`+projectProp+
				`,"port":{"type":"integer","description":"Port the app listens on (default 8080, which betorun builds use)"}`+
				`,"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Plain environment variables (never secrets)"}`, "path"),
			Annotations: mcp.Annotations{Title: "Deploy", OpenWorldHint: true}, Handler: e.deploy},
		{Name: "restart_workload", Description: "Restart a container (rolling replacement, no downtime with several replicas).",
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
	res := DeployResult{Name: name}

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
	list, err := m.cl.ListWorkloads(ctx, m.project)
	if err != nil {
		return mcp.Result{}, err
	}
	for i := range list {
		if list[i].Name == name {
			res.WorkloadID = list[i].ID.String()
		}
	}
	if res.WorkloadID != "" {
		req := api.UpdateWorkloadRequest{Image: build.Image, TargetPort: port}
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
			EnvVars: env, Replicas: 1, TargetPort: port, CPULimit: defaultCPULimit, MemoryLimit: defaultMemoryLimit})
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
