package llm

import (
	"context"
	"iter"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Métriques. Labels bornés seulement (contrat de
// internal/infrastructure/observability) : provider = nom de configuration
// (deux ou trois au plus), outcome = Kind ou "ok", mode = generate|stream.
// Jamais d'organisation, d'utilisateur ni de modèle libre.
var (
	llmCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "llm", Name: "calls_total",
		Help: "Appels aux modèles de langage, par fournisseur, mode et résultat.",
	}, []string{"provider", "mode", "outcome"})

	llmDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "kubemanager", Subsystem: "llm", Name: "call_duration_seconds",
		Help:    "Durée des appels aux modèles (flux : jusqu'au dernier événement).",
		Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 15, 30, 60, 120},
	}, []string{"provider", "mode"})

	llmFirstToken = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "kubemanager", Subsystem: "llm", Name: "time_to_first_event_seconds",
		Help:    "Délai avant le premier événement d'un flux (ce que l'utilisateur ressent).",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 15},
	}, []string{"provider"})

	llmTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubemanager", Subsystem: "llm", Name: "tokens_total",
		Help: "Jetons consommés, par fournisseur et sens (input|output).",
	}, []string{"provider", "direction"})
)

// Instrumented : mesure chaque appel du fournisseur (ou de la chaîne) enveloppé.
// Le label provider est celui de la réponse quand il est connu (chaîne : le
// fournisseur qui a vraiment répondu).
type Instrumented struct {
	next Provider
}

func Instrument(p Provider) *Instrumented { return &Instrumented{next: p} }

func (i *Instrumented) Name() string { return i.next.Name() }

func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	return string(KindOf(err))
}

func providerOf(resp *Response, err error, fallback string) string {
	if resp != nil && resp.Provider != "" {
		return resp.Provider
	}
	if e := asError(fallback, err); err != nil && e.Provider != "" {
		return e.Provider
	}
	return fallback
}

func observeUsage(provider string, u Usage) {
	if !u.Known {
		return
	}
	llmTokens.WithLabelValues(provider, "input").Add(float64(u.InputTokens))
	llmTokens.WithLabelValues(provider, "output").Add(float64(u.OutputTokens))
}

func (i *Instrumented) Generate(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	resp, err := i.next.Generate(ctx, req)
	p := providerOf(resp, err, i.Name())
	llmCalls.WithLabelValues(p, "generate", outcome(err)).Inc()
	llmDuration.WithLabelValues(p, "generate").Observe(time.Since(start).Seconds())
	if resp != nil {
		observeUsage(p, resp.Usage)
	}
	return resp, err
}

func (i *Instrumented) Stream(ctx context.Context, req Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		start := time.Now()
		first := true
		var final error
		var resp *Response
		defer func() {
			p := providerOf(resp, final, i.Name())
			out := outcome(final)
			if final == nil && resp == nil {
				out = string(KindCanceled) // l'appelant a quitté la boucle
			}
			llmCalls.WithLabelValues(p, "stream", out).Inc()
			llmDuration.WithLabelValues(p, "stream").Observe(time.Since(start).Seconds())
			if resp != nil {
				observeUsage(p, resp.Usage)
			}
		}()
		for ev, err := range i.next.Stream(ctx, req) {
			if err != nil {
				final = err
				yield(Event{}, err)
				return
			}
			if first {
				first = false
				p := i.Name()
				if ev.Response != nil && ev.Response.Provider != "" {
					p = ev.Response.Provider
				}
				llmFirstToken.WithLabelValues(p).Observe(time.Since(start).Seconds())
			}
			if ev.Kind == EventDone {
				resp = ev.Response
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}
