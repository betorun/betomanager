package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Exécution d'un plan « mettre en ligne » validé : les étapes sont jouées à
// la suite, côté serveur, dans une goroutine détachée de la requête. Chaque
// écriture passe par sa route (cat.Execute) avec un jeton délégué « write »
// NEUF de 2 min (une étape = un jeton) ; les attentes relisent l'état avec
// des jetons « read ». Un échec arrête le plan, jamais de nouvelle tentative.

// PlanTimings : bornes et intervalle de relecture (zéro : valeurs par défaut).
type PlanTimings struct {
	// Poll : intervalle de relecture d'une attente (3 s).
	Poll time.Duration
	// BuildWait : attente du premier build (20 min).
	BuildWait time.Duration
	// StepWait : attente d'une base, d'un service, du conteneur (5 min).
	StepWait time.Duration
	// Total : plan entier (30 min).
	Total time.Duration
}

func (t PlanTimings) withDefaults() PlanTimings {
	if t.Poll <= 0 {
		t.Poll = 3 * time.Second
	}
	if t.BuildWait <= 0 {
		t.BuildWait = 20 * time.Minute
	}
	if t.StepWait <= 0 {
		t.StepWait = 5 * time.Minute
	}
	if t.Total <= 0 {
		t.Total = 30 * time.Minute
	}
	return t
}

// planWriteTTL : jeton d'une étape d'écriture ; planReadTTL : d'une relecture.
const (
	planWriteTTL = 2 * time.Minute
	planReadTTL  = 2 * time.Minute
)

var plansTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "kubemanager", Subsystem: "copilot", Name: "plans_total",
	Help: "Plans « mettre en ligne » du Copilot, par issue (done|failed|paused).",
}, []string{"outcome"})

// PlanCaller : la personne qui a validé le plan (sa session signe les jetons
// délégués de chaque étape ; son adresse et sa langue suivent les appels).
type PlanCaller struct {
	UserID         uuid.UUID
	SessionID      uuid.UUID
	ACR            string
	ClientIP       string
	AcceptLanguage string
}

// StartPlan : état initial enregistré, note dans la conversation, plan joué
// en arrière-plan. ctx : celui de la requête de validation (détaché ici).
func (s *Service) StartPlan(ctx context.Context, p *Proposal, caller PlanCaller) (*PlanState, error) {
	s.Mu.RLock()
	cat, handler := s.Catalog, s.Handler
	s.Mu.RUnlock()
	now := s.CurrentTime().UTC()
	st := PlanState{Token: p.Token, Status: PlanRunning, Title: p.Plan.Name, StartedAt: now,
		Steps: make([]StepState, len(p.Plan.Steps))}
	for i, step := range p.Plan.Steps {
		params := make(map[string]string, len(step.Params))
		for k, v := range step.Params {
			params[k] = v
		}
		st.Steps[i] = StepState{Key: step.Key, Kind: step.Kind, Params: params, Status: StepPending}
	}
	rec := &PlanRecord{UserID: p.UserID, ConversationID: p.ConversationID, HeartbeatAt: now, State: st}
	if err := s.PlansStore().Save(ctx, rec); err != nil {
		return nil, err
	}
	kinds := make([]string, len(st.Steps))
	for i, step := range st.Steps {
		kinds[i] = string(step.Kind)
	}
	s.Note(context.WithoutCancel(ctx), p.UserID, p.ConversationID, fmt.Sprintf(
		"[The person validated the launch plan for %q from %s (branch %s). It now runs on the server, step by step: %s. "+
			"The console shows its progress; do not claim it is finished before a note says so.]",
		p.Plan.Name, p.Plan.Repository, p.Plan.Branch, strings.Join(kinds, ", ")))

	t := s.PlanTimings.withDefaults()
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.Total)
	run := &planRun{s: s, cat: cat, handler: handler, caller: caller, plan: p.Plan, rec: rec, t: t}
	out := rec.State
	out.Steps = append([]StepState(nil), rec.State.Steps...)
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				s.Logger.Errorw("copilot: plan panicked", "panic", r)
			}
		}()
		run.run(runCtx)
	}()
	return &out, nil
}

