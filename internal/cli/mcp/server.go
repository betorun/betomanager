// Package mcp : serveur Model Context Protocol minimal, sur stdin/stdout.
//
// Assez pour les hôtes d'agents de code (Claude Code, Cursor, Windsurf,
// VS Code) : initialize, ping, tools/list, tools/call, annulation
// (notifications/cancelled) et progression (notifications/progress).
// Aucune dépendance : JSON-RPC 2.0, un message JSON par ligne.
//
// Sortie standard = protocole, rien d'autre. Les journaux vont sur stderr.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Versions du protocole comprises, de la plus récente à la plus ancienne.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Annotations : indications pour l'hôte (demander confirmation ou non).
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// Tool : un outil exposé à l'agent.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations Annotations     `json:"annotations"`
	// Handler : exécute l'outil. Une erreur rendue est montrée à l'agent
	// comme résultat en erreur (isError), pas comme erreur de protocole.
	Handler func(ctx context.Context, call Call) (Result, error) `json:"-"`
}

// Call : un appel d'outil.
type Call struct {
	Arguments json.RawMessage
	// Progress : signale l'avancement (sans effet si l'hôte ne l'a pas demandé).
	Progress func(done, total float64, message string)
}

// Result : résultat d'un outil.
type Result struct {
	Text       string // texte lisible par l'agent
	Structured any    // données structurées (structuredContent), facultatif
	IsError    bool
}

// Server : serveur MCP.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
	// Log : journal (stderr), facultatif.
	Log func(format string, args ...any)

	out      *json.Encoder
	outMu    sync.Mutex
	inflight sync.Map // id (string) → context.CancelFunc
	wg       sync.WaitGroup
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

const maxLine = 4 << 20

// Run : sert jusqu'à la fin de in (l'hôte ferme le canal) ou de ctx.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(nil, nil, &rpcError{codeParse, "parse error"})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if len(req.ID) > 0 {
				s.reply(req.ID, nil, &rpcError{codeInvalidRequest, "invalid request"})
			}
			continue
		}
		s.dispatch(ctx, req)
	}
	// Entrée fermée : les appels en cours vont à leur terme et répondent
	// (un déploiement lancé n'est pas abandonné au milieu).
	s.wg.Wait()
	cancel()
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, req request) {
	notification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.initialize(req.Params), nil)
	case "ping":
		s.reply(req.ID, struct{}{}, nil)
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": s.Tools}, nil)
	case "tools/call":
		// En parallèle : un déploiement de plusieurs minutes ne bloque ni
		// les pings ni les annulations.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.callTool(ctx, req)
		}()
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &p) == nil {
			if c, ok := s.inflight.Load(idKey(p.RequestID)); ok {
				c.(context.CancelFunc)()
			}
		}
	default:
		if !notification { // notifications inconnues (initialized…) : ignorées
			s.reply(req.ID, nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method})
		}
	}
}

func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := protocolVersions[0]
	for _, v := range protocolVersions {
		if v == p.ProtocolVersion {
			version = v
		}
	}
	res := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		res["instructions"] = s.Instructions
	}
	return res
}

func (s *Server) callTool(ctx context.Context, req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.reply(req.ID, nil, &rpcError{codeInvalidParams, "invalid params"})
		return
	}
	var tool *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == p.Name {
			tool = &s.Tools[i]
		}
	}
	if tool == nil {
		s.reply(req.ID, nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name})
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	key := idKey(req.ID)
	s.inflight.Store(key, cancel)
	defer s.inflight.Delete(key)

	call := Call{Arguments: p.Arguments, Progress: func(float64, float64, string) {}}
	if len(p.Meta.ProgressToken) > 0 {
		token := p.Meta.ProgressToken
		call.Progress = func(done, total float64, message string) {
			params := map[string]any{"progressToken": token, "progress": done}
			if total > 0 {
				params["total"] = total
			}
			if message != "" {
				params["message"] = message
			}
			s.notify("notifications/progress", params)
		}
	}
	res, err := s.safeRun(ctx, tool, call)
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return // annulé par l'hôte : pas de réponse (spécification)
	}
	if err != nil {
		res = Result{Text: err.Error(), IsError: true}
	}
	out := map[string]any{
		"content": []map[string]string{{"type": "text", "text": res.Text}},
		"isError": res.IsError,
	}
	if res.Structured != nil {
		out["structuredContent"] = res.Structured
	}
	s.reply(req.ID, out, nil)
}

// safeRun : un outil qui panique rend une erreur, le serveur continue.
func (s *Server) safeRun(ctx context.Context, tool *Tool, call Call) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			if s.Log != nil {
				s.Log("mcp: tool %s panicked: %v", tool.Name, r)
			}
			err = fmt.Errorf("internal error in %s", tool.Name)
		}
	}()
	return tool.Handler(ctx, call)
}

func idKey(id json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return "n:" + n.String()
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return "s:" + strconv.Quote(s)
	}
	return string(id)
}

func (s *Server) reply(id json.RawMessage, result any, e *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		msg["id"] = nil
	}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	s.write(msg)
}

func (s *Server) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *Server) write(msg any) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if err := s.out.Encode(msg); err != nil && s.Log != nil {
		s.Log("mcp: write: %v", err)
	}
}
