// Package eval : le banc d'évaluation de l'Opérateur (ADR 0016 §8).
//
// Chaque scénario rejoue une panne avec un faux cluster, de faux journaux et
// un faux Prometheus, puis fait enquêter l'Opérateur avec la vraie boucle
// (Investigator, prompt système, enregistrement des étapes). La note est
// d'abord déterministe (cause trouvée, preuves lues, budget d'outils) ; un
// juge, facultatif, vérifie ce qui ne se lit pas par motif (action
// dangereuse proposée, instruction piégée suivie).
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/operator"
	"github.com/thekrauss/kubemanager/internal/operator/kube"
	"github.com/thekrauss/kubemanager/internal/operator/prom"
)

// Scenario : une panne rejouée.
type Scenario struct {
	Name     string
	Question string
	// Cluster : nom du faux cluster (sujet du dossier).
	Cluster string
	// Objects : l'état du cluster, construit à l'instant du scénario.
	Objects func(now time.Time) []runtime.Object
	// Logs : journaux par "namespace/pod" ("namespace/pod#previous" pour
	// l'instance précédente).
	Logs map[string]string
	// Prometheus : nil = aucune source.
	Prometheus *FakeProm

	// Cause : chaque groupe doit être trouvé dans la réponse (une des
	// expressions du groupe suffit, sans tenir compte de la casse).
	Cause [][]string
	// Evidence : au moins un de ces outils doit avoir été appelé avec succès.
	Evidence []string
	// MaxToolCalls : au-delà, l'enquête est jugée trop coûteuse.
	MaxToolCalls int
	// Rubric : consigne du juge, en plus de la consigne commune.
	Rubric string
	// Probes : vérifient le décor sans modèle (TestScenarios).
	Probes []Probe
}

// Probe : un appel d'outil dont le résultat doit contenir Want.
type Probe struct {
	Tool string
	Args string
	Want string
}

// FakeProm : alertes, cibles et métriques d'un faux Prometheus.
type FakeProm struct {
	Alerts  []map[string]any
	Targets []map[string]any
	Metrics []Metric
}

// Metric : une série renvoyée par toute requête qui contient Match.
type Metric struct {
	Match  string
	Labels map[string]string
	// Values : réparties sur la dernière heure ; la dernière est l'instantané.
	Values []float64
}

// env : les outils d'un scénario ; Close libère le faux Prometheus.
type env struct {
	tools agentkit.ToolRunner
	close func()
}

func (sc Scenario) env(now time.Time) (*env, error) {
	cs := fake.NewSimpleClientset(sc.Objects(now)...)
	clock := func() time.Time { return now }
	clusters := kube.New(map[string]kubernetes.Interface{sc.Cluster: cs})
	clusters.SetClock(clock)
	runners := []agentkit.ToolRunner{&logs{inner: clusters, logs: sc.Logs}}
	closeFn := func() {}
	if sc.Prometheus != nil {
		srv := httptest.NewServer(sc.Prometheus.handler(now))
		closeFn = srv.Close
		sources := prom.New(map[string]string{"plateforme": srv.URL}, nil)
		sources.SetClock(clock)
		runners = append(runners, sources)
	}
	tools, err := operator.Combine(runners...)
	if err != nil {
		closeFn()
		return nil, err
	}
	return &env{tools: tools, close: closeFn}, nil
}

// logs : remplace k8s_pod_logs (le faux clientset rend toujours le même
// texte) par les journaux du scénario.
type logs struct {
	inner agentkit.ToolRunner
	logs  map[string]string
}

func (l *logs) Definitions() []llm.Tool { return l.inner.Definitions() }

func (l *logs) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	if name != "k8s_pod_logs" {
		return l.inner.Run(ctx, name, args)
	}
	var a struct {
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		Previous  bool   `json:"previous"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Namespace == "" || a.Pod == "" {
		return agentkit.ToolResult{Content: "namespace et pod sont obligatoires", IsError: true}
	}
	key := a.Namespace + "/" + a.Pod
	if a.Previous {
		if s, ok := l.logs[key+"#previous"]; ok {
			return agentkit.ToolResult{Content: s}
		}
	}
	if s, ok := l.logs[key]; ok {
		return agentkit.ToolResult{Content: s}
	}
	return agentkit.ToolResult{Content: "journal vide"}
}

func (p *FakeProm) handler(now time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var data any
		switch r.URL.Path {
		case "/api/v1/alerts":
			data = map[string]any{"alerts": orEmpty(p.Alerts)}
		case "/api/v1/targets":
			data = map[string]any{"activeTargets": orEmpty(p.Targets)}
		case "/api/v1/query", "/api/v1/query_range":
			q := r.Form.Get("query")
			series := []map[string]any{}
			for _, m := range p.Metrics {
				if !strings.Contains(q, m.Match) || len(m.Values) == 0 {
					continue
				}
				if r.URL.Path == "/api/v1/query" {
					series = append(series, map[string]any{"metric": m.Labels,
						"value": []any{now.Unix(), fmt.Sprint(m.Values[len(m.Values)-1])}})
					continue
				}
				var vals [][]any
				step := time.Hour / time.Duration(max(1, len(m.Values)-1))
				for i, v := range m.Values {
					vals = append(vals, []any{now.Add(-time.Hour + time.Duration(i)*step).Unix(), fmt.Sprint(v)})
				}
				series = append(series, map[string]any{"metric": m.Labels, "values": vals})
			}
			kind := "vector"
			if r.URL.Path == "/api/v1/query_range" {
				kind = "matrix"
			}
			data = map[string]any{"resultType": kind, "result": series}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
	})
}

func orEmpty(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

// CheckProbes : le décor dit bien ce que le scénario attend (sans modèle).
func (sc Scenario) CheckProbes(ctx context.Context, now time.Time) []string {
	e, err := sc.env(now)
	if err != nil {
		return []string{err.Error()}
	}
	defer e.close()
	var bad []string
	for _, p := range sc.Probes {
		r := e.tools.Run(ctx, p.Tool, json.RawMessage(p.Args))
		if r.IsError || !regexp.MustCompile("(?i)"+p.Want).MatchString(r.Content) {
			bad = append(bad, fmt.Sprintf("%s %s : %q absent de\n%s", p.Tool, p.Args, p.Want, r.Content))
		}
	}
	return bad
}
