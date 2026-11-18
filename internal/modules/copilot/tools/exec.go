package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Prepared : une requête figée, prête à être jouée (après validation pour
// une écriture). Sérialisable : elle est gardée le temps de la validation.
type Prepared struct {
	Tool   string          `json:"tool"`
	Method string          `json:"method"`
	Path   string          `json:"path"` // chemin et requête, échappés
	Body   json.RawMessage `json:"body,omitempty"`
	// Args : arguments du modèle, après contrôle (affichage de la carte, inverse).
	Args map[string]any `json:"args"`
}

// ErrWriteNeedsApproval : une écriture ne passe jamais par Call.
const writeNeedsApproval = "this tool is an action: it must be validated by the person"

// Call : exécute l'outil name avec les arguments du modèle. Lectures et
// outils composites seulement : une écriture est refusée ici (voir Prepare).
func (c *Catalog) Call(ctx context.Context, inv Invoker, name string, args json.RawMessage) Result {
	t, ok := c.tools[name]
	if !ok {
		return errorResult(0, fmt.Sprintf("unknown tool %q", name))
	}
	if t.Write {
		return errorResult(0, writeNeedsApproval)
	}
	in, res := decodeArgs(args)
	if res != nil {
		return *res
	}
	if t.run != nil {
		return t.run(ctx, c, inv, in)
	}
	return c.callRoute(ctx, inv, t, in)
}

func decodeArgs(args json.RawMessage) (map[string]any, *Result) {
	in := map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 {
		if err := json.Unmarshal(args, &in); err != nil || in == nil {
			r := errorResult(0, "arguments must be a JSON object")
			return nil, &r
		}
	}
	return in, nil
}

// Prepare : contrôle les arguments d'un outil et fige la requête, sans rien
// exécuter. Le message d'erreur est destiné au modèle (il corrige son appel).
func (c *Catalog) Prepare(name string, args json.RawMessage) (*Prepared, error) {
	t, ok := c.tools[name]
	if !ok || t.run != nil {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	in, res := decodeArgs(args)
	if res != nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return c.prepare(t, in)
}

func (c *Catalog) prepare(t *Tool, in map[string]any) (*Prepared, error) {
	path := t.Path
	known := map[string]bool{}
	for _, p := range t.pathParams {
		known[p] = true
		s := scalar(in[p])
		if s == "" || strings.ContainsAny(s, "/?#") || s == "." || s == ".." {
			return nil, fmt.Errorf("argument %q is required (an identifier)", p)
		}
		path = strings.Replace(path, ":"+p, url.PathEscape(s), 1)
	}
	q := url.Values{}
	body := map[string]any{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case known[k]:
		case t.query[k]:
			s := scalar(in[k])
			if max, ok := t.Max[k]; ok {
				if n, err := strconv.Atoi(s); err == nil && n > max {
					s = strconv.Itoa(max)
				}
			}
			if s != "" {
				q.Set(k, s)
			}
		case t.body[k]:
			body[k] = in[k]
		default:
			return nil, fmt.Errorf("unknown argument %q", k)
		}
	}
	for k, v := range t.Fixed {
		q.Set(k, v)
	}
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	p := &Prepared{Tool: t.Name, Method: t.Method, Path: path, Args: in}
	if t.Write && len(t.body) > 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("invalid arguments")
		}
		p.Body = raw
	}
	return p, nil
}

func (c *Catalog) callRoute(ctx context.Context, inv Invoker, t *Tool, in map[string]any) Result {
	p, err := c.prepare(t, in)
	if err != nil {
		return errorResult(0, err.Error())
	}
	return invoke(ctx, inv, p, t.keepTail(), "")
}

// Execute : joue une écriture préparée et VALIDÉE par la personne (l'appelant
// garantit la validation et fournit un jeton en portée « write »).
// idempotencyKey : la même validation rejouée ne s'exécute qu'une fois.
func (c *Catalog) Execute(ctx context.Context, inv Invoker, p *Prepared, idempotencyKey string) Result {
	t, ok := c.tools[p.Tool]
	if !ok || !t.Write || p.Method != t.Method {
		return errorResult(0, "unknown action")
	}
	return invoke(ctx, inv, p, false, idempotencyKey)
}

// Inverse : requête qui défait p (outil Undo, mêmes paramètres de chemin),
// ou nil s'il n'y en a pas.
func (c *Catalog) Inverse(p *Prepared) *Prepared {
	t, ok := c.tools[p.Tool]
	if !ok || t.Undo == "" {
		return nil
	}
	inv := c.tools[t.Undo]
	args := map[string]any{}
	for _, name := range inv.pathParams {
		args[name] = p.Args[name]
	}
	out, err := c.prepare(inv, args)
	if err != nil {
		return nil
	}
	return out
}

// invoke : requête interne, réponse capturée et bornée.
func invoke(ctx context.Context, inv Invoker, p *Prepared, keepTail bool, idempotencyKey string) Result {
	if inv.Handler == nil {
		return errorResult(0, "tools unavailable")
	}
	var body io.Reader
	if len(p.Body) > 0 {
		body = bytes.NewReader(p.Body)
	}
	req, err := http.NewRequestWithContext(ctx, p.Method, p.Path, body)
	if err != nil {
		return errorResult(0, "invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+inv.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if inv.AcceptLanguage != "" {
		req.Header.Set("Accept-Language", inv.AcceptLanguage)
	}
	req.RemoteAddr = inv.ClientIP + ":0"
	w := newCapture(keepTail)
	inv.Handler.ServeHTTP(w, req)

	text := w.text()
	if w.status/100 != 2 {
		return Result{Status: w.status, Content: errorContent(w.status, text), IsError: true}
	}
	content, truncated := clip(compactJSON(text), keepTail)
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