type planRun struct {
	s       *Service
	cat     *tools.Catalog
	handler http.Handler
	caller  PlanCaller
	plan    *LaunchPlan
	rec     *PlanRecord
	t       PlanTimings
}

// errPaused : la personne a demandé l'arrêt pendant une attente.
var errPaused = errors.New("paused")

func (r *planRun) now() time.Time { return r.s.CurrentTime().UTC() }

func (r *planRun) save() {
	r.rec.HeartbeatAt = r.now()
	if err := r.s.PlansStore().Save(context.Background(), r.rec); err != nil {
		r.s.Logger.Warnw("copilot: plan state not saved", "error", err)
	}
}

func (r *planRun) paused() bool {
	return r.s.PlansStore().PauseRequested(context.Background(), r.rec.State.Token)
}

func (r *planRun) run(ctx context.Context) {
	st := &r.rec.State
	failedAt := ""
	for i := range st.Steps {
		step := &st.Steps[i]
		if r.paused() {
			st.Status = PlanPaused
			break
		}
		if ctx.Err() != nil {
			st.Status, failedAt = PlanFailed, step.Key
			step.Status, step.Message = StepFailed, "the plan exceeded its maximum duration"
			break
		}
		started := r.now()
		step.Status, step.StartedAt = StepRunning, &started
		r.save()
		dns, err := r.exec(ctx, step)
		ended := r.now()
		step.EndedAt = &ended
		switch {
		case errors.Is(err, errPaused):
			step.Status, step.Message = StepSkipped, "paused"
			st.Status = PlanPaused
		case err != nil:
			if ctx.Err() != nil {
				err = errors.New("the plan exceeded its maximum duration")
			}
			step.Status, step.Message = StepFailed, err.Error()
			st.Status, failedAt = PlanFailed, step.Key
		default:
			step.Status, step.DNS = StepDone, dns
		}
		if st.Status != PlanRunning {
			break
		}
		r.save()
	}
	if st.Status == PlanRunning {
		st.Status = PlanDone
	}
	for i := range st.Steps {
		if st.Steps[i].Status == StepPending {
			st.Steps[i].Status = StepSkipped
		}
	}
	ended := r.now()
	st.EndedAt = &ended
	r.save()
	plansTotal.WithLabelValues(st.Status).Inc()
	r.s.Note(context.Background(), r.rec.UserID, r.rec.ConversationID, r.summary(failedAt))
}

// summary : note de fin pour le tour suivant (identifiants des ressources
// créées, enregistrements DNS à poser).
func (r *planRun) summary(failedAt string) string {
	st := &r.rec.State
	var b strings.Builder
	fmt.Fprintf(&b, "[The launch plan for %q ended: %s", st.Title, st.Status)
	if failedAt != "" {
		for _, s := range st.Steps {
			if s.Key == failedAt {
				fmt.Fprintf(&b, " at step %s (%s)", s.Key, s.Message)
			}
		}
	}
	res := st.Resources
	for _, kv := range [][2]string{{"container id", res.WorkloadID}, {"build id", res.BuildID},
		{"database id", res.DatabaseID}, {"service id", res.AddonID}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, ", %s %s", kv[0], kv[1])
		}
	}
	for _, s := range st.Steps {
		if s.DNS != nil {
			fmt.Fprintf(&b, ", DNS record to create for %s: %s %s %s", s.Params["hostname"], s.DNS.Type, s.DNS.Name, s.DNS.Value)
		}
	}
	if st.Status != PlanDone {
		b.WriteString(". Steps after the stop were skipped; nothing is retried automatically")
	}
	b.WriteString(".]")
	return b.String()
}

