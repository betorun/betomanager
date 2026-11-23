package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Plan « mettre en ligne » (lot B). Le modèle ne donne que des paramètres
// (propose_launch_plan) ; le serveur construit les étapes à partir des
// outils existants, toujours dans le même ordre, et n'accepte que des
// ressources relues avec les droits de la personne (compte GitHub, dépôt,
// branche). Pas de langage de références libre : chaque étape sait d'où
// viennent ses identifiants (réponses des étapes précédentes).

// StepKind : sorte d'étape (contrat de la console).
type StepKind string

const (
	StepCreateDatabase StepKind = "create_database"
	StepWaitDatabase   StepKind = "wait_database"
	StepCreateAddon    StepKind = "create_addon"
	StepWaitAddon      StepKind = "wait_addon"
	StepCreateWorkload StepKind = "create_workload"
	StepWaitBuild      StepKind = "wait_build"
	StepAttachDatabase StepKind = "attach_database"
	StepAttachAddon    StepKind = "attach_addon"
	StepAddDomain      StepKind = "add_domain"
	StepWaitRunning    StepKind = "wait_running"
)

// PlanStep : une étape telle que la carte la montre.
type PlanStep struct {
	Key    string            `json:"key"`
	Kind   StepKind          `json:"kind"`
	Params map[string]string `json:"params"`
}

// PlanView : la carte d'un plan (événement SSE action, champ plan).
type PlanView struct {
	Title      string     `json:"title"`
	Repository string     `json:"repository"`
	Branch     string     `json:"branch"`
	Steps      []PlanStep `json:"steps"`
}

// PlanDatabase, PlanAddon : ressources à créer avant le conteneur.
type PlanDatabase struct {
	Name string `json:"name"`
}

type PlanAddon struct {
	Template string `json:"template"`
	Name     string `json:"name"`
}

// LaunchPlan : plan figé à la proposition (gardé avec la carte).
type LaunchPlan struct {
	ProjectID      string        `json:"project_id"`
	Name           string        `json:"name"`
	InstallationID int64         `json:"installation_id"`
	RepoID         int64         `json:"repo_id"`
	Repository     string        `json:"repository"` // full_name lisible
	Branch         string        `json:"branch"`
	ContextDir     string        `json:"context_dir,omitempty"`
	Dockerfile     string        `json:"dockerfile,omitempty"`
	TargetPort     int           `json:"target_port,omitempty"`
	Kind           string        `json:"kind,omitempty"`
	Database       *PlanDatabase `json:"database,omitempty"`
	Addon          *PlanAddon    `json:"addon,omitempty"`
	Hostname       string        `json:"hostname,omitempty"`
	Steps          []PlanStep    `json:"steps"`
}

// View : la carte du plan.
func (lp *LaunchPlan) View() PlanView {
	steps := make([]PlanStep, len(lp.Steps))
	for i, s := range lp.Steps {
		params := make(map[string]string, len(s.Params))
		for k, v := range s.Params {
			params[k] = v
		}
		steps[i] = PlanStep{Key: s.Key, Kind: s.Kind, Params: params}
	}
	return PlanView{Title: lp.Name, Repository: lp.Repository, Branch: lp.Branch, Steps: steps}
}

// ── Arguments du modèle ────────────────────────────────────────────────────

// launchArgs : arguments de propose_launch_plan (champs inconnus refusés).
type launchArgs struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	GitHub    *struct {
		InstallationID int64  `json:"installation_id"`
		Repository     any    `json:"repository"`
		Branch         string `json:"branch"`
		ContextDir     string `json:"context_dir"`
		Dockerfile     string `json:"dockerfile"`
	} `json:"github"`
	TargetPort int           `json:"target_port"`
	Kind       string        `json:"kind"`
	Database   *PlanDatabase `json:"database"`
	Addon      *PlanAddon    `json:"addon"`
	Domain     *struct {
		Hostname string `json:"hostname"`
	} `json:"domain"`
}

