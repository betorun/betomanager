// Package prom : chemin de secours de l'Opérateur vers Prometheus (ADR 0016
// §1), en lecture.
//
// Chaque source est un Prometheus joignable depuis le serveur de gestion
// (réseau privé ou tunnel), nommé dans OPERATOR_PROMETHEUS. Seule l'API de
// requête est appelée (query, query_range, alerts, targets) : Prometheus ne
// peut rien y modifier. Les requêtes sont bornées (durée, plage, points,
// séries) pour ne jamais charger le Prometheus d'une plateforme en panne.
package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

// Bornes.
const (
	maxQuery     = 2000
	maxResult    = 24 << 10
	maxBody      = 4 << 20
	maxSeries    = 100
	maxRange     = 7 * 24 * time.Hour
	maxPoints    = 300
	minStep      = 15 * time.Second
	samples      = 12
	queryTimeout = 15 * time.Second
	toolTimeout  = 20 * time.Second
)

var sourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Sources : les Prometheus joignables, par nom.
type Sources struct {
	urls  map[string]string
	names []string
	http  *http.Client
	now   func() time.Time
}

// Parse : "nom=url,nom=url" (OPERATOR_PROMETHEUS). Vide = aucune source.
func Parse(spec string) (*Sources, error) {
	urls := map[string]string{}
	for part := range strings.SplitSeq(spec, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, raw, ok := strings.Cut(part, "=")
		name, raw = strings.TrimSpace(name), strings.TrimRight(strings.TrimSpace(raw), "/")
		if !ok || !sourceName.MatchString(name) {
			return nil, fmt.Errorf("operator: source Prometheus invalide : %q (nom=url)", part)
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
			return nil, fmt.Errorf("operator: URL Prometheus invalide pour %s", name)
		}
		if _, dup := urls[name]; dup {
			return nil, fmt.Errorf("operator: source Prometheus en double : %q", name)
		}
		urls[name] = raw
	}
	return New(urls, nil), nil
}

// New : depuis des URL déjà vérifiées (tests) ; hc nil = client par défaut.
func New(urls map[string]string, hc *http.Client) *Sources {
	if hc == nil {
		hc = &http.Client{Timeout: toolTimeout}
	}
	s := &Sources{urls: urls, http: hc, now: time.Now}
	for n := range urls {
		s.names = append(s.names, n)
	}
	sort.Strings(s.names)
	return s
}

// SetClock : l'horloge des plages et des âges (banc d'évaluation, tests).
func (s *Sources) SetClock(now func() time.Time) { s.now = now }

// Names : les sources connues.
func (s *Sources) Names() []string { return s.names }

// apiError : l'erreur rendue par Prometheus (requête invalide, délai…), à
// montrer au modèle pour qu'il corrige sa requête.
type apiError struct{ kind, msg string }

func (e *apiError) Error() string { return "Prometheus : " + e.kind + " : " + e.msg }

// get : appel d'une route de l'API, réponse "data" décodée dans out.
func (s *Sources) get(ctx context.Context, source, path string, form url.Values, out any) error {
	base, ok := s.urls[source]
	if !ok {
		return fmt.Errorf("source Prometheus inconnue %q ; sources : %s", source, strings.Join(s.names, ", "))
	}
	var req *http.Request
	var err error
	if form != nil {
		// POST : les longues requêtes PromQL passent sans souci de longueur d'URL.
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	}
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("Prometheus %s injoignable : %w", source, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return errors.New("réponse trop grosse : restreindre la requête (filtre de labels, agrégation, topk)")
	}
	var env struct {
		Status    string          `json:"status"`
		ErrorType string          `json:"errorType"`
		Error     string          `json:"error"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("Prometheus %s : réponse illisible (HTTP %d)", source, resp.StatusCode)
	}
	if env.Status != "success" {
		return &apiError{kind: env.ErrorType, msg: env.Error}
	}
	return json.Unmarshal(env.Data, out)
}

type tool struct {
	def llm.Tool
	run func(ctx context.Context, s *Sources, args json.RawMessage) (string, error)
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if required == nil {
		req = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `,"additionalProperties":false}`)
}

const (
	pSource = `"source":{"type":"string","description":"Nom de la source Prometheus (prom_sources)"}`
	pQuery  = `"query":{"type":"string","description":"Requête PromQL ; préférer les agrégations (sum by, topk) aux séries brutes"}`
)

var tools = []tool{
	{llm.Tool{Name: "prom_sources", Description: "Liste les Prometheus joignables par le chemin de secours, avec leurs cibles en panne et leurs alertes actives.",
		Parameters: schema("")}, sourcesTool},
	{llm.Tool{Name: "prom_alerts", Description: "Alertes actives d'une source (en cours ou en attente) : nom, sévérité, labels, depuis quand, résumé.",
		Parameters: schema(pSource, "source")}, alertsTool},
	{llm.Tool{Name: "prom_targets", Description: "Cibles de collecte d'une source : seulement celles en panne, sauf all=true ; dernière erreur de collecte.",
		Parameters: schema(pSource+`,"all":{"type":"boolean"}`, "source")}, targetsTool},
	{llm.Tool{Name: "prom_query", Description: "Requête PromQL instantanée (maintenant, ou à l'instant at, RFC 3339). 100 séries au plus.",
		Parameters: schema(pSource+","+pQuery+`,"at":{"type":"string","description":"Instant RFC 3339 ; vide = maintenant"}`, "source", "query")}, queryTool},
	{llm.Tool{Name: "prom_query_range", Description: "Requête PromQL sur une plage (since : durée jusqu'à maintenant, ex. 30m, 6h, 2d ; 7 jours au plus). Chaque série est résumée : min, max, dernière valeur et une douzaine de points.",
		Parameters: schema(pSource+","+pQuery+`,"since":{"type":"string","description":"Durée de la plage, ex. 1h (défaut)"}`, "source", "query")}, rangeTool},
}

// Definitions : agentkit.ToolRunner.
func (s *Sources) Definitions() []llm.Tool {
	out := make([]llm.Tool, len(tools))
	for i, t := range tools {
		out[i] = t.def
	}
	return out
}

// Run : agentkit.ToolRunner. Une erreur est rendue au modèle (IsError).
func (s *Sources) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	for _, t := range tools {
		if t.def.Name != name {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, toolTimeout)
		defer cancel()
		out, err := t.run(ctx, s, args)
		if err != nil {
			return agentkit.ToolResult{Content: err.Error(), IsError: true}
		}
		if len(out) > maxResult {
			return agentkit.ToolResult{Content: cut(out, maxResult), Truncated: true}
		}
		return agentkit.ToolResult{Content: out}
	}
	return agentkit.ToolResult{Content: "outil inconnu : " + name, IsError: true}
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		args = []byte("{}")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return errors.New("arguments invalides : " + err.Error())
	}
	return nil
}

// cut : tronque sur une limite de ligne, sans couper un caractère.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return strings.ToValidUTF8(s, "")
}