// invoker : jeton délégué neuf de la personne, portée scope.
func (r *planRun) invoker(scope string, ttl time.Duration) (tools.Invoker, error) {
	token, _, err := r.s.STS.MintDelegated(sts.Delegation{UserID: r.caller.UserID, SessionID: r.caller.SessionID,
		ACR: r.caller.ACR, Actor: sts.ActorCopilot, Scope: scope, TTL: ttl})
	if err != nil {
		r.s.Logger.Errorw("copilot: plan delegated token", "scope", scope, "error", err)
		return tools.Invoker{}, errors.New("the Copilot could not act on your behalf (session ended?)")
	}
	return tools.Invoker{Handler: r.handler, Token: token, ClientIP: r.caller.ClientIP, AcceptLanguage: r.caller.AcceptLanguage}, nil
}

// write : une étape d'écriture, jouée une fois (clé d'idempotence = jeton du
// plan + clé d'étape), réponse décodée dans out.
func (r *planRun) write(ctx context.Context, step *StepState, tool string, args map[string]any, out any) error {
	prepared, err := prepare(r.cat, tool, args)
	if err != nil {
		return err
	}
	inv, err := r.invoker(sts.ScopeWrite, planWriteTTL)
	if err != nil {
		return err
	}
	res := r.cat.Execute(ctx, inv, prepared, r.rec.State.Token+":"+step.Key)
	if res.IsError {
		return errors.New(apiMessage(res.Content))
	}
	if out != nil && json.Unmarshal([]byte(res.Content), out) != nil {
		return errors.New("unreadable answer")
	}
	return nil
}

// statusSets : issue d'une attente selon le statut relu.
type statusSets struct {
	ok   map[string]bool
	fail map[string]bool
}

func set(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// Statuts réels : bases (internal/modules/databases/domain), builds
// (internal/modules/builds/domain), conteneurs et services, dont le statut
// est celui de leur conteneur porteur (internal/modules/utils/constants.go).
var (
	databaseStatuses = statusSets{ok: set("READY"), fail: set("FAILED", "DELETING", "STOPPED")}
	buildStatuses    = statusSets{ok: set("SUCCEEDED"), fail: set("FAILED", "CANCELLED")}
	workloadStatuses = statusSets{ok: set("RUNNING"), fail: set("FAILED", "STOPPED", "DELETING", "DELETED", "DELETE_FAILED",
		"CRASHLOOP", "IMAGE_PULL_FAILED", "CONFIG_ERROR", "OOM_KILLED", "EVICTED", "RUN_FAILED")}
)

// wait : relit tool jusqu'à un statut de réussite ou d'échec, au plus bound.
// Une réponse 4xx de la relecture arrête l'attente ; une erreur passagère
// (5xx, 429) est relue au tour suivant.
func (r *planRun) wait(ctx context.Context, bound time.Duration, tool string, args map[string]any, sets statusSets) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	raw, _ := json.Marshal(args)
	for {
		inv, err := r.invoker(sts.ScopeRead, planReadTTL)
		if err != nil {
			return err
		}
		res := r.cat.Call(ctx, inv, tool, raw)
		r.save() // battement de cœur
		switch {
		case res.IsError && res.Status >= 400 && res.Status < 500 && res.Status != http.StatusTooManyRequests:
			return errors.New(apiMessage(res.Content))
		case !res.IsError:
			var v struct {
				Status  string `json:"status"`
				Failure string `json:"failure"`
			}
			_ = json.Unmarshal([]byte(res.Content), &v)
			if sets.ok[v.Status] {
				return nil
			}
			if sets.fail[v.Status] {
				if v.Failure != "" {
					return fmt.Errorf("status %s (%s)", v.Status, v.Failure)
				}
				return fmt.Errorf("status %s", v.Status)
			}
		}
		if r.paused() {
			return errPaused
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready after %s", bound.Round(time.Second))
		case <-time.After(r.t.Poll):
		}
	}
}

