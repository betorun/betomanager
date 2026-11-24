package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var now = time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)

// fakeProm : répond comme l'API HTTP de Prometheus ; garde les formulaires.
type fakeProm struct {
	mu    sync.Mutex
	forms []map[string]string
}

func (f *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := map[string]string{"method": r.Method, "path": r.URL.Path}
	for k := range r.Form {
		form[k] = r.Form.Get(k)
	}
	f.mu.Lock()
	f.forms = append(f.forms, form)
	f.mu.Unlock()
	ok := func(data string) { fmt.Fprintf(w, `{"status":"success","data":%s}`, data) }
	switch r.URL.Path {
	case "/api/v1/targets":
		ok(`{"activeTargets":[
			{"labels":{"instance":"api:8080","job":"api"},"scrapePool":"api","scrapeUrl":"http://api:8080/metrics","health":"up","lastError":"","lastScrape":"2030-03-01T11:59:50Z"},
			{"labels":{"instance":"temporal:9090","job":"temporal"},"scrapePool":"temporal","scrapeUrl":"http://temporal:9090/metrics","health":"down","lastError":"connection refused","lastScrape":"2030-03-01T11:59:45Z"}]}`)
	case "/api/v1/alerts":
		ok(`{"alerts":[
			{"labels":{"alertname":"Pending","severity":"info"},"annotations":{},"state":"pending","activeAt":"2030-03-01T11:58:00Z","value":"1"},
			{"labels":{"alertname":"TemporalDown","severity":"critical","job":"temporal"},"annotations":{"summary":"Temporal ne répond plus"},"state":"firing","activeAt":"2030-03-01T11:30:00Z","value":"0"}]}`)
	case "/api/v1/query":
		switch r.Form.Get("query") {
		case "up":
			ok(`{"resultType":"vector","result":[
				{"metric":{"__name__":"up","job":"temporal"},"value":[1898596800,"0"]},
				{"metric":{"__name__":"up","job":"api"},"value":[1898596800,"1"]}]}`)
		case "scalar(1)":
			ok(`{"resultType":"scalar","result":[1898596800,"1"]}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error: unexpected end of input"}`)
		}
	case "/api/v1/query_range":
		var vals []string
		for i := 0; i <= 60; i++ {
			vals = append(vals, fmt.Sprintf(`[%d,"%d"]`, now.Add(-time.Hour).Unix()+int64(i*60), i*1000000))
		}
		ok(`{"resultType":"matrix","result":[{"metric":{"__name__":"mem","pod":"api-1"},"values":[` + strings.Join(vals, ",") + `]}]}`)
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T) (*Sources, *fakeProm) {
	t.Helper()
	f := &fakeProm{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s := New(map[string]string{"plateforme": srv.URL, "morte": "http://127.0.0.1:1"}, nil)
	s.now = func() time.Time { return now }
	return s, f
}

func run(t *testing.T, s *Sources, name, args string) (string, bool) {
	t.Helper()
	r := s.Run(context.Background(), name, json.RawMessage(args))
	return r.Content, r.IsError
}

func TestSourcesAlertsTargets(t *testing.T) {
	s, _ := setup(t)
	out, isErr := run(t, s, "prom_sources", `{}`)
	if isErr || !strings.Contains(out, "plateforme\t1/2\t1") || !strings.Contains(out, "morte\tinjoignable") {
		t.Fatalf("sources :\n%s", out)
	}
	out, _ = run(t, s, "prom_alerts", `{"source":"plateforme"}`)
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[1], "firing\tTemporalDown\tcritical\t30m\tjob=\"temporal\"\tTemporal ne répond plus") {
		t.Fatalf("alertes :\n%s", out)
	}
	out, _ = run(t, s, "prom_targets", `{"source":"plateforme"}`)
	if !strings.Contains(out, "down\ttemporal\ttemporal:9090\t15s\tconnection refused") || strings.Contains(out, "api:8080") {
		t.Fatalf("cibles :\n%s", out)
	}
	out, _ = run(t, s, "prom_targets", `{"source":"plateforme","all":true}`)
	if !strings.Contains(out, "api:8080") {
		t.Fatalf("toutes les cibles :\n%s", out)
	}
	if out, isErr := run(t, s, "prom_alerts", `{"source":"inconnue"}`); !isErr || !strings.Contains(out, "morte, plateforme") {
		t.Fatalf("source inconnue : %s", out)
	}
}

func TestQueries(t *testing.T) {
	s, f := setup(t)
	out, isErr := run(t, s, "prom_query", `{"source":"plateforme","query":"up"}`)
	if isErr || !strings.Contains(out, "1\tup{job=\"api\"}\n0\tup{job=\"temporal\"}") {
		t.Fatalf("instantanée :\n%s", out)
	}
	if form := f.forms[len(f.forms)-1]; form["method"] != "POST" || form["timeout"] != "15s" {
		t.Fatalf("requête : %v", form)
	}
	if out, _ := run(t, s, "prom_query", `{"source":"plateforme","query":"scalar(1)"}`); out != "1" {
		t.Fatalf("scalaire : %q", out)
	}
	// L'erreur de Prometheus revient au modèle, qui corrige sa requête.
	if out, isErr := run(t, s, "prom_query", `{"source":"plateforme","query":"sum("}`); !isErr || !strings.Contains(out, "bad_data") {
		t.Fatalf("erreur : %s", out)
	}
	if _, isErr := run(t, s, "prom_query", `{"source":"plateforme","query":"  "}`); !isErr {
		t.Fatal("requête vide acceptée")
	}

	out, isErr = run(t, s, "prom_query_range", `{"source":"plateforme","query":"mem","since":"1h"}`)
	if isErr || !strings.Contains(out, "min 0  max 60000000  dernière 60000000 (il y a 0s)  points 61") || !strings.Contains(out, "-1h=0") {
		t.Fatalf("plage :\n%s", out)
	}
	form := f.forms[len(f.forms)-1]
	if form["step"] != "15" || form["end"] != fmt.Sprint(now.Unix()) || form["start"] != fmt.Sprint(now.Add(-time.Hour).Unix()) {
		t.Fatalf("plage : %v", form)
	}
	run(t, s, "prom_query_range", `{"source":"plateforme","query":"mem","since":"7d"}`)
	if form := f.forms[len(f.forms)-1]; form["step"] != "2016" {
		t.Fatalf("pas sur 7 jours : %v", form["step"])
	}
	for _, bad := range []string{"8d", "-1h", "demain"} {
		if _, isErr := run(t, s, "prom_query_range", `{"source":"plateforme","query":"mem","since":"`+bad+`"}`); !isErr {
			t.Fatalf("plage %s acceptée", bad)
		}
	}
}

func TestParse(t *testing.T) {
	s, err := Parse(" plateforme=http://127.0.0.1:19090/ , prod-1=https://prom.interne:9090")
	if err != nil || strings.Join(s.Names(), ",") != "plateforme,prod-1" || s.urls["plateforme"] != "http://127.0.0.1:19090" {
		t.Fatal(s, err)
	}
	if s, err := Parse(""); err != nil || len(s.Names()) != 0 {
		t.Fatal(err)
	}
	for _, bad := range []string{"plateforme", "Pla=http://x", "a=ftp://x", "a=http://u:p@x", "a=http://x,a=http://y", "a=http://x?y=1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
}