// resourceNameRe : nom d'un conteneur, d'une base ou d'un service (3 à 30
// caractères : minuscules, chiffres, tirets). L'API revérifie à l'exécution.
var (
	resourceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,28}[a-z0-9]$`)
	hostnameRe     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62}\.)+[A-Za-z]{2,63}$`)
)

// repoRef : dépôt désigné par son identifiant ou son full_name.
type repoRef struct {
	id       int64
	fullName string
}

// parseLaunchArgs : décode et contrôle les arguments ; le message d'erreur
// est destiné au modèle (il corrige son appel).
func parseLaunchArgs(raw json.RawMessage) (*LaunchPlan, repoRef, error) {
	var in launchArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil {
		return nil, repoRef{}, fmt.Errorf("invalid arguments: %v", err)
	}
	lp := &LaunchPlan{ProjectID: strings.TrimSpace(in.ProjectID), Name: strings.TrimSpace(in.Name),
		TargetPort: in.TargetPort, Kind: strings.TrimSpace(in.Kind)}
	switch {
	case lp.ProjectID == "":
		return nil, repoRef{}, errors.New(`argument "project_id" is required (see list_projects)`)
	case !resourceNameRe.MatchString(lp.Name):
		return nil, repoRef{}, errors.New(`argument "name": 3 to 30 lowercase letters, digits and hyphens`)
	case in.GitHub == nil || in.GitHub.InstallationID <= 0 || strings.TrimSpace(in.GitHub.Branch) == "":
		return nil, repoRef{}, errors.New(`argument "github" needs installation_id, repository and branch (read them with list_github_accounts, list_github_repositories and list_github_branches)`)
	case lp.Kind != "" && lp.Kind != "web" && lp.Kind != "worker":
		return nil, repoRef{}, errors.New(`argument "kind": web or worker`)
	case lp.TargetPort < 0 || lp.TargetPort > 65535:
		return nil, repoRef{}, errors.New(`argument "target_port": 1 to 65535`)
	}
	if lp.Kind == "" {
		lp.Kind = "web"
	}
	ref, err := parseRepoRef(in.GitHub.Repository)
	if err != nil {
		return nil, repoRef{}, err
	}
	lp.InstallationID = in.GitHub.InstallationID
	lp.Branch = strings.TrimSpace(in.GitHub.Branch)
	lp.ContextDir = strings.TrimSpace(in.GitHub.ContextDir)
	lp.Dockerfile = strings.TrimSpace(in.GitHub.Dockerfile)
	if lp.Kind == "worker" && lp.TargetPort != 0 {
		return nil, repoRef{}, errors.New("a worker listens on no port: leave target_port out")
	}
	if in.Database != nil {
		name := strings.TrimSpace(in.Database.Name)
		if !resourceNameRe.MatchString(name) {
			return nil, repoRef{}, errors.New(`argument "database.name": 3 to 30 lowercase letters, digits and hyphens`)
		}
		lp.Database = &PlanDatabase{Name: name}
	}
	if in.Addon != nil {
		a := PlanAddon{Template: strings.TrimSpace(in.Addon.Template), Name: strings.TrimSpace(in.Addon.Name)}
		switch {
		case a.Template != "redis" && a.Template != "mariadb" && a.Template != "meilisearch":
			return nil, repoRef{}, errors.New(`argument "addon.template": redis, mariadb or meilisearch`)
		case !resourceNameRe.MatchString(a.Name):
			return nil, repoRef{}, errors.New(`argument "addon.name": 3 to 30 lowercase letters, digits and hyphens`)
		}
		lp.Addon = &a
	}
	if in.Domain != nil {
		host := strings.ToLower(strings.TrimSpace(in.Domain.Hostname))
		switch {
		case !hostnameRe.MatchString(host):
			return nil, repoRef{}, errors.New(`argument "domain.hostname": a domain name, e.g. app.example.com`)
		case lp.Kind != "web":
			return nil, repoRef{}, errors.New("only a web container takes a domain")
		}
		lp.Hostname = host
	}
	names := map[string]bool{lp.Name: true}
	for _, n := range []string{nameOf(lp.Database), addonName(lp.Addon)} {
		if n == "" {
			continue
		}
		if names[n] {
			return nil, repoRef{}, fmt.Errorf("the name %q is used twice: the container, the database and the service need different names", n)
		}
		names[n] = true
	}
	return lp, ref, nil
}

