package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type sourceArgs struct {
	Source string `json:"source"`
}

type target struct {
	Labels     map[string]string `json:"labels"`
	ScrapePool string            `json:"scrapePool"`
	ScrapeURL  string            `json:"scrapeUrl"`
	Health     string            `json:"health"`
	LastError  string            `json:"lastError"`
	LastScrape time.Time         `json:"lastScrape"`
}

type alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    *time.Time        `json:"activeAt"`
	Value       string            `json:"value"`
}

func (s *Sources) targets(ctx context.Context, source string) ([]target, error) {
	var data struct {
		Active []target `json:"activeTargets"`
	}
	err := s.get(ctx, source, "/api/v1/targets?state=active", nil, &data)
	return data.Active, err
}

func (s *Sources) alerts(ctx context.Context, source string) ([]alert, error) {
	var data struct {
		Alerts []alert `json:"alerts"`
	}
	err := s.get(ctx, source, "/api/v1/alerts", nil, &data)
	return data.Alerts, err
}

func sourcesTool(ctx context.Context, s *Sources, _ json.RawMessage) (string, error) {
	if len(s.names) == 0 {
		return "aucune source Prometheus configurée", nil
	}
	var b strings.Builder
	b.WriteString("SOURCE\tCIBLES EN PANNE\tALERTES ACTIVES\n")
	for _, n := range s.names {
		ts, err := s.targets(ctx, n)
		if err != nil {
			fmt.Fprintf(&b, "%s\tinjoignable : %v\t-\n", n, err)
			continue
		}
		down := 0
		for _, t := range ts {
			if t.Health != "up" {
				down++
			}
		}
		firing := "?"
		if as, err := s.alerts(ctx, n); err == nil {
			c := 0
			for _, a := range as {
				if a.State == "firing" {
					c++
				}
			}
			firing = strconv.Itoa(c)
		}
		fmt.Fprintf(&b, "%s\t%d/%d\t%s\n", n, down, len(ts), firing)
	}
	return b.String(), nil
}

func alertsTool(ctx context.Context, s *Sources, raw json.RawMessage) (string, error) {
	var a sourceArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	as, err := s.alerts(ctx, a.Source)
	if err != nil {
		return "", err
	}
	if len(as) == 0 {
		return "aucune alerte active", nil
	}
	// En cours d'abord, puis les plus anciennes.
	sort.SliceStable(as, func(i, j int) bool {
		if (as[i].State == "firing") != (as[j].State == "firing") {
			return as[i].State == "firing"
		}
		return since(as[i].ActiveAt).Before(since(as[j].ActiveAt))
	})
	now := s.now()
	var b strings.Builder
	b.WriteString("ÉTAT\tALERTE\tSÉVÉRITÉ\tDEPUIS\tLABELS\tRÉSUMÉ\n")
	for i, x := range as {
		if i == maxSeries {
			fmt.Fprintf(&b, "… %d alertes de plus non affichées\n", len(as)-i)
			break
		}
		summary := x.Annotations["summary"]
		if summary == "" {
			summary = x.Annotations["description"]
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", x.State, x.Labels["alertname"], dash(x.Labels["severity"]),
			ago(now, since(x.ActiveAt)), labels(x.Labels, "alertname", "severity"), dash(oneLine(summary)))
	}
	return b.String(), nil
}

func since(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func targetsTool(ctx context.Context, s *Sources, raw json.RawMessage) (string, error) {
	var a struct {
		sourceArgs
		All bool `json:"all"`
	}
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	ts, err := s.targets(ctx, a.Source)
	if err != nil {
		return "", err
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].ScrapePool != ts[j].ScrapePool {
			return ts[i].ScrapePool < ts[j].ScrapePool
		}
		return ts[i].ScrapeURL < ts[j].ScrapeURL
	})
	now := s.now()
	var b strings.Builder
	b.WriteString("ÉTAT\tTÂCHE\tINSTANCE\tDERNIÈRE COLLECTE\tERREUR\n")
	rows := 0
	for _, t := range ts {
		if !a.All && t.Health == "up" {
			continue
		}
		if rows == maxSeries {
			b.WriteString("… cibles suivantes non affichées\n")
			break
		}
		rows++
		lastScrape := "-"
		if !t.LastScrape.IsZero() {
			lastScrape = ago(now, t.LastScrape)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\n", t.Health, t.ScrapePool, dash(t.Labels["instance"]), lastScrape, dash(oneLine(t.LastError)))
	}
	if rows == 0 {
		if a.All {
			return "aucune cible", nil
		}
		return fmt.Sprintf("aucune cible en panne (%d cibles)", len(ts)), nil
	}
	return b.String(), nil
}

type queryArgs struct {
	sourceArgs
	Query string `json:"query"`
	At    string `json:"at"`
	Since string `json:"since"`
}

func (a *queryArgs) check() error {
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" || len(a.Query) > maxQuery {
		return fmt.Errorf("requête PromQL vide ou trop longue (%d caractères au plus)", maxQuery)
	}
	return nil
}

// result : la réponse d'une requête ; Result dépend de ResultType.
type result struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

type series struct {
	Metric map[string]string `json:"metric"`
	Value  sample            `json:"value"`
	Values []sample          `json:"values"`
}

// sample : [horodatage, "valeur"].
type sample struct {
	T float64
	V string
}

func (p *sample) UnmarshalJSON(b []byte) error {
	var raw [2]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[0], &p.T); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &p.V)
}

