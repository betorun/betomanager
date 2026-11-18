package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// session : serveur branché sur des tuyaux, messages ligne à ligne.
type session struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Scanner
}

func start(t *testing.T, tools ...Tool) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{Name: "betorun", Version: "test", Tools: tools}
	go func() { _ = s.Run(context.Background(), inR, outW); outW.Close() }()
	t.Cleanup(func() { inW.Close() })
	return &session{t: t, in: inW, out: bufio.NewScanner(outR)}
}

func (s *session) send(msg string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, msg+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) next() map[string]any {
	s.t.Helper()
	done := make(chan bool, 1)
	go func() { done <- s.out.Scan() }()
	select {
	case ok := <-done:
		if !ok {
			s.t.Fatal("flux fermé")
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("pas de réponse")
	}
	var m map[string]any
	if err := json.Unmarshal(s.out.Bytes(), &m); err != nil {
		s.t.Fatalf("réponse non JSON : %s", s.out.Bytes())
	}
	return m
}

var echo = Tool{Name: "echo", Description: "e", InputSchema: json.RawMessage(`{"type":"object"}`),
	Handler: func(_ context.Context, c Call) (Result, error) {
		c.Progress(1, 2, "halfway")
		return Result{Text: string(c.Arguments), Structured: map[string]int{"n": 1}}, nil
	}}

var failing = Tool{Name: "fail", Description: "f", InputSchema: json.RawMessage(`{"type":"object"}`),
	Handler: func(context.Context, Call) (Result, error) { return Result{}, errors.New("not signed in") }}

var slow = Tool{Name: "slow", Description: "s", InputSchema: json.RawMessage(`{"type":"object"}`),
	Handler: func(ctx context.Context, _ Call) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}

func TestInitializeAndList(t *testing.T) {
	s := start(t, echo)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t"}}}`)
	r := s.next()
	res := r["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "betorun" {
		t.Fatalf("initialize : %v", r)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`) // pas de réponse
	s.send(`{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	r = s.next()
	if r["id"] != "a" || len(r["result"].(map[string]any)["tools"].([]any)) != 1 {
		t.Fatalf("tools/list : %v", r)
	}
	// Version inconnue : la plus récente.
	s.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := s.next()["result"].(map[string]any)["protocolVersion"]; v != protocolVersions[0] {
		t.Fatalf("version = %v", v)
	}
}

func TestCallsAndErrors(t *testing.T) {
	s := start(t, echo, failing)
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1},"_meta":{"progressToken":"p1"}}}`)
	progress := s.next()
	if progress["method"] != "notifications/progress" || progress["params"].(map[string]any)["progressToken"] != "p1" {
		t.Fatalf("progression : %v", progress)
	}
	r := s.next()
	res := r["result"].(map[string]any)
	if res["isError"] != false || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), `"x":1`) || res["structuredContent"] == nil {
		t.Fatalf("appel : %v", r)
	}
	// Erreur d'outil : résultat isError, pas erreur de protocole.
	s.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`)
	r = s.next()
	if r["error"] != nil || r["result"].(map[string]any)["isError"] != true {
		t.Fatalf("échec d'outil : %v", r)
	}
	s.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`)
	if s.next()["error"] == nil {
		t.Fatal("outil inconnu : erreur attendue")
	}
	s.send(`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`)
	if code := s.next()["error"].(map[string]any)["code"]; code != float64(codeMethodNotFound) {
		t.Fatalf("méthode inconnue : %v", code)
	}
	s.send(`pas du json`)
	if code := s.next()["error"].(map[string]any)["code"]; code != float64(codeParse) {
		t.Fatalf("parse : %v", code)
	}
	s.send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if r := s.next(); r["id"] != float64(7) || r["result"] == nil {
		t.Fatalf("ping : %v", r)
	}
}

// Un appel long n'empêche pas le ping ; une annulation l'arrête sans réponse.
func TestCancelLongCall(t *testing.T) {
	s := start(t, slow)
	s.send(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"slow"}}`)
	s.send(`{"jsonrpc":"2.0","id":11,"method":"ping"}`)
	if r := s.next(); r["id"] != float64(11) {
		t.Fatalf("ping bloqué par l'appel long : %v", r)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10}}`)
	s.send(`{"jsonrpc":"2.0","id":12,"method":"ping"}`)
	if r := s.next(); r["id"] != float64(12) {
		t.Fatalf("après annulation, attendu le ping 12 (aucune réponse à 10) : %v", r)
	}
}