func nameOf(d *PlanDatabase) string {
	if d == nil {
		return ""
	}
	return d.Name
}

func addonName(a *PlanAddon) string {
	if a == nil {
		return ""
	}
	return a.Name
}

func parseRepoRef(v any) (repoRef, error) {
	bad := errors.New(`argument "github.repository": the full_name (owner/name) or the id read with list_github_repositories`)
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	default:
		return repoRef{}, bad
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return repoRef{}, bad
		}
		return repoRef{id: n}, nil
	}
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/ ") {
		return repoRef{}, bad
	}
	return repoRef{fullName: s}, nil
}

// ── Requêtes des étapes ────────────────────────────────────────────────────

// placeholderID : identifiant de forme valide, pour contrôler à la
// proposition les arguments d'une étape qui dépend d'une ressource à venir.
const placeholderID = "00000000-0000-0000-0000-000000000000"

func (lp *LaunchPlan) workloadArgs() map[string]any {
	args := map[string]any{"id": lp.ProjectID, "name": lp.Name, "installation_id": lp.InstallationID,
		"repo_id": lp.RepoID, "branch": lp.Branch, "kind": lp.Kind}
	if lp.ContextDir != "" {
		args["context_dir"] = lp.ContextDir
	}
	if lp.Dockerfile != "" {
		args["dockerfile"] = lp.Dockerfile
	}
	if lp.TargetPort > 0 {
		args["target_port"] = lp.TargetPort
	}
	return args
}

func (lp *LaunchPlan) databaseArgs() map[string]any {
	return map[string]any{"id": lp.ProjectID, "name": lp.Database.Name}
}

func (lp *LaunchPlan) addonArgs() map[string]any {
	return map[string]any{"id": lp.ProjectID, "template": lp.Addon.Template, "name": lp.Addon.Name}
}

func (lp *LaunchPlan) attachDatabaseArgs(databaseID, workloadID string) map[string]any {
	return map[string]any{"id": lp.ProjectID, "databaseID": databaseID, "workload_id": workloadID}
}

func (lp *LaunchPlan) attachAddonArgs(addonID, workloadID string) map[string]any {
	return map[string]any{"id": lp.ProjectID, "addonID": addonID, "workload_id": workloadID}
}

func (lp *LaunchPlan) domainArgs(workloadID string) map[string]any {
	return map[string]any{"id": workloadID, "hostname": lp.Hostname}
}

// prepare : fige une requête d'outil (arguments contrôlés par le catalogue).
func prepare(cat *tools.Catalog, tool string, args map[string]any) (*tools.Prepared, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return cat.Prepare(tool, raw)
}

// check : chaque requête est préparée une fois à la proposition (les
// identifiants à venir remplacés par un identifiant de forme valide) : des
// arguments faux sont refusés avant la carte.
func (lp *LaunchPlan) check(cat *tools.Catalog) error {
	type request struct {
		tool string
		args map[string]any
	}
	reqs := []request{{tools.ToolCreateWorkloadGitHub, lp.workloadArgs()}}
	if lp.Database != nil {
		reqs = append(reqs, request{tools.ToolCreateDatabase, lp.databaseArgs()},
			request{tools.ToolAttachDatabase, lp.attachDatabaseArgs(placeholderID, placeholderID)})
	}
	if lp.Addon != nil {
		reqs = append(reqs, request{tools.ToolCreateAddon, lp.addonArgs()},
			request{tools.ToolAttachAddon, lp.attachAddonArgs(placeholderID, placeholderID)})
	}
	if lp.Hostname != "" {
		reqs = append(reqs, request{tools.ToolAddWorkloadDomain, lp.domainArgs(placeholderID)})
	}
	for _, r := range reqs {
		if _, err := prepare(cat, r.tool, r.args); err != nil {
			return fmt.Errorf("%s: %v", r.tool, err)
		}
	}
	return nil
}

