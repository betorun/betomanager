package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// APITools : le chemin normal (ADR 0016 §1) : les outils de lecture des
// routes /admin, appelés par l'API avec le jeton délégué de la personne et
// le motif du dossier comme justification. L'Opérateur ne fabrique aucune
// URL de l'API : il nomme un outil du catalogue que l'API lui donne.
type APITools struct {
	BaseURL string // …/kmanager/v1
	HTTP    *http.Client
}

// For : les outils ouverts à la personne, pour un tour.
func (a *APITools) For(ctx context.Context, id Identity, reason string) (agentkit.ToolRunner, error) {
	if a == nil || a.BaseURL == "" {
		return nil, ErrAPIUnavailable
	}
	var out struct {
		Items []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"items"`
	}
	status, body, err := a.do(ctx, http.MethodGet, "/admin/operator/tools", id.Token, "", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w (catalogue : HTTP %d)", ErrAPIUnavailable, status)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w (catalogue illisible)", ErrAPIUnavailable)
	}
	r := &apiRunner{api: a, token: id.Token, reason: reason}
	for _, t := range out.Items {
		r.defs = append(r.defs, llm.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return r, nil
}

func (a *APITools) do(ctx context.Context, method, path, token, reason string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept-Language", "fr")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if reason != "" {
		req.Header.Set("X-Betorun-Justification", reason)
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w (%v)", ErrAPIUnavailable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

type apiRunner struct {
	api    *APITools
	token  string
	reason string
	defs   []llm.Tool
}

func (r *apiRunner) Definitions() []llm.Tool { return r.defs }

func (r *apiRunner) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	if !json.Valid(args) {
		args = json.RawMessage("{}")
	}
	status, body, err := r.api.do(ctx, http.MethodPost, "/admin/operator/tools/"+url.PathEscape(name), r.token, r.reason,
		map[string]json.RawMessage{"arguments": args})
	if err != nil {
		return agentkit.ToolResult{Content: "API injoignable : " + err.Error(), IsError: true}
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return agentkit.ToolResult{Content: "jeton expiré : la personne doit recharger la page de l'Opérateur", IsError: true}
	default:
		return agentkit.ToolResult{Content: fmt.Sprintf("refusé par l'API (HTTP %d) : %s", status, clip(body)), IsError: true}
	}
	var res struct {
		Content   string `json:"content"`
		IsError   bool   `json:"is_error"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return agentkit.ToolResult{Content: "réponse de l'API illisible", IsError: true}
	}
	return agentkit.ToolResult{Content: res.Content, IsError: res.IsError, Truncated: res.Truncated}
}

func clip(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(bytes.ToValidUTF8(b, nil))
}

// toolSet : plusieurs catalogues vus comme un seul.
type toolSet struct {
	runners []agentkit.ToolRunner
	byName  map[string]agentkit.ToolRunner
	defs    []llm.Tool
}

// Combine : réunit des catalogues ; un nom en double est une erreur.
func Combine(runners ...agentkit.ToolRunner) (agentkit.ToolRunner, error) {
	s := &toolSet{byName: map[string]agentkit.ToolRunner{}}
	for _, r := range runners {
		if r == nil {
			continue
		}
		for _, d := range r.Definitions() {
			if _, dup := s.byName[d.Name]; dup {
				return nil, fmt.Errorf("operator: outil en double : %s", d.Name)
			}
			s.byName[d.Name] = r
			s.defs = append(s.defs, d)
		}
	}
	return s, nil
}

func (s *toolSet) Definitions() []llm.Tool { return s.defs }

func (s *toolSet) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	r, ok := s.byName[name]
	if !ok {
		return agentkit.ToolResult{Content: "outil inconnu : " + name, IsError: true}
	}
	return r.Run(ctx, name, args)
}
