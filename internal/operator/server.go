package operator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/operator/action"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// Store : la base vue par les routes.
type Store interface {
	Dossiers
	// Get : un dossier et toutes ses étapes (la console).
	Get(ctx context.Context, id uuid.UUID) (store.Investigation, []store.Step, error)
	Ping(ctx context.Context) error
	Create(ctx context.Context, in store.Investigation) (store.Investigation, error)
	List(ctx context.Context, limit int) ([]store.Investigation, error)
	Conclude(ctx context.Context, id uuid.UUID, status store.Status, conclusion string, in, out int64) error
	Journal(ctx context.Context, actor, kind string, investigation *uuid.UUID, detail map[string]any) error
	JournalEntries(ctx context.Context, limit int) ([]store.Entry, error)
}

// RescueAuth : accès de secours (rescue.Service) ; nil : désactivé.
type RescueAuth interface {
	Login(ctx context.Context, email, password, code, reason, remote string) (string, time.Time, store.RescueAccount, error)
	Session(ctx context.Context, token string) (store.RescueAccount, error)
	Logout(ctx context.Context, token, email string) error
}

// Server : l'API HTTP de l'Opérateur, jointe par tunnel SSH depuis le
// navigateur de l'équipe (jeton délégué de la personne).
type Server struct {
	Store Store
	Auth  Authenticator
	// Rescue : comptes locaux de l'équipe (jetons rsc_…), nil : désactivé.
	Rescue RescueAuth
	// Investigator : nil sans modèle (la console reste utilisable à la main).
	Investigator *Investigator
	// Infrastructure : outils du chemin de secours, ouverts aux seules
	// personnes qui en ont le droit (Capabilities.Infrastructure).
	Infrastructure agentkit.ToolRunner
	// API : outils du chemin normal (routes /admin), nil sans API.
	API ToolSource
	// Actions : catalogue des actions proposables (cartes) ; nil : aucune.
	Actions *action.Registry
	// Cards : cartes du dossier (la base) ; nil : aucune action.
	Cards Cards
	Model string
	// Origins : origines de la console autorisées (CORS).
	Origins []string
	Logger  *slog.Logger

	busy sync.Map // dossier → tour en cours (un seul à la fois)
	// running : actions validées en cours d'exécution (attendues à l'arrêt).
	running sync.WaitGroup
	// syncing : relecture des cartes de l'API, une à la fois par dossier.
	syncing singleflight.Group
}

// Drain : attend la fin des actions en cours (arrêt du service), au plus
// jusqu'à ctx ; rend false si des actions tournaient encore.
func (s *Server) Drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() { s.running.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

const maxBody = 64 << 10

// Handler : les routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("GET /v1/me", s.authed(s.me))
	mux.Handle("GET /v1/investigations", s.authed(s.list))
	mux.Handle("POST /v1/investigations", s.authed(s.create))
	mux.Handle("GET /v1/investigations/{id}", s.authed(s.get))
	mux.Handle("POST /v1/investigations/{id}/messages", s.authed(s.ask))
	mux.Handle("POST /v1/investigations/{id}/close", s.authed(s.close))
	mux.Handle("POST /v1/investigations/{id}/cards/{card}/approve", s.authed(s.approve))
	mux.Handle("POST /v1/investigations/{id}/cards/{card}/reject", s.authed(s.reject))
	mux.HandleFunc("POST /v1/rescue/login", s.rescueLogin)
	mux.Handle("POST /v1/rescue/logout", s.authed(s.rescueLogout))
	mux.Handle("GET /v1/journal", s.authed(s.journal))
	return s.cors(mux)
}

// cors : la console (autre origine) appelle l'Opérateur avec un jeton dans
// l'en-tête ; aucun cookie, donc pas d'identifiants CORS.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && slices.Contains(s.Origins, o) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authed(h func(http.ResponseWriter, *http.Request, Identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.identify(r.Context(), bearer(r))
		switch {
		case errors.Is(err, ErrUnauthenticated):
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Jeton absent, expiré ou refusé : reconnectez-vous à la console.")
			return
		case err != nil:
			s.Logger.Warn("operator auth unavailable", "error", err)
			writeError(w, http.StatusServiceUnavailable, "API_UNAVAILABLE", "L'API ne répond pas : utilisez l'accès de secours.")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)), id)
	})
}