// buildSteps : étapes demandées, toujours dans cet ordre.
func (lp *LaunchPlan) buildSteps() []PlanStep {
	var steps []PlanStep
	add := func(kind StepKind, params map[string]string) {
		steps = append(steps, PlanStep{Key: string(kind), Kind: kind, Params: params})
	}
	if lp.Database != nil {
		add(StepCreateDatabase, map[string]string{"name": lp.Database.Name})
		add(StepWaitDatabase, map[string]string{"name": lp.Database.Name})
	}
	if lp.Addon != nil {
		add(StepCreateAddon, map[string]string{"template": lp.Addon.Template, "name": lp.Addon.Name})
		add(StepWaitAddon, map[string]string{"name": lp.Addon.Name})
	}
	wl := map[string]string{"name": lp.Name, "repository": lp.Repository, "branch": lp.Branch}
	if lp.TargetPort > 0 {
		wl["port"] = strconv.Itoa(lp.TargetPort)
	}
	add(StepCreateWorkload, wl)
	add(StepWaitBuild, map[string]string{"name": lp.Name, "repository": lp.Repository, "branch": lp.Branch})
	if lp.Database != nil {
		add(StepAttachDatabase, map[string]string{"name": lp.Database.Name})
	}
	if lp.Addon != nil {
		add(StepAttachAddon, map[string]string{"name": lp.Addon.Name})
	}
	if lp.Hostname != "" {
		add(StepAddDomain, map[string]string{"hostname": lp.Hostname})
	}
	add(StepWaitRunning, map[string]string{"name": lp.Name})
	return steps
}

// ── Proposition ────────────────────────────────────────────────────────────

// awaitingPlan : ce que le modèle reçoit quand il propose un plan.
const awaitingPlan = `{"status":"awaiting_validation","note":"A plan card was shown to the person. NOTHING has been done yet: ` +
	`the steps only run, one after the other, if they validate it. Briefly list what the plan will do (and, if no port ` +
	`was read in the repository, that the default port is used); never say it is done."}`

// proposePlan : contrôle les arguments, relit le compte GitHub, le dépôt et
// la branche avec les droits de la personne (introuvable : pas de carte),
// construit les étapes et montre UNE carte.
func (r *BoundRunner) proposePlan(ctx context.Context, t *tools.Tool, args json.RawMessage) tools.Result {
	if r.proposed >= 1 {
		return toolError("only one action can be proposed per message; wait for the person to validate the first one")
	}
	lp, ref, err := parseLaunchArgs(args)
	if err != nil {
		return toolError(err.Error())
	}
	if err := r.resolveGitHub(ctx, lp, ref); err != nil {
		return toolError(err.Error())
	}
	if err := lp.check(r.Cat); err != nil {
		return toolError(err.Error())
	}
	lp.Steps = lp.buildSteps()
	token, err := NewToken()
	if err != nil {
		return toolError("the plan could not be prepared")
	}
	now := r.Svc.CurrentTime().UTC()
	p := &Proposal{Token: token, UserID: r.UserID, ConversationID: r.ConversationID,
		Request: tools.Prepared{Tool: t.Name}, Risk: t.Risk, Target: lp.Name,
		CreatedAt: now, ExpiresAt: now.Add(ProposalTTL), Plan: lp}
	if err := r.Svc.ProposalsStore().Save(ctx, p, ProposalTTL); err != nil {
		r.Svc.Logger.Warnw("copilot: plan not saved", "error", err)
		return toolError("the plan could not be prepared")
	}
	r.proposed++
	view := p.View(nil)
	if r.OnProposal != nil {
		r.OnProposal(view)
	}
	// La carte (dépôt relu, étapes) est une preuve, comme une carte d'action.
	if r.Evidence != nil {
		if raw, err := json.Marshal(view); err == nil {
			r.Evidence.Add(string(raw))
		}
	}
	return tools.Result{Status: http.StatusAccepted, Content: awaitingPlan}
}

