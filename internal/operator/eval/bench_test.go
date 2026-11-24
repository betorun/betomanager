package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/anthropic"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
)

var stage = time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)

// Sans modèle : chaque décor montre bien ce que le scénario attend.
func TestScenarios(t *testing.T) {
	names := map[string]bool{}
	for _, sc := range Scenarios {
		if names[sc.Name] {
			t.Fatalf("scénario en double : %s", sc.Name)
		}
		names[sc.Name] = true
		if len(sc.Cause) == 0 || len(sc.Probes) == 0 || sc.Rubric == "" || sc.MaxToolCalls == 0 {
			t.Errorf("%s : cause, sondes, consigne du juge et budget obligatoires", sc.Name)
		}
		for _, bad := range sc.CheckProbes(context.Background(), stage) {
			t.Errorf("%s : %s", sc.Name, bad)
		}
	}
}

// La chaîne complète avec un modèle scripté : enquête, note, juge.
func TestBenchGrades(t *testing.T) {
	sc := Scenarios[1] // oom
	good := llmtest.New("claude",
		llmtest.Call("c1", "k8s_pods", `{"cluster":"prod-1","namespace":"kora-api"}`),
		llmtest.Say("Le conteneur api est tué par OOMKilled : il dépasse sa limite mémoire de 256 Mi."))
	judge := llmtest.New("juge", llmtest.Say("VERDICT: OK\nCause juste, aucune action risquée."))
	out := (&Bench{Model: good, Judge: judge, Price: Price{Input: 4, Output: 20}}).Run(context.Background(), sc)
	if !out.Pass || out.ToolCalls != 1 || out.Tools[0] != "k8s_pods" || out.Judge == "" {
		t.Fatalf("bonne enquête : %+v", out)
	}
	if !strings.Contains(judge.Requests()[0].Messages[0].Text, "256 Mi") {
		t.Fatal("le juge n'a pas reçu la réponse")
	}

	// Sans preuve et sans la cause : deux échecs ; le juge refuse aussi.
	bad := llmtest.New("claude", llmtest.Say("Sans doute un souci réseau."))
	no := llmtest.New("juge", llmtest.Say("VERDICT: ÉCHEC\nCause fausse."))
	out = (&Bench{Model: bad, Judge: no}).Run(context.Background(), sc)
	if out.Pass || len(out.Failures) != 4 {
		t.Fatalf("mauvaise enquête : %v", out.Failures)
	}
}

// Le banc réel, payant : OPERATOR_EVAL_KEY_FILE (clé Anthropic) l'active.
//
//	OPERATOR_EVAL_KEY_FILE=~/.secrets/anthropic \
//	  go test ./internal/operator/eval/ -run TestBench -count=1 -v -timeout 60m
//
// OPERATOR_EVAL_MODEL (défaut claude-opus-5-5), OPERATOR_EVAL_EFFORT
// (défaut high), OPERATOR_EVAL_RUNS (passages par scénario, défaut 1 ; un
// scénario passe si tous ses passages passent), OPERATOR_EVAL_ONLY (noms
// séparés par des virgules), OPERATOR_EVAL_NO_JUDGE=true, OPERATOR_EVAL_REPORT
// (fichier JSON du rapport).
func TestBench(t *testing.T) {
	keyFile := os.Getenv("OPERATOR_EVAL_KEY_FILE")
	if keyFile == "" {
		t.Skip("banc payant : OPERATOR_EVAL_KEY_FILE absent")
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(raw))
	model := or(os.Getenv("OPERATOR_EVAL_MODEL"), "claude-opus-5-5")
	effort := or(os.Getenv("OPERATOR_EVAL_EFFORT"), "high")
	runs, _ := strconv.Atoi(or(os.Getenv("OPERATOR_EVAL_RUNS"), "1"))
	agent, err := anthropic.New(anthropic.Config{Name: "claude", APIKey: key, Model: model, Effort: effort})
	if err != nil {
		t.Fatal(err)
	}
	b := &Bench{Model: agent, Price: Price{Input: 4, Output: 20}}
	if os.Getenv("OPERATOR_EVAL_NO_JUDGE") != "true" {
		var judge llm.Provider
		if judge, err = anthropic.New(anthropic.Config{Name: "juge", APIKey: key, Model: "claude-opus-5-5", Effort: "medium"}); err != nil {
			t.Fatal(err)
		}
		b.Judge = judge
	}
	only := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("OPERATOR_EVAL_ONLY"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			only[n] = true
		}
	}

	var report []Outcome
	var total float64
	var lines []string
	for _, sc := range Scenarios {
		if len(only) > 0 && !only[sc.Name] {
			continue
		}
		passed := 0
		for i := 0; i < max(1, runs); i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			out := b.Run(ctx, sc)
			cancel()
			report = append(report, out)
			total += out.CostUSD
			if out.Pass {
				passed++
			} else {
				t.Errorf("%s (passage %d) : %s\n--- réponse ---\n%s\n--- juge ---\n%s", sc.Name, i+1, strings.Join(out.Failures, " ; "), out.Answer, out.Judge)
			}
			lines = append(lines, fmt.Sprintf("%-22s %-5t outils %2d  jetons %6d/%5d  %5.3f $  %s", sc.Name, out.Pass, out.ToolCalls,
				out.InputTokens, out.OutputTokens, out.CostUSD, out.Duration.Round(time.Second)))
		}
		lines = append(lines, fmt.Sprintf("%-22s %d/%d", sc.Name, passed, max(1, runs)))
	}
	t.Logf("modèle %s, effort %s\n%s\ncoût total (majorant) : %.3f $", model, effort, strings.Join(lines, "\n"), total)
	if path := os.Getenv("OPERATOR_EVAL_REPORT"); path != "" {
		data, _ := json.MarshalIndent(map[string]any{"model": model, "effort": effort, "at": time.Now().UTC(), "outcomes": report}, "", "  ")
		if err := os.WriteFile(filepath.Clean(path), data, 0o600); err != nil {
			t.Error(err)
		}
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
