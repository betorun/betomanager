package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Invoker : de quoi rejouer une route au nom d'une personne.
type Invoker struct {
	// Handler : le moteur HTTP de l'API (gin), appelé en interne.
	Handler http.Handler
	// Token : jeton délégué de la personne (sts.MethodDelegated).
	Token string
	// ClientIP : adresse de la personne (rate limit et audit à son nom).
	ClientIP string
	// AcceptLanguage : langue de la personne (messages d'erreur).
	AcceptLanguage string
}

// Result : réponse d'un outil, prête pour le modèle.
type Result struct {
	Status    int
	Content   string
	IsError   bool
	Truncated bool
}

const (
	// maxCaptured : au-delà, la réponse d'une route est coupée (mémoire).
	maxCaptured = 256 << 10
	// MaxContent : ce que le modèle reçoit d'un outil, au plus.
	MaxContent = 12 << 10
)

// KeepTail : pour ces outils, on garde la FIN de la réponse (journaux).
func (t *Tool) keepTail() bool { return strings.HasSuffix(t.Path, "/logs") }

// Call : exécute l'outil name avec les arguments du modèle.
func (c *Catalog) Call(ctx context.Context, inv Invoker, name string, args json.RawMessage) Result {
	t, ok := c.tools[name]
	if !ok {
		return errorResult(0, fmt.Sprintf("unknown tool %q", name))
	}
	in := map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return errorResult(0, "arguments must be a JSON object")
		}
	}
	if t.run != nil {
		return t.run(ctx, c, inv, in)
	}
	return c.callRoute(ctx, inv, t, in)
}

func (c *Catalog) callRoute(ctx context.Context, inv Invoker, t *Tool, in map[string]any) Result {
	path := t.Path
	known := map[string]bool{}
	for _, p := range t.pathParams {
		known[p] = true
		v, ok := in[p]
		s := scalar(v)
		if !ok || s == "" || strings.ContainsAny(s, "/?#") || s == "." || s == ".." {
			return errorResult(0, fmt.Sprintf("argument %q is required (an identifier)", p))
		}
		path = strings.Replace(path, ":"+p, url.PathEscape(s), 1)
	}
	q := url.Values{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if known[k] {
			continue
		}
		if !t.query[k] {
			return errorResult(0, fmt.Sprintf("unknown argument %q", k))
		}
		s := scalar(in[k])
		if max, ok := t.Max[k]; ok {
			if n, err := strconv.Atoi(s); err == nil && n > max {
				s = strconv.Itoa(max)
			}
		}
		if s != "" {
			q.Set(k, s)
		}
	}
	for k, v := range t.Fixed {
		q.Set(k, v)
	}
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return invoke(ctx, inv, path, t.keepTail())
}

// invoke : GET interne, réponse capturée et bornée.
func invoke(ctx context.Context, inv Invoker, path string, keepTail bool) Result {
	if inv.Handler == nil {
		return errorResult(0, "tools unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return errorResult(0, "invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+inv.Token)
	req.Header.Set("Accept", "application/json")
	if inv.AcceptLanguage != "" {
		req.Header.Set("Accept-Language", inv.AcceptLanguage)
	}
	req.RemoteAddr = inv.ClientIP + ":0"
	w := newCapture(keepTail)
	inv.Handler.ServeHTTP(w, req)

	body := w.text()
	if w.status/100 != 2 {
		return Result{Status: w.status, Content: errorContent(w.status, body), IsError: true}
	}
	content, truncated := clip(compactJSON(body), keepTail)
	return Result{Status: w.status, Content: content, Truncated: truncated || w.truncated}
}

func errorResult(status int, msg string) Result {
	return Result{Status: status, Content: errorContent(status, `{"message":`+strconv.Quote(msg)+`}`), IsError: true}
}

// errorContent : statut et message de l'API, sans le reste du corps.
func errorContent(status int, body string) string {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	msg := e.Message
	if msg == "" {
		msg = e.Error
	}
	out, _ := json.Marshal(map[string]any{"status": status, "code": e.Code, "message": msg})
	return string(out)
}

func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return "" // objets et tableaux : refusés comme valeur vide
	}
}

func compactJSON(s string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(s)) == nil {
		return buf.String()
	}
	return s
}

// clip : au plus MaxContent octets, en gardant le début (ou la fin pour les
// journaux), sans couper un caractère.
func clip(s string, keepTail bool) (string, bool) {
	if len(s) <= MaxContent {
		return s, false
	}
	if keepTail {
		s = s[len(s)-MaxContent:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < 400 {
			s = s[i+1:] // repartir d'une ligne entière
		}
		return "[…début coupé…]\n" + s, true
	}
	s = s[:MaxContent]
	for len(s) > 0 && !utf8.RuneStart(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s + "\n[…suite coupée…]", true
}

// capture : http.ResponseWriter en mémoire, borné à maxCaptured octets
// (le début, ou la fin si keepTail).
type capture struct {
	header    http.Header
	status    int
	buf       []byte
	keepTail  bool
	truncated bool
}

func newCapture(keepTail bool) *capture {
	return &capture{header: http.Header{}, keepTail: keepTail}
}

func (w *capture) Header() http.Header { return w.header }

func (w *capture) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *capture) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.buf = append(w.buf, b...)
	if len(w.buf) > maxCaptured {
		w.truncated = true
		if w.keepTail {
			w.buf = append([]byte(nil), w.buf[len(w.buf)-maxCaptured:]...)
		} else {
			w.buf = w.buf[:maxCaptured]
		}
	}
	return len(b), nil
}

// Flush : les routes en flux appellent Flush ; rien à faire en mémoire.
func (w *capture) Flush() {}

func (w *capture) text() string {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return string(w.buf)
}
