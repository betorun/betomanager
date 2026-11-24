package agentkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm/llmtest"
)

// slowRunner : chaque appel attend ; compte les appels simultanés.
type slowRunner struct {
	now, peak atomic.Int32
}

func (s *slowRunner) Definitions() []llm.Tool {
	return []llm.Tool{{Name: "read", Description: "d", Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)}}
}

func (s *slowRunner) Run(_ context.Context, _ string, args json.RawMessage) ToolResult {
	n := s.now.Add(1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	var in struct{ N int }
	_ = json.Unmarshal(args, &in)
	time.Sleep(time.Duration(10*(6-in.N)) * time.Millisecond) // le premier finit le dernier
	s.now.Add(-1)
	return ToolResult{Content: fmt.Sprintf("r%d", in.N)}
}

// unsafeEmitter : échoue si deux événements se croisent.
type unsafeEmitter struct {
	busy  atomic.Bool
	mu    sync.Mutex
	count int
	t     *testing.T
}

func (u *unsafeEmitter) enter() {
	if !u.busy.CompareAndSwap(false, true) {
		u.t.Error("événements concurrents")
	}
	time.Sleep(time.Millisecond)
	u.mu.Lock()
	u.count++
	u.mu.Unlock()
	u.busy.Store(false)
}
func (u *unsafeEmitter) Text(string)                  {}
func (u *unsafeEmitter) ToolStart(string, string)     { u.enter() }
func (u *unsafeEmitter) ToolEnd(string, string, bool) { u.enter() }

func batch(n int) llmtest.Step {
	st := llmtest.Step{Usage: llm.Usage{InputTokens: 1, OutputTokens: 1, Known: true}}
	for i := 1; i <= n; i++ {
		st.ToolCalls = append(st.ToolCalls, llm.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "read", Arguments: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
	}
	return st
}

// Lot de 5 lectures, 3 à la fois : résultats dans l'ordre des appels,
// événements jamais simultanés, plafond respecté.
func TestParallelTools(t *testing.T) {
	p := llmtest.New("p", batch(5), llmtest.Say("fini"))
	run := &slowRunner{}
	em := &unsafeEmitter{t: t}
	res, err := (&Agent{LLM: p, Limits: Limits{ParallelTools: 3}}).Run(context.Background(), turn(), run, em)
	if err != nil || res.ToolCalls != 5 {
		t.Fatalf("%+v %v", res, err)
	}
	if pk := run.peak.Load(); pk < 2 || pk > 3 {
		t.Fatalf("appels simultanés : %d", pk)
	}
	if em.count != 10 {
		t.Fatalf("événements : %d", em.count)
	}
	msgs := p.Requests()[1].Messages
	tools := msgs[len(msgs)-5:]
	for i, m := range tools {
		if m.ToolResult == nil || m.ToolResult.CallID != fmt.Sprintf("c%d", i+1) || !strings.Contains(m.ToolResult.Content, fmt.Sprintf("r%d", i+1)) {
			t.Fatalf("résultat %d : %+v", i, m.ToolResult)
		}
	}
}

// Sans ParallelTools : un appel à la fois.
func TestSequentialByDefault(t *testing.T) {
	p := llmtest.New("p", batch(3), llmtest.Say("fini"))
	run := &slowRunner{}
	if _, err := (&Agent{LLM: p}).Run(context.Background(), turn(), run, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if pk := run.peak.Load(); pk != 1 {
		t.Fatalf("appels simultanés : %d", pk)
	}
}