// identify : jeton de secours (rsc_…) ou jeton délégué de l'API.
func (s *Server) identify(ctx context.Context, token string) (Identity, error) {
	if strings.HasPrefix(token, rescueTokenPrefix) {
		if s.Rescue == nil {
			return Identity{}, ErrUnauthenticated
		}
		acc, err := s.Rescue.Session(ctx, token)
		if err != nil {
			if isRescueRefusal(err) {
				return Identity{}, ErrUnauthenticated
			}
			return Identity{}, err
		}
		return Identity{UserID: acc.ID, Email: acc.Email, Rescue: true, Token: token,
			Capabilities: Capabilities{Infrastructure: true}}, nil
	}
	return s.Auth.Identify(ctx, token)
}

// health : 200 si la base répond, 503 sinon ; dit si l'agent est disponible.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	body := map[string]any{"database": "ok", "agent": s.Investigator != nil}
	code := http.StatusOK
	if err := s.Store.Ping(ctx); err != nil {
		s.Logger.Warn("operator database unreachable", "error", err)
		body["database"] = "down"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, id Identity) {
	writeJSON(w, http.StatusOK, map[string]any{"user_id": id.UserID, "email": id.Email,
		"capabilities": id.Capabilities, "agent": s.Investigator != nil, "rescue": id.Rescue})
}

// note : entrée du journal du serveur de gestion (une erreur n'arrête pas
// la requête, elle est journalisée).
func (s *Server) note(ctx context.Context, id Identity, kind string, inv *uuid.UUID, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["email"] = id.Email
	if id.Rescue {
		detail["rescue"] = true
	}
	if err := s.Store.Journal(context.WithoutCancel(ctx), id.Actor(), kind, inv, detail); err != nil {
		s.Logger.Error("operator journal", "kind", kind, "error", err)
	}
}

type loginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
	// Reason : pourquoi l'accès de secours (journalisé, envoyé à l'équipe).
	Reason string `json:"reason"`
}

// rescueLogin : connexion de secours (mot de passe et code TOTP).
func (s *Server) rescueLogin(w http.ResponseWriter, r *http.Request) {
	if s.Rescue == nil {
		writeError(w, http.StatusNotFound, "RESCUE_DISABLED", "L'accès de secours n'est pas configuré sur ce serveur.")
		return
	}
	var in loginInput
	if !decodeBody(w, r, &in) {
		return
	}
	token, exp, _, err := s.Rescue.Login(r.Context(), in.Email, in.Password, in.Code, in.Reason, r.RemoteAddr)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": exp})
	case isRescueReason(err):
		writeError(w, http.StatusBadRequest, "REASON_REQUIRED", "Indiquez le motif de l'accès de secours (10 à 300 caractères).")
	case isRescueLocked(err):
		writeError(w, http.StatusTooManyRequests, "LOCKED", "Trop de tentatives : réessayez dans 15 minutes.")
	case isRescueRefusal(err):
		writeError(w, http.StatusUnauthorized, "BAD_CREDENTIALS", "E-mail, mot de passe ou code refusé.")
	default:
		s.internal(w, "rescue login", err)
	}
}