// exec : joue une étape ; identifiants pris des réponses précédentes.
func (r *planRun) exec(ctx context.Context, step *StepState) (*DNSRecord, error) {
	lp, res := r.plan, &r.rec.State.Resources
	switch step.Kind {
	case StepCreateDatabase:
		var out struct {
			ID string `json:"id"`
		}
		if err := r.write(ctx, step, tools.ToolCreateDatabase, lp.databaseArgs(), &out); err != nil {
			return nil, err
		}
		if out.ID == "" {
			return nil, errors.New("the database id was not returned")
		}
		res.DatabaseID = out.ID
	case StepWaitDatabase:
		return nil, r.wait(ctx, r.t.StepWait, tools.ToolGetDatabase,
			map[string]any{"id": lp.ProjectID, "databaseID": res.DatabaseID}, databaseStatuses)
	case StepCreateAddon:
		var out struct {
			ID string `json:"id"`
		}
		if err := r.write(ctx, step, tools.ToolCreateAddon, lp.addonArgs(), &out); err != nil {
			return nil, err
		}
		if out.ID == "" {
			return nil, errors.New("the service id was not returned")
		}
		res.AddonID = out.ID
	case StepWaitAddon:
		return nil, r.wait(ctx, r.t.StepWait, tools.ToolGetAddon,
			map[string]any{"id": lp.ProjectID, "addonID": res.AddonID}, workloadStatuses)
	case StepCreateWorkload:
		var out struct {
			WorkloadID string `json:"workload_id"`
			Build      *struct {
				ID string `json:"id"`
			} `json:"build"`
		}
		if err := r.write(ctx, step, tools.ToolCreateWorkloadGitHub, lp.workloadArgs(), &out); err != nil {
			return nil, err
		}
		if out.WorkloadID == "" {
			return nil, errors.New("the container id was not returned")
		}
		res.WorkloadID = out.WorkloadID
		if out.Build != nil {
			res.BuildID = out.Build.ID
		}
	case StepWaitBuild:
		if res.BuildID == "" {
			return nil, errors.New("no build was started for the container")
		}
		return nil, r.wait(ctx, r.t.BuildWait, tools.ToolGetBuild,
			map[string]any{"id": lp.ProjectID, "buildID": res.BuildID}, buildStatuses)
	case StepAttachDatabase:
		return nil, r.write(ctx, step, tools.ToolAttachDatabase, lp.attachDatabaseArgs(res.DatabaseID, res.WorkloadID), nil)
	case StepAttachAddon:
		return nil, r.write(ctx, step, tools.ToolAttachAddon, lp.attachAddonArgs(res.AddonID, res.WorkloadID), nil)
	case StepAddDomain:
		var out struct {
			ID      string `json:"id"`
			Records []struct {
				Type        string `json:"type"`
				Name        string `json:"name"`
				Host        string `json:"host"`
				Value       string `json:"value"`
				Alternative bool   `json:"alternative"`
			} `json:"records"`
		}
		if err := r.write(ctx, step, tools.ToolAddWorkloadDomain, lp.domainArgs(res.WorkloadID), &out); err != nil {
			return nil, err
		}
		r.rec.DomainID = out.ID
		// Premier enregistrement à poser (preuve de propriété, sinon routage).
		for _, rc := range out.Records {
			if rc.Alternative {
				continue
			}
			name := rc.Host
			if name == "" {
				name = rc.Name
			}
			return &DNSRecord{Type: rc.Type, Name: name, Value: rc.Value}, nil
		}
	case StepWaitRunning:
		return nil, r.wait(ctx, r.t.StepWait, tools.ToolGetWorkload, map[string]any{"id": res.WorkloadID}, workloadStatuses)
	default:
		return nil, fmt.Errorf("unknown step %q", step.Kind)
	}
	return nil, nil
}
