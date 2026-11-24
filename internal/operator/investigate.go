package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// Limites d'un tour de l'Opérateur (ADR 0016 §1 ter) : une investigation
// suit souvent plusieurs pistes.
var Limits = agentkit.Limits{
	MaxIterations:   30,
	MaxToolCalls:    80,
	Timeout:         10 * time.Minute,
	MaxOutputTokens: 16000, // réflexion adaptative comprise
	// Lectures d'un même lot en parallèle (nœuds, clusters, API) : les
	// outils de l'Opérateur sont sans état partagé, le flux est verrouillé.
	ParallelTools: 4,
}

// digestLen : taille du condensé d'un résultat d'outil, gardé à vie.
const digestLen = 600

// Dossiers : ce que l'investigation lit et écrit dans la base.
type Dossiers interface {
	// Conversation : le dossier et ses échanges, sans les résultats d'outils.
	Conversation(ctx context.Context, id uuid.UUID) (store.Investigation, []store.Step, error)
	AddStep(ctx context.Context, id uuid.UUID, st store.Step) (int, error)
	AddUsage(ctx context.Context, id uuid.UUID, in, out int64) error
}

// Investigator : fait avancer un dossier d'une question.
type Investigator struct {
	Store  Dossiers
	Agent  *agentkit.Agent
	Logger *slog.Logger
	Now    func() time.Time
}

// Ask : pose une question dans un dossier ouvert ; l'agent enquête avec les
// outils ouverts à la personne, chaque étape est enregistrée. Rend la réponse.
func (iv *Investigator) Ask(ctx context.Context, id uuid.UUID, question string, tools agentkit.ToolRunner, emit agentkit.Emitter) (agentkit.Result, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return agentkit.Result{}, errors.New("operator: question vide")
	}
	inv, steps, err := iv.Store.Conversation(ctx, id)
	if err != nil {
		return agentkit.Result{}, err
	}
	if inv.Status != store.StatusOpen {
		return agentkit.Result{}, store.ErrClosed
	}
	history, outcomes := History(steps)
	if _, err := iv.Store.AddStep(ctx, id, store.Step{Kind: store.StepQuestion, Digest: question}); err != nil {
		return agentkit.Result{}, err
	}

	rec := &recorder{tools: tools, store: iv.Store, id: id, logger: iv.Logger, now: iv.now}
	res, runErr := iv.Agent.Run(ctx, agentkit.Turn{
		System:  Prompt(inv, iv.now()),
		History: history,
		Message: withOutcomes(nonEmpty(outcomes), question),
		Caller:  llm.Caller{UserID: inv.ActorUserID, TurnID: id.String()},
	}, rec, emit)

	// Le dossier garde la réponse même partielle, et la raison de l'arrêt.
	// Contexte détaché : un client qui ferme la page n'efface pas la trace.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	answer := res.Answer
	if res.Stop != agentkit.StopComplete {
		answer = strings.TrimSpace(answer + "\n\n[arrêt : " + string(res.Stop) + "]")
	}
	if _, err := iv.Store.AddStep(save, id, store.Step{Kind: store.StepAnswer, Digest: answer, IsError: runErr != nil}); err != nil {
		iv.Logger.Error("operator answer not recorded", "investigation", id, "error", err)
	}
	if res.Usage.Known {
		if err := iv.Store.AddUsage(save, id, int64(res.Usage.InputTokens), int64(res.Usage.OutputTokens)); err != nil {
			iv.Logger.Error("operator usage not recorded", "investigation", id, "error", err)
		}
	}
	return res, runErr
}

func (iv *Investigator) now() time.Time {
	if iv.Now != nil {
		return iv.Now()
	}
	return time.Now().UTC()
}

// History : les échanges précédents du dossier, en texte seulement (les
// résultats d'outils restent dans le dossier, pas dans le contexte). L'issue
// des cartes (validées, exécutées, refusées) précède la question suivante :
// l'agent sait ce qui a été fait entre deux tours. Rend aussi les issues
// survenues après la dernière question, à joindre à la nouvelle.
func History(steps []store.Step) ([]llm.Message, string) {
	var out []llm.Message
	var pending []string
	for _, st := range steps {
		switch st.Kind {
		case store.StepAction:
			pending = append(pending, st.Digest)
		case store.StepQuestion:
			out = append(out, llm.Message{Role: llm.RoleUser, Text: withOutcomes(pending, st.Digest)})
			pending = nil
		case store.StepAnswer:
			if st.Digest != "" {
				out = append(out, llm.Message{Role: llm.RoleAssistant, Text: st.Digest})
			}
		}
	}
	return out, strings.Join(pending, "\n")
}

func withOutcomes(outcomes []string, question string) string {
	if len(outcomes) == 0 {
		return question
	}
	return "[Issue des actions depuis ta dernière réponse]\n" + strings.Join(outcomes, "\n") + "\n\n" + question
}

// recorder : enregistre chaque appel d'outil. Un résultat qui n'a pas pu
// être enregistré n'est pas montré au modèle : aucune conclusion ne repose
// sur une preuve absente du dossier.
type recorder struct {
	tools  agentkit.ToolRunner
	store  Dossiers
	id     uuid.UUID
	logger *slog.Logger
	now    func() time.Time
}

func (r *recorder) Definitions() []llm.Tool { return r.tools.Definitions() }

func (r *recorder) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	start := r.now()
	res := r.tools.Run(ctx, name, args)
	full := res.Content
	var input json.RawMessage
	if json.Valid(args) {
		input = args
	}
	_, err := r.store.AddStep(ctx, r.id, store.Step{
		Kind: store.StepTool, Tool: name, Input: input, Digest: Digest(full),
		Result: &full, IsError: res.IsError, DurationMs: int(r.now().Sub(start).Milliseconds()),
	})
	if err != nil {
		r.logger.Error("operator tool step not recorded", "investigation", r.id, "tool", name, "error", err)
		return agentkit.ToolResult{Content: "résultat non enregistré dans le dossier (base indisponible) : non utilisable", IsError: true}
	}
	return res
}

// Digest : condensé d'un résultat, gardé après la purge des résultats
// complets. Début du texte, coupé sur une ligne, avec le nombre de lignes.
func Digest(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= digestLen {
		return s
	}
	lines := strings.Count(s, "\n") + 1
	cut := s[:digestLen]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndexByte(cut, '\n'); i > digestLen/2 {
		cut = cut[:i]
	}
	return fmt.Sprintf("%s\n… (%d lignes, %d octets)", cut, lines, len(s))
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