func (s *Server) rescueLogout(w http.ResponseWriter, r *http.Request, id Identity) {
	if !id.Rescue {
		writeError(w, http.StatusBadRequest, "NOT_RESCUE", "Cette session n'est pas une session de secours.")
		return
	}
	if err := s.Rescue.Logout(r.Context(), id.Token, id.Email); err != nil {
		s.internal(w, "rescue logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request, _ Identity) {
	items, err := s.Store.JournalEntries(r.Context(), 200)
	if err != nil {
		s.internal(w, "journal", err)
		return
	}
	views := make([]JournalView, len(items))
	for i, e := range items {
		views[i] = JournalView{UID: e.UID, At: e.At, Actor: e.Actor, Kind: e.Kind, InvestigationID: e.InvestigationID, Detail: e.Detail}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

// JournalView : une entrée du journal telle que la console la montre (sans
// l'identifiant interne).
type JournalView struct {
	UID             uuid.UUID      `json:"uid"`
	At              time.Time      `json:"at"`
	Actor           string         `json:"actor"`
	Kind            string         `json:"kind"`
	InvestigationID *uuid.UUID     `json:"investigation_id"`
	Detail          map[string]any `json:"detail"`
}

// InvestigationView : un dossier tel que la console le montre.
type InvestigationView struct {
	ID           uuid.UUID  `json:"id"`
	SubjectKind  string     `json:"subject_kind"`
	SubjectRef   string     `json:"subject_ref"`
	Reason       string     `json:"reason"`
	ActorEmail   string     `json:"actor_email"`
	Model        string     `json:"model"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	ConcludedAt  *time.Time `json:"concluded_at,omitempty"`
}

// StepView : une étape.
type StepView struct {
	Seq        int             `json:"seq"`
	Kind       string          `json:"kind"`
	Tool       string          `json:"tool,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Digest     string          `json:"digest"`
	Result     *string         `json:"result,omitempty"`
	IsError    bool            `json:"is_error"`
	DurationMs int             `json:"duration_ms"`
	CreatedAt  time.Time       `json:"created_at"`
}

func view(i store.Investigation) InvestigationView {
	return InvestigationView{ID: i.ID, SubjectKind: string(i.SubjectKind), SubjectRef: i.SubjectRef, Reason: i.Reason,
		ActorEmail: i.ActorEmail, Model: i.Model, Status: string(i.Status), Conclusion: i.Conclusion,
		InputTokens: i.InputTokens, OutputTokens: i.OutputTokens, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt, ConcludedAt: i.ConcludedAt}
}

// Dossiers partagés dans l'équipe : toute personne admise les lit.
func (s *Server) list(w http.ResponseWriter, r *http.Request, _ Identity) {
	items, err := s.Store.List(r.Context(), 100)
	if err != nil {
		s.internal(w, "list", err)
		return
	}
	out := make([]InvestigationView, len(items))
	for i, it := range items {
		out[i] = view(it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

var subjects = map[store.SubjectKind]bool{store.SubjectTicket: true, store.SubjectCluster: true,
	store.SubjectOrganization: true, store.SubjectIncident: true, store.SubjectQuestion: true}

type createInput struct {
	SubjectKind store.SubjectKind `json:"subject_kind"`
	SubjectRef  string            `json:"subject_ref"`
	Reason      string            `json:"reason"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, id Identity) {
	var in createInput
	if !decodeBody(w, r, &in) {
		return
	}
	in.SubjectRef, in.Reason = strings.TrimSpace(in.SubjectRef), strings.TrimSpace(in.Reason)
	if !subjects[in.SubjectKind] {
		writeError(w, http.StatusBadRequest, "INVALID_SUBJECT", "Sujet attendu : ticket, cluster, organization, incident ou question.")
		return
	}
	if len(in.SubjectRef) > 200 || len(in.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "TOO_LONG", "Référence (200) ou motif (500 caractères) trop long.")
		return
	}
	inv, err := s.Store.Create(r.Context(), store.Investigation{SubjectKind: in.SubjectKind, SubjectRef: in.SubjectRef,
		Reason: in.Reason, ActorUserID: id.Actor(), ActorEmail: id.Email, Model: s.Model})
	if err != nil {
		s.internal(w, "create", err)
		return
	}
	s.note(r.Context(), id, "investigation.opened", &inv.ID, map[string]any{"subject": string(in.SubjectKind) + " " + in.SubjectRef, "reason": in.Reason})
	writeJSON(w, http.StatusCreated, view(inv))
}

func (s *Server) load(w http.ResponseWriter, r *http.Request) (store.Investigation, []store.Step, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Dossier introuvable.")
		return store.Investigation{}, nil, false
	}
	inv, steps, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Dossier introuvable.")
		return store.Investigation{}, nil, false
	}
	if err != nil {
		s.internal(w, "get", err)
		return store.Investigation{}, nil, false
	}
	return inv, steps, true
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, id Identity) {
	inv, steps, ok := s.load(w, r)
	if !ok {
		return
	}
	// Une carte de l'API décidée dans la console entre dans le dossier.
	if s.Cards != nil && inv.Status == store.StatusOpen {
		s.syncCards(r.Context(), id, inv.ID)
		if _, fresh, err := s.Store.Get(r.Context(), inv.ID); err == nil {
			steps = fresh
		}
	}
	out := make([]StepView, len(steps))
	for i, st := range steps {
		out[i] = StepView{Seq: st.Seq, Kind: string(st.Kind), Tool: st.Tool, Input: st.Input, Digest: st.Digest,
			Result: st.Result, IsError: st.IsError, DurationMs: st.DurationMs, CreatedAt: st.CreatedAt}
	}
	cards := []CardView{}
	if s.Cards != nil {
		list, err := s.Cards.Cards(r.Context(), inv.ID)
		if err != nil {
			s.internal(w, "cards", err)
			return
		}
		for _, c := range list {
			cards = append(cards, cardView(c))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"investigation": view(inv), "steps": out, "cards": cards})
}

// owner : seule la personne qui a ouvert le dossier le fait avancer (le
// motif et la responsabilité sont les siens).
func owner(w http.ResponseWriter, inv store.Investigation, id Identity) bool {
	if inv.ActorUserID != id.Actor() {
		writeError(w, http.StatusForbidden, "NOT_OWNER", "Seule la personne qui a ouvert ce dossier peut le faire avancer.")
		return false
	}
	if inv.Status != store.StatusOpen {
		writeError(w, http.StatusConflict, "CLOSED", "Ce dossier est clos.")
		return false
	}
	return true
}

type askInput struct {
	Message string `json:"message"`
}

// ask : une question ; la réponse arrive en flux (text/event-stream) :
// text, tool_start, tool_end, card (action proposée), puis done ou error.
func (s *Server) ask(w http.ResponseWriter, r *http.Request, id Identity) {
	if s.Investigator == nil {
		writeError(w, http.StatusServiceUnavailable, "AGENT_UNAVAILABLE", "L'agent n'est pas configuré : les outils restent utilisables à la main.")
		return
	}
	var in askInput
	if !decodeBody(w, r, &in) {
		return
	}
	in.Message = strings.TrimSpace(in.Message)
	if in.Message == "" || len([]rune(in.Message)) > 8000 {
		writeError(w, http.StatusBadRequest, "INVALID_MESSAGE", "La question doit contenir de 1 à 8000 caractères.")
		return
	}
	inv, _, ok := s.load(w, r)
	if !ok || !owner(w, inv, id) {
		return
	}
	if _, running := s.busy.LoadOrStore(inv.ID, true); running {
		writeError(w, http.StatusConflict, "BUSY", "Une question est déjà en cours dans ce dossier.")
		return
	}
	defer s.busy.Delete(inv.ID)

	s.note(r.Context(), id, "investigation.question", &inv.ID, nil)
	s.syncCards(r.Context(), id, inv.ID)
	ev := newEvents(w)
	defer ev.heartbeat(heartbeatEvery)()
	tools := s.toolsFor(r.Context(), id, inv.Reason, inv.ID, func(c CardView) { ev.send("card", c) })
	res, err := s.Investigator.Ask(r.Context(), inv.ID, in.Message, tools, ev)
	if err != nil {
		s.Logger.Warn("operator turn failed", "investigation", inv.ID, "error", err)
		ev.send("error", map[string]any{"kind": string(llm.KindOf(err)), "message": "Le modèle n'a pas pu répondre."})
		return
	}
	ev.send("done", map[string]any{"stop": res.Stop, "tool_calls": res.ToolCalls,
		"input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens})
}

// ToolSource : outils du chemin normal pour une personne et un motif.
type ToolSource interface {
	For(ctx context.Context, id Identity, reason string) (agentkit.ToolRunner, error)
}

// toolsFor : outils ouverts à la personne : ceux de l'API (selon ses droits,
// avec le motif du dossier), plus le chemin de secours si elle y a droit.
// Une API injoignable n'empêche pas l'enquête sur l'infrastructure.
// Les actions proposables (cartes) : le catalogue fixe (nœuds, clusters),
// plus les actions de l'API ouvertes à la personne.
func (s *Server) toolsFor(ctx context.Context, id Identity, reason string, inv uuid.UUID, notify func(CardView)) agentkit.ToolRunner {
	var runners []agentkit.ToolRunner
	reg := s.Actions
	// Session de secours : jamais l'API (pas de jeton, pas de données des
	// clients).
	if s.API != nil && !id.Rescue {
		api, err := s.API.For(ctx, id, reason)
		if err != nil {
			s.Logger.Warn("operator API tools unavailable", "error", err)
		} else {
			runners = append(runners, api)
			if src, ok := api.(interface {
				CardActions(uuid.UUID) action.Provider
			}); ok {
				if p := src.CardActions(inv); p != nil {
					if withAPI, err := reg.With(p); err != nil {
						s.Logger.Error("operator api actions", "error", err)
					} else {
						reg = withAPI
					}
				}
			}
		}
	}
	if id.Capabilities.Infrastructure && s.Infrastructure != nil {
		runners = append(runners, s.Infrastructure)
	}
	if p := s.proposerFor(ctx, id, inv, reg, notify); p != nil {
		runners = append(runners, p)
	}
	set, err := Combine(runners...)
	if err != nil {
		s.Logger.Error("operator tools", "error", err)
		return noTools{}
	}
	return set
}

type noTools struct{}

func (noTools) Definitions() []llm.Tool { return nil }
func (noTools) Run(context.Context, string, json.RawMessage) agentkit.ToolResult {
	return agentkit.ToolResult{Content: "aucun outil disponible", IsError: true}
}

type closeInput struct {
	Status     store.Status `json:"status"`
	Conclusion string       `json:"conclusion"`
}

func (s *Server) close(w http.ResponseWriter, r *http.Request, id Identity) {
	var in closeInput
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Status != store.StatusConcluded && in.Status != store.StatusAbandoned {
		writeError(w, http.StatusBadRequest, "INVALID_STATUS", "Statut attendu : concluded ou abandoned.")
		return
	}
	inv, _, ok := s.load(w, r)
	if !ok || !owner(w, inv, id) {
		return
	}
	if _, running := s.busy.Load(inv.ID); running {
		writeError(w, http.StatusConflict, "BUSY", "Une question est en cours dans ce dossier.")
		return
	}
	switch err := s.Store.Conclude(r.Context(), inv.ID, in.Status, strings.TrimSpace(in.Conclusion), 0, 0); {
	case errors.Is(err, store.ErrClosed):
		writeError(w, http.StatusConflict, "CLOSED", "Ce dossier est clos.")
	case err != nil:
		s.internal(w, "close", err)
	default:
		s.note(r.Context(), id, "investigation.closed", &inv.ID, map[string]any{"status": string(in.Status)})
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) internal(w http.ResponseWriter, op string, err error) {
	s.Logger.Error("operator request failed", "op", op, "error", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", "Erreur interne de l'Opérateur.")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Corps de requête invalide.")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"code": kind, "message": msg})
}

// events : flux text/event-stream d'un tour (agentkit.Emitter).
type events struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
	// done : flux fermé (handler rendu) : plus aucune écriture.
	done bool
}

// heartbeatEvery : commentaire SSE envoyé pendant un silence (un outil lent,
// la réflexion du modèle) : les proxys et le navigateur ne coupent pas le
// flux pour inactivité.
var heartbeatEvery = 15 * time.Second

// heartbeat : « : ping » à intervalle, jusqu'à l'appel de la fonction rendue
// (qui ferme aussi le flux aux écritures tardives).
func (e *events) heartbeat(every time.Duration) func() {
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				e.write(": ping\n\n")
			}
		}
	}()
	return func() {
		close(stop)
		<-finished
		e.mu.Lock()
		e.done = true
		e.mu.Unlock()
	}
}

func (e *events) write(raw string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	_, _ = e.w.Write([]byte(raw))
	if e.f != nil {
		e.f.Flush()
	}
}

func newEvents(w http.ResponseWriter) *events {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &events{w: w, f: f}
}

func (e *events) send(kind string, data any) {
	b, _ := json.Marshal(data)
	e.write("event: " + kind + "\ndata: " + string(b) + "\n\n")
}

func (e *events) Text(delta string) { e.send("text", map[string]string{"delta": delta}) }
func (e *events) ToolStart(callID, name string) {
	e.send("tool_start", map[string]string{"id": callID, "name": name})
}
func (e *events) ToolEnd(callID, name string, ok bool) {
	e.send("tool_end", map[string]any{"id": callID, "name": name, "ok": ok})
}
