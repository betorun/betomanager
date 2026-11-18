package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// maxSSELine : une trame ne dépasse jamais cette taille (arguments d'outil
// compris, qui arrivent par morceaux).
const maxSSELine = 1 << 20

// readSSE : lit un flux « Server-Sent Events » et passe chaque champ data à
// onData. S'arrête sur [DONE], sur la fin du corps, ou quand onData rend
// false. Les trames sur plusieurs lignes data: sont recollées ; les
// commentaires (« : ping ») et les champs event/id sont ignorés.
func readSSE(r io.Reader, onData func([]byte) (bool, error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxSSELine)
	var data bytes.Buffer
	flush := func() (bool, error) {
		if data.Len() == 0 {
			return true, nil
		}
		payload := bytes.TrimSpace(data.Bytes())
		defer data.Reset()
		if bytes.Equal(payload, []byte("[DONE]")) {
			return false, nil
		}
		return onData(payload)
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			cont, err := flush()
			if err != nil || !cont {
				return err
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(v, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	_, err := flush() // dernière trame sans ligne vide
	return err
}

// accumulator : reconstruit la réponse complète à partir des morceaux.
// Les appels d'outils arrivent par index : id et nom au premier morceau,
// arguments par fragments.
type accumulator struct {
	text     strings.Builder
	calls    map[int]*llm.ToolCall
	args     map[int]*strings.Builder
	model    string
	stop     llm.StopReason
	usage    llm.Usage
	finished bool
}

func newAccumulator() *accumulator {
	return &accumulator{calls: map[int]*llm.ToolCall{}, args: map[int]*strings.Builder{}, stop: llm.StopUnknown}
}

// add : intègre une trame ; rend le texte nouveau à transmettre.
func (a *accumulator) add(w wireResponse) string {
	if w.Model != "" {
		a.model = w.Model
	}
	if w.Usage != nil {
		a.usage = usage(w.Usage) // trame finale (include_usage), souvent sans choix
	}
	var out string
	for _, ch := range w.Choices {
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			a.text.WriteString(*ch.Delta.Content)
			out += *ch.Delta.Content
		}
		for pos, tc := range ch.Delta.ToolCalls {
			idx := pos
			if tc.Index != nil {
				idx = *tc.Index
			}
			call, ok := a.calls[idx]
			if !ok {
				call = &llm.ToolCall{}
				a.calls[idx] = call
				a.args[idx] = &strings.Builder{}
			}
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if call.Name == "" {
				call.Name = tc.Function.Name // envoyé au premier morceau seulement
			}
			a.args[idx].WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.stop = stopReason(*ch.FinishReason)
			a.finished = true
		}
	}
	return out
}

func (a *accumulator) response() *llm.Response {
	resp := &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Text: a.text.String()},
		Stop:    a.stop,
		Usage:   a.usage,
		Model:   a.model,
	}
	idx := make([]int, 0, len(a.calls))
	for i := range a.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for n, i := range idx {
		call := *a.calls[i]
		call.Arguments = json.RawMessage(a.args[i].String())
		if call.ID == "" {
			call.ID = "call_" + strconv.Itoa(n)
		}
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, call)
	}
	return resp
}
