package eval

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/operator"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// Price : dollars par million de jetons. Les jetons d'entrée comptent au
// prix plein (lectures du cache comprises) : le coût affiché est un majorant.
type Price struct{ Input, Output float64 }

// Bench : fait enquêter un modèle sur des scénarios.
type Bench struct {
	Model llm.Provider
	// Judge : nil = notation déterministe seule.
	Judge llm.Provider
	Price Price
	// Now : l'instant du décor (défaut : 1er mars 2030, 12 h UTC, fixe pour
	// des âges stables d'un passage à l'autre).
	Now time.Time
}

// Outcome : le résultat d'un scénario.
type Outcome struct {
	Scenario     string        `json:"scenario"`
	Pass         bool          `json:"pass"`
	Failures     []string      `json:"failures,omitempty"`
	Judge        string        `json:"judge,omitempty"`
	Stop         string        `json:"stop"`
	ToolCalls    int           `json:"tool_calls"`
	Tools        []string      `json:"tools"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	CostUSD      float64       `json:"cost_usd"`
	Duration     time.Duration `json:"duration_ns"`
	Answer       string        `json:"answer"`
}

// Run : un scénario, de bout en bout.
func (b *Bench) Run(ctx context.Context, sc Scenario) Outcome {
	now := b.Now
	if now.IsZero() {
		now = time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)
	}
	out := Outcome{Scenario: sc.Name}
	e, err := sc.env(now)
	if err != nil {
		out.Failures = []string{"décor : " + err.Error()}
		return out
	}
	defer e.close()

	dossiers := newDossiers(sc, now)
	iv := &operator.Investigator{Store: dossiers, Agent: &agentkit.Agent{LLM: b.Model, Limits: operator.Limits},
		Now: func() time.Time { return now }}
	start := time.Now()
	res, err := iv.Ask(ctx, dossiers.inv.ID, sc.Question, e.tools, nopEmitter{})
	out.Duration = time.Since(start)
	out.Answer, out.Stop, out.ToolCalls = res.Answer, string(res.Stop), res.ToolCalls
	out.InputTokens, out.OutputTokens = int64(res.Usage.InputTokens), int64(res.Usage.OutputTokens)
	out.CostUSD = (float64(out.InputTokens)*b.Price.Input + float64(out.OutputTokens)*b.Price.Output) / 1e6
	ok := map[string]bool{}
	for _, st := range dossiers.steps {
		if st.Kind == store.StepTool {
			out.Tools = append(out.Tools, st.Tool)
			if !st.IsError {
				ok[st.Tool] = true
			}
		}
	}
	if err != nil {
		out.Failures = append(out.Failures, "enquête : "+err.Error())
		return out
	}
	out.Failures = append(out.Failures, Grade(sc, res, ok)...)
	if b.Judge != nil {
		verdict, pass, err := b.judge(ctx, sc, res.Answer)
		out.Judge = verdict
		switch {
		case err != nil:
			out.Failures = append(out.Failures, "juge : "+err.Error())
		case !pass:
			out.Failures = append(out.Failures, "juge : échec")
		}
	}
	out.Pass = len(out.Failures) == 0
	return out
}

// Grade : la note déterministe. ok : outils appelés sans erreur.
func Grade(sc Scenario, res agentkit.Result, ok map[string]bool) []string {
	var fails []string
	if res.Stop != agentkit.StopComplete {
		fails = append(fails, "arrêt avant la fin : "+string(res.Stop))
	}
	for _, group := range sc.Cause {
		found := false
		for _, pat := range group {
			if regexp.MustCompile("(?i)" + pat).MatchString(res.Answer) {
				found = true
				break
			}
		}
		if !found {
			fails = append(fails, "cause non trouvée : "+strings.Join(group, " | "))
		}
	}
	if len(sc.Evidence) > 0 {
		read := false
		for _, t := range sc.Evidence {
			read = read || ok[t]
		}
		if !read {
			fails = append(fails, "preuve non lue : "+strings.Join(sc.Evidence, " | "))
		}
	}
	if sc.MaxToolCalls > 0 && res.ToolCalls > sc.MaxToolCalls {
		fails = append(fails, fmt.Sprintf("trop d'appels d'outils : %d > %d", res.ToolCalls, sc.MaxToolCalls))
	}
	return fails
}

//go:embed judge.md
var judgePrompt string

var verdictLine = regexp.MustCompile(`(?m)^VERDICT\s*:\s*(OK|ÉCHEC|ECHEC)\b`)

func (b *Bench) judge(ctx context.Context, sc Scenario, answer string) (string, bool, error) {
	msg := fmt.Sprintf("<question>\n%s\n</question>\n<attendu>\n%s\n</attendu>\n<reponse>\n%s\n</reponse>", sc.Question, sc.Rubric, answer)
	resp, err := b.Judge.Generate(ctx, llm.Request{System: judgePrompt, MaxOutputTokens: 4000,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: msg}}})
	if err != nil {
		return "", false, err
	}
	text := strings.TrimSpace(resp.Message.Text)
	m := verdictLine.FindStringSubmatch(text)
	if m == nil {
		return text, false, fmt.Errorf("verdict illisible")
	}
	return text, m[1] == "OK", nil
}

type nopEmitter struct{}

func (nopEmitter) Text(string)                  {}
func (nopEmitter) ToolStart(string, string)     {}
func (nopEmitter) ToolEnd(string, string, bool) {}

// dossiers : un dossier en mémoire (operator.Dossiers).
type dossiers struct {
	mu    sync.Mutex
	inv   store.Investigation
	steps []store.Step
}

func newDossiers(sc Scenario, now time.Time) *dossiers {
	return &dossiers{inv: store.Investigation{ID: uuid.New(), SubjectKind: store.SubjectCluster, SubjectRef: sc.Cluster,
		ActorUserID: "eval", ActorEmail: "banc@betorun.test", Status: store.StatusOpen, CreatedAt: now, UpdatedAt: now}}
}

func (d *dossiers) Get(_ context.Context, _ uuid.UUID) (store.Investigation, []store.Step, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inv, append([]store.Step(nil), d.steps...), nil
}

func (d *dossiers) AddStep(_ context.Context, _ uuid.UUID, st store.Step) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st.Seq = len(d.steps) + 1
	d.steps = append(d.steps, st)
	return st.Seq, nil
}

func (d *dossiers) AddUsage(_ context.Context, _ uuid.UUID, in, out int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inv.InputTokens += in
	d.inv.OutputTokens += out
	return nil
}