// readTool : appelle un outil de lecture et décode sa réponse.
func (r *BoundRunner) readTool(ctx context.Context, tool string, args map[string]any, out any) (tools.Result, error) {
	raw, _ := json.Marshal(args)
	res := r.Cat.Call(ctx, r.Inv, tool, raw)
	if res.IsError {
		return res, fmt.Errorf("%s refused or failed (HTTP %d): %s", tool, res.Status, apiMessage(res.Content))
	}
	if res.Truncated {
		return res, nil // réponse coupée : pas de JSON entier
	}
	if err := json.Unmarshal([]byte(res.Content), out); err != nil {
		return res, fmt.Errorf("%s: unreadable answer", tool)
	}
	return res, nil
}

const (
	// repoPageSize, repoPages : recherche d'un dépôt (réponse sous la borne
	// de 12 Kio d'un outil).
	repoPageSize = 30
	repoPages    = 10
)

// resolveGitHub : le compte est relié au projet, le dépôt lui est
// accessible, la branche existe ; complète l'identifiant et le full_name.
func (r *BoundRunner) resolveGitHub(ctx context.Context, lp *LaunchPlan, ref repoRef) error {
	var accounts []struct {
		InstallationID int64  `json:"installation_id"`
		AccountLogin   string `json:"account_login"`
		Suspended      bool   `json:"suspended"`
	}
	if _, err := r.readTool(ctx, tools.ToolListGitHubAccounts, map[string]any{"id": lp.ProjectID}, &accounts); err != nil {
		return err
	}
	found := false
	for _, a := range accounts {
		if a.InstallationID == lp.InstallationID {
			if a.Suspended {
				return fmt.Errorf("the GitHub account %s is suspended on GitHub: no plan can use it", a.AccountLogin)
			}
			found = true
		}
	}
	if !found {
		return errors.New("this GitHub account is not linked to the project (see list_github_accounts); " +
			"if none is linked, the person links one on the new container screen. Never invent an account")
	}

	type repo struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	var match *repo
	query := ""
	if ref.fullName != "" {
		_, query, _ = strings.Cut(ref.fullName, "/")
	}
	for page := 1; page <= repoPages && match == nil; page++ {
		var out struct {
			Repositories []repo `json:"repositories"`
			HasMore      bool   `json:"has_more"`
		}
		args := map[string]any{"id": lp.ProjectID, "installationID": lp.InstallationID, "page": page, "per_page": repoPageSize}
		if query != "" {
			args["q"] = query
		}
		if _, err := r.readTool(ctx, tools.ToolListGitHubRepositories, args, &out); err != nil {
			return err
		}
		for i := range out.Repositories {
			rp := out.Repositories[i]
			if (ref.id != 0 && rp.ID == ref.id) || (ref.fullName != "" && strings.EqualFold(rp.FullName, ref.fullName)) {
				match = &rp
				break
			}
		}
		if !out.HasMore {
			break
		}
	}
	if match == nil {
		return errors.New("this repository was not found in the GitHub account linked to the project " +
			"(read list_github_repositories): no plan. Never invent a repository")
	}
	lp.RepoID, lp.Repository = match.ID, match.FullName

	var branches struct {
		Branches []string `json:"branches"`
		Default  string   `json:"default"`
	}
	res, err := r.readTool(ctx, tools.ToolListGitHubBranches,
		map[string]any{"id": lp.ProjectID, "installationID": lp.InstallationID, "repoID": lp.RepoID}, &branches)
	if err != nil {
		return err
	}
	if res.Truncated {
		return nil // liste coupée : l'API revérifie la branche à la création
	}
	for _, b := range branches.Branches {
		if b == lp.Branch {
			return nil
		}
	}
	return fmt.Errorf("the branch %q does not exist in %s (default branch: %s; read list_github_branches)",
		lp.Branch, lp.Repository, branches.Default)
}

// apiMessage : message d'erreur de l'API dans un résultat d'outil.
func apiMessage(content string) string {
	var e struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal([]byte(content), &e)
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "request failed"
}