func queryTool(ctx context.Context, s *Sources, raw json.RawMessage) (string, error) {
	var a queryArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if err := a.check(); err != nil {
		return "", err
	}
	form := url.Values{"query": {a.Query}, "timeout": {queryTimeout.String()}}
	if a.At != "" {
		t, err := time.Parse(time.RFC3339, a.At)
		if err != nil {
			return "", fmt.Errorf("at doit être un instant RFC 3339 : %q", a.At)
		}
		form.Set("time", strconv.FormatInt(t.Unix(), 10))
	}
	var r result
	if err := s.get(ctx, a.Source, "/api/v1/query", form, &r); err != nil {
		return "", err
	}
	switch r.ResultType {
	case "scalar", "string":
		var v sample
		if err := json.Unmarshal(r.Result, &v); err != nil {
			return "", err
		}
		return v.V, nil
	case "vector":
		var ss []series
		if err := json.Unmarshal(r.Result, &ss); err != nil {
			return "", err
		}
		if len(ss) == 0 {
			return "aucune série (résultat vide)", nil
		}
		sortSeries(ss)
		var b strings.Builder
		fmt.Fprintf(&b, "%d séries\nVALEUR\tSÉRIE\n", len(ss))
		for i, x := range ss {
			if i == maxSeries {
				fmt.Fprintf(&b, "… %d séries de plus : agréger (sum by, topk)\n", len(ss)-i)
				break
			}
			fmt.Fprintf(&b, "%s\t%s\n", num(x.Value.V), name(x.Metric))
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("type de résultat inattendu : %s", r.ResultType)
}

func rangeTool(ctx context.Context, s *Sources, raw json.RawMessage) (string, error) {
	var a queryArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if err := a.check(); err != nil {
		return "", err
	}
	span := time.Hour
	if a.Since != "" {
		d, err := duration(a.Since)
		if err != nil || d <= 0 || d > maxRange {
			return "", fmt.Errorf("since : une durée de 7 jours au plus, ex. 30m, 6h, 2d (reçu %q)", a.Since)
		}
		span = d
	}
	step := max(minStep, (span / maxPoints).Round(time.Second))
	end := s.now()
	form := url.Values{"query": {a.Query}, "timeout": {queryTimeout.String()},
		"start": {strconv.FormatInt(end.Add(-span).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)},
		"step": {strconv.Itoa(int(step.Seconds()))}}
	var r result
	if err := s.get(ctx, a.Source, "/api/v1/query_range", form, &r); err != nil {
		return "", err
	}
	if r.ResultType != "matrix" {
		return "", fmt.Errorf("type de résultat inattendu : %s", r.ResultType)
	}
	var ss []series
	if err := json.Unmarshal(r.Result, &ss); err != nil {
		return "", err
	}
	if len(ss) == 0 {
		return "aucune série sur la plage (résultat vide)", nil
	}
	sortSeries(ss)
	var b strings.Builder
	fmt.Fprintf(&b, "%d séries, plage %s, pas %s\n", len(ss), span, step)
	for i, x := range ss {
		if i == 30 {
			fmt.Fprintf(&b, "… %d séries de plus : agréger (sum by, topk)\n", len(ss)-i)
			break
		}
		b.WriteString(summarize(x, end))
	}
	return b.String(), nil
}

// summarize : min, max, dernière valeur, trous, et quelques points répartis
// sur la plage (âge du point = valeur).
func summarize(x series, end time.Time) string {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range x.Values {
		if v, err := strconv.ParseFloat(p.V, 64); err == nil && !math.IsNaN(v) {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", name(x.Metric))
	if len(x.Values) == 0 {
		b.WriteString("  aucun point\n")
		return b.String()
	}
	last := x.Values[len(x.Values)-1]
	fmt.Fprintf(&b, "  min %s  max %s  dernière %s (il y a %s)  points %d\n", num(fmtFloat(lo)), num(fmtFloat(hi)), num(last.V),
		ago(end, unix(last.T)), len(x.Values))
	b.WriteString("  ")
	n := len(x.Values)
	stride := max(1, n/samples)
	var pts []string
	for i := 0; i < n; i += stride {
		p := x.Values[i]
		pts = append(pts, "-"+ago(end, unix(p.T))+"="+num(p.V))
	}
	if (n-1)%stride != 0 {
		pts = append(pts, "-"+ago(end, unix(last.T))+"="+num(last.V))
	}
	b.WriteString(strings.Join(pts, " "))
	b.WriteString("\n")
	return b.String()
}

func unix(t float64) time.Time {
	sec, frac := math.Modf(t)
	return time.Unix(int64(sec), int64(frac*1e9))
}

func fmtFloat(v float64) string {
	if math.IsInf(v, 0) {
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// num : valeur lisible (4 chiffres significatifs).
func num(s string) string {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return s
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e12 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

func sortSeries(ss []series) {
	sort.SliceStable(ss, func(i, j int) bool { return name(ss[i].Metric) < name(ss[j].Metric) })
}

// name : métrique{labels triés}.
func name(m map[string]string) string {
	return m["__name__"] + "{" + labels(m, "__name__") + "}"
}

func labels(m map[string]string, skip ...string) string {
	keys := make([]string, 0, len(m))
outer:
	for k := range m {
		for _, s := range skip {
			if k == s {
				continue outer
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Quote(m[k])
	}
	return strings.Join(parts, ",")
}

// duration : comme time.ParseDuration, avec les jours (d).
func duration(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "…"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
