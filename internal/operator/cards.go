package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/agentkit"
	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
	"github.com/thekrauss/kubemanager/internal/operator/action"
	"github.com/thekrauss/kubemanager/internal/operator/fanout"
	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// Cartes (ADR 0016 §6) : l'agent propose une action du catalogue avec sa
// justification ; une personne qui en a le droit la valide ou la refuse ;
// l'Opérateur l'exécute alors et inscrit l'issue dans le dossier (l'agent la
// relit au tour suivant) et dans le journal (recopié dans l'audit).

// Cards : les cartes vues par les routes.
type Cards interface {
	ProposeCard(ctx context.Context, c store.Card) (store.Card, bool, error)
	DecideCard(ctx context.Context, investigation, id uuid.UUID, approve bool, by, note string) (store.Card, error)
	SettleCard(ctx context.Context, investigation, id uuid.UUID, to store.CardStatus, by, note, result string) (store.Card, error)
	FinishCard(ctx context.Context, id uuid.UUID, ok bool, result string) error
	Cards(ctx context.Context, investigation uuid.UUID) ([]store.Card, error)
	ReapInterrupted(ctx context.Context, after time.Duration, note string) ([]store.Card, error)
}

// ExecTimeout : durée maximale d'une exécution (un redémarrage de service
// peut prendre deux minutes).
const ExecTimeout = 3 * time.Minute

// proposePrefix : outils de proposition vus par le modèle.
const proposePrefix = "propose_"

// ToolName : nom de l'outil de proposition d'une action.
func ToolName(kind string) string { return proposePrefix + strings.ReplaceAll(kind, ".", "_") }

// CardView : une carte telle que la console la montre.
type CardView struct {
	ID           uuid.UUID       `json:"id"`
	Kind         string          `json:"kind"`
	Params       json.RawMessage `json:"params"`
	Target       string          `json:"target"`
	Summary      string          `json:"summary"`
	State        string          `json:"state"`
	Rationale    string          `json:"rationale"`
	Risk         string          `json:"risk"`
	Confirm      string          `json:"confirm,omitempty"`
	Status       string          `json:"status"`
	ProposedAt   time.Time       `json:"proposed_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	DecisionNote string          `json:"decision_note,omitempty"`
	Result       string          `json:"result,omitempty"`
	ExecutedAt   *time.Time      `json:"executed_at,omitempty"`
	// Executor : « operator » (validation ici, exécution par l'Opérateur) ou
	// « api » (validation auprès de l'API : POST /admin/operator/cards/{api_card}/approve
	// avec la session de la personne ; l'Opérateur relit l'issue).
	Executor string     `json:"executor"`
	APICard  *uuid.UUID `json:"api_card,omitempty"`
	// Details : carte de l'API : la requête figée par l'API, champ par champ
	// (ex. « body » de reply_to_customer : le texte exact envoyé au client),
	// à montrer tel quel avant la validation.
	Details map[string]any `json:"details,omitempty"`
}

func cardView(c store.Card) CardView {
	v := cardViewBase(c)
	v.Executor = "operator"
	if IsAPIKind(c.Kind) {
		v.Executor = "api"
		var ref apiCardRef
		if json.Unmarshal(c.Params, &ref) == nil && ref.Card != uuid.Nil {
			v.APICard = &ref.Card
		}
		// L'état d'une carte de l'API est le détail de la requête (JSON).
		var d map[string]any
		if c.State != "" && json.Unmarshal([]byte(c.State), &d) == nil {
			v.Details, v.State = d, ""
		}
	}
	return v
}

func cardViewBase(c store.Card) CardView {
	return CardView{ID: c.ID, Kind: c.Kind, Params: c.Params, Target: c.Target, Summary: c.Summary, State: c.State,
		Rationale: c.Rationale, Risk: c.Risk, Confirm: c.Confirm, Status: string(c.Status), ProposedAt: c.ProposedAt,
		ExpiresAt: c.ExpiresAt, DecidedBy: c.DecidedBy, DecidedAt: c.DecidedAt, DecisionNote: c.DecisionNote,
		Result: c.Result, ExecutedAt: c.ExecutedAt}
}

// allowed : la personne peut valider (donc se voir proposer) l'action.
func allowed(id Identity, c action.Capability) bool {
	switch c {
	case action.CapInfrastructure:
		return id.Capabilities.Infrastructure
	case action.CapAPI:
		// L'API vérifie la permission de la route à la validation.
		return !id.Rescue && id.Token != ""
	}
	return false
}

// proposer : les outils propose_* d'un tour.
type proposer struct {
	reg    *action.Registry
	cards  Cards
	inv    uuid.UUID
	kinds  map[string]string // outil → action
	defs   []llm.Tool
	notify func(CardView)
	note   func(kind string, detail map[string]any)
}

func (s *Server) proposerFor(ctx context.Context, id Identity, inv uuid.UUID, reg *action.Registry, notify func(CardView)) agentkit.ToolRunner {
	if reg == nil || s.Cards == nil {
		return nil
	}
	p := &proposer{reg: reg, cards: s.Cards, inv: inv, kinds: map[string]string{}, notify: notify,
		note: func(kind string, d map[string]any) { s.note(ctx, id, kind, &inv, d) }}
	for _, spec := range reg.Specs() {
		if !allowed(id, spec.Capability) {
			continue
		}
		params, err := withRationale(spec.Parameters)
		if err != nil {
			s.Logger.Error("operator action schema", "kind", spec.Kind, "error", err)
			continue
		}
		name := ToolName(spec.Kind)
		p.kinds[name] = spec.Kind
		p.defs = append(p.defs, llm.Tool{Name: name, Parameters: params,
			Description: "PROPOSE (n'exécute pas) : " + spec.Description +
				" Crée une carte que l'équipe valide ou refuse ; l'issue apparaît au tour suivant."})
	}
	if len(p.defs) == 0 {
		return nil
	}
	return p
}

// withRationale : ajoute au schéma la justification obligatoire.
func withRationale(raw json.RawMessage) (json.RawMessage, error) {
	var sch map[string]any
	if err := json.Unmarshal(raw, &sch); err != nil {
		return nil, err
	}
	props, _ := sch["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["rationale"] = map[string]any{"type": "string", "minLength": 20, "maxLength": 1500,
		"description": "Pourquoi cette action : la cause établie, les preuves (outils lus) et l'effet attendu."}
	sch["properties"] = props
	req, _ := sch["required"].([]any)
	sch["required"] = append(req, "rationale")
	return json.Marshal(sch)
}

func (p *proposer) Definitions() []llm.Tool { return p.defs }

func (p *proposer) Run(ctx context.Context, name string, args json.RawMessage) agentkit.ToolResult {
	kind, ok := p.kinds[name]
	if !ok {
		return agentkit.ToolResult{Content: "outil inconnu : " + name, IsError: true}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return agentkit.ToolResult{Content: "arguments invalides : " + err.Error(), IsError: true}
	}
	var rationale string
	_ = json.Unmarshal(fields["rationale"], &rationale)
	rationale = strings.TrimSpace(rationale)
	if n := utf8.RuneCountInString(rationale); n < 20 || n > 1500 {
		return agentkit.ToolResult{Content: "rationale : de 20 à 1500 caractères : la cause, les preuves, l'effet attendu", IsError: true}
	}
	delete(fields, "rationale")
	params, _ := json.Marshal(fields)

	prep, err := p.reg.Prepare(ctx, kind, params)
	if err != nil {
		return agentkit.ToolResult{Content: "proposition refusée : " + err.Error(), IsError: true}
	}
	c, dup, err := p.cards.ProposeCard(ctx, store.Card{InvestigationID: p.inv, Kind: kind, Params: prep.Params,
		Target: prep.Target, Summary: prep.Summary, State: prep.State, Rationale: rationale, Risk: string(prep.Risk), Confirm: prep.Confirm})
	switch {
	case errors.Is(err, store.ErrTooManyCards):
		return agentkit.ToolResult{Content: fmt.Sprintf("déjà %d cartes en attente dans ce dossier : attendre la décision de l'équipe", store.MaxOpenCards), IsError: true}
	case err != nil:
		return agentkit.ToolResult{Content: "carte non enregistrée : " + err.Error(), IsError: true}
	}
	if !dup {
		if p.note != nil {
			p.note("card.proposed", cardDetail(c))
		}
		if p.notify != nil {
			p.notify(cardView(c))
		}
	}
	msg := fmt.Sprintf("Carte %s proposée (risque %s) : %s État observé : %s.\nEn attente de validation humaine : l'action N'EST PAS faite. "+
		"Dis-le à la personne, sans affirmer de résultat ; l'issue apparaîtra au tour suivant.", c.ID, c.Risk, c.Summary, dashIfEmpty(c.State))
	if dup {
		msg = fmt.Sprintf("Carte %s identique déjà en attente : rien de nouveau n'est proposé.", c.ID)
	}
	return agentkit.ToolResult{Content: msg}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Routes.

type decideInput struct {
	Confirm string `json:"confirm"`
	Note    string `json:"note"`
}

func (s *Server) loadCard(w http.ResponseWriter, r *http.Request, id Identity) (store.Investigation, store.Card, action.Spec, bool) {
	if s.Cards == nil {
		writeError(w, http.StatusServiceUnavailable, "ACTIONS_UNAVAILABLE", "Aucune action n'est configurée sur cet Opérateur.")
		return store.Investigation{}, store.Card{}, action.Spec{}, false
	}
	inv, _, ok := s.load(w, r)
	if !ok {
		return store.Investigation{}, store.Card{}, action.Spec{}, false
	}
	cardID, err := uuid.Parse(r.PathValue("card"))
	if err != nil {
		writeError(w, http.StatusNotFound, "CARD_NOT_FOUND", "Carte introuvable.")
		return store.Investigation{}, store.Card{}, action.Spec{}, false
	}
	cards, err := s.Cards.Cards(r.Context(), inv.ID)
	if err != nil {
		s.internal(w, "cards", err)
		return store.Investigation{}, store.Card{}, action.Spec{}, false
	}
	for _, c := range cards {
		if c.ID != cardID {
			continue
		}
		spec, known := s.spec(c.Kind)
		if !known || !allowed(id, spec.Capability) {
			writeError(w, http.StatusForbidden, "NOT_ALLOWED", "Vous n'avez pas le droit de décider de cette action.")
			return store.Investigation{}, store.Card{}, action.Spec{}, false
		}
		if inv.Status != store.StatusOpen {
			writeError(w, http.StatusConflict, "CLOSED", "Ce dossier est clos.")
			return store.Investigation{}, store.Card{}, action.Spec{}, false
		}
		if c.Status != store.CardProposed {
			writeError(w, http.StatusConflict, "CARD_DECIDED", "Cette carte n'est plus en attente ("+string(c.Status)+").")
			return store.Investigation{}, store.Card{}, action.Spec{}, false
		}
		return inv, c, spec, true
	}
	writeError(w, http.StatusNotFound, "CARD_NOT_FOUND", "Carte introuvable.")
	return store.Investigation{}, store.Card{}, action.Spec{}, false
}

func (s *Server) decided(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrCardDecided):
		writeError(w, http.StatusConflict, "CARD_DECIDED", "Cette carte vient d'être décidée ou a expiré.")
	case errors.Is(err, store.ErrCardNotFound):
		writeError(w, http.StatusNotFound, "CARD_NOT_FOUND", "Carte introuvable.")
	case err != nil:
		s.internal(w, "card decide", err)
	default:
		return true
	}
	return false
}

// approve : valide puis exécute. Avant d'agir, l'action est revérifiée sur
// l'état réel : si le risque a monté ou la cible changé, l'action n'est pas
// jouée (une nouvelle proposition est nécessaire).
func (s *Server) approve(w http.ResponseWriter, r *http.Request, id Identity) {
	var in decideInput
	if !decodeBody(w, r, &in) {
		return
	}
	inv, c, spec, ok := s.loadCard(w, r, id)
	if !ok {
		return
	}
	if spec.External {
		s.approveRemote(w, r, id, inv, c)
		return
	}
	if c.Confirm != "" && strings.TrimSpace(in.Confirm) != c.Confirm {
		writeError(w, http.StatusBadRequest, "CONFIRM_MISMATCH", "Retapez le nom de la cible pour valider : "+c.Confirm)
		return
	}
	c, err := s.Cards.DecideCard(r.Context(), inv.ID, c.ID, true, id.Email, strings.TrimSpace(in.Note))
	if !s.decided(w, err) {
		return
	}
	s.note(r.Context(), id, "card.approved", &inv.ID, cardDetail(c))

	// Contexte détaché : une page fermée (ou l'arrêt du service) n'interrompt
	// pas une action lancée ; l'arrêt l'attend (Drain).
	s.running.Add(1)
	defer s.running.Done()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ExecTimeout)
	defer cancel()
	result, execErr := s.execute(ctx, c, id)
	okRun := execErr == nil
	if !okRun {
		result = execErr.Error()
	}
	if err := s.Cards.FinishCard(ctx, c.ID, okRun, result); err != nil {
		s.Logger.Error("operator card not finished", "card", c.ID, "error", err)
	}
	c.Result = result
	c.Status = store.CardExecuted
	kind := "card.executed"
	if !okRun {
		c.Status, kind = store.CardFailed, "card.execution_error"
	}
	now := time.Now().UTC()
	c.ExecutedAt = &now
	d := cardDetail(c)
	d["result"] = clipText(result, 500)
	s.note(ctx, id, kind, &inv.ID, d)
	s.outcome(ctx, inv.ID, c, fmt.Sprintf("Carte %s (%s, %s) validée par %s : %s. %s", c.ID, c.Kind, c.Target, id.Email,
		map[bool]string{true: "exécutée", false: "ÉCHEC"}[okRun], result), !okRun)
	writeJSON(w, http.StatusOK, map[string]any{"card": cardView(c)})
}

func (s *Server) execute(ctx context.Context, c store.Card, id Identity) (string, error) {
	prep, err := s.Actions.Prepare(ctx, c.Kind, c.Params)
	if err != nil {
		return "", fmt.Errorf("action non jouée : l'état a changé depuis la proposition : %w", err)
	}
	if !bytes.Equal(compact(prep.Params), compact(c.Params)) || prep.Target != c.Target {
		return "", errors.New("action non jouée : la cible a changé depuis la proposition")
	}
	if action.Risk(prep.Risk).Rank() > action.Risk(c.Risk).Rank() {
		return "", fmt.Errorf("action non jouée : le risque est passé de %s à %s (%s) ; une nouvelle proposition est nécessaire", c.Risk, prep.Risk, prep.Summary)
	}
	return s.Actions.Execute(ctx, c.Kind, c.Params, action.Approval{CardID: c.ID.String(), InvestigationID: c.InvestigationID.String(), ApprovedBy: id.Email})
}

func compact(b []byte) []byte {
	var out bytes.Buffer
	if json.Compact(&out, b) != nil {
		return b
	}
	return out.Bytes()
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, id Identity) {
	var in decideInput
	if !decodeBody(w, r, &in) {
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > 500 {
		writeError(w, http.StatusBadRequest, "INVALID_NOTE", "La raison du refus tient en 500 caractères.")
		return
	}
	inv, c, spec, ok := s.loadCard(w, r, id)
	if !ok {
		return
	}
	if spec.External {
		remote := s.remote(id)
		if remote == nil {
			writeError(w, http.StatusServiceUnavailable, "API_UNAVAILABLE", "L'API ne répond pas : la carte ne peut pas être refusée maintenant.")
			return
		}
		if err := remote.Reject(r.Context(), c.Kind, c.Params, in.Note); err != nil {
			// Déjà décidée auprès de l'API : l'issue est relue.
			if s.syncCard(r.Context(), id, inv.ID, c) {
				writeError(w, http.StatusConflict, "CARD_DECIDED", "Cette carte vient d'être décidée dans la console.")
				return
			}
			s.Logger.Warn("operator api card reject", "card", c.ID, "error", err)
			writeError(w, http.StatusBadGateway, "API_REFUSED", "L'API n'a pas enregistré le refus.")
			return
		}
	}
	c, err := s.Cards.DecideCard(r.Context(), inv.ID, c.ID, false, id.Email, in.Note)
	if !s.decided(w, err) {
		return
	}
	d := cardDetail(c)
	d["note"] = in.Note
	s.note(r.Context(), id, "card.rejected", &inv.ID, d)
	text := fmt.Sprintf("Carte %s (%s, %s) refusée par %s.", c.ID, c.Kind, c.Target, id.Email)
	if in.Note != "" {
		text += " Raison : " + in.Note
	}
	s.outcome(r.Context(), inv.ID, c, text, false)
	writeJSON(w, http.StatusOK, map[string]any{"card": cardView(c)})
}

// outcome : l'issue entre dans le dossier (l'agent la relit).
func (s *Server) outcome(ctx context.Context, inv uuid.UUID, c store.Card, text string, failed bool) {
	input, _ := json.Marshal(map[string]any{"card": c.ID, "params": c.Params})
	if _, err := s.Store.AddStep(context.WithoutCancel(ctx), inv, store.Step{Kind: store.StepAction, Tool: c.Kind, Input: input,
		Digest: Digest(text), IsError: failed}); err != nil {
		s.Logger.Error("operator card outcome not recorded", "card", c.ID, "error", err)
	}
}

func cardDetail(c store.Card) map[string]any {
	return map[string]any{"card": c.ID.String(), "action": c.Kind, "target": c.Target, "risk": c.Risk}
}

func clipText(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// spec : une action du catalogue fixe, ou une action de l'API (catalogue
// propre à chaque personne, décidée par l'API).
func (s *Server) spec(kind string) (action.Spec, bool) {
	if IsAPIKind(kind) {
		return action.Spec{Kind: kind, Capability: action.CapAPI, External: true}, true
	}
	return s.Actions.Spec(kind)
}

// RemoteSource : cartes validées hors de l'Opérateur (l'API).
type RemoteSource interface {
	APICards(id Identity) action.Remote
}

func (s *Server) remote(id Identity) action.Remote {
	src, ok := s.API.(RemoteSource)
	if !ok {
		return nil
	}
	return src.APICards(id)
}

// approveRemote : la carte a été validée (ou non) auprès de l'API ; on relit
// l'issue et on l'inscrit dans le dossier.
func (s *Server) approveRemote(w http.ResponseWriter, r *http.Request, id Identity, inv store.Investigation, c store.Card) {
	if s.remote(id) == nil {
		writeError(w, http.StatusServiceUnavailable, "API_UNAVAILABLE", "L'API ne répond pas : l'issue de la carte ne peut pas être relue.")
		return
	}
	if !s.syncCard(r.Context(), id, inv.ID, c) {
		writeError(w, http.StatusConflict, "APPROVE_AT_API", "Cette action se valide auprès de l'API, avec votre session : POST /admin/operator/cards/{api_card}/approve.")
		return
	}
	cards, err := s.Cards.Cards(r.Context(), inv.ID)
	if err != nil {
		s.internal(w, "cards", err)
		return
	}
	for _, x := range cards {
		if x.ID == c.ID {
			writeJSON(w, http.StatusOK, map[string]any{"card": cardView(x)})
			return
		}
	}
	writeError(w, http.StatusNotFound, "CARD_NOT_FOUND", "Carte introuvable.")
}

// syncCard : relit auprès de l'API l'issue d'une carte proposée ; rend true
// si elle est décidée (et l'inscrit dans le dossier).
func (s *Server) syncCard(ctx context.Context, id Identity, inv uuid.UUID, c store.Card) bool {
	remote := s.remote(id)
	// Expirée ici mais peut-être validée à temps auprès de l'API : relue.
	if remote == nil || (c.Status != store.CardProposed && c.Status != store.CardExpired) {
		return false
	}
	out, err := remote.Outcome(ctx, c.Kind, c.Params)
	if err != nil {
		s.Logger.Warn("operator api card outcome", "card", c.ID, "error", err)
		return false
	}
	if !out.Done {
		return false
	}
	if out.Expired {
		// Expirée sans décision auprès de l'API : close ici aussi (plus relue).
		if _, err := s.Cards.SettleCard(ctx, inv, c.ID, store.CardExpired, "", "expirée auprès de l'API", ""); err != nil &&
			!errors.Is(err, store.ErrCardDecided) {
			s.Logger.Warn("operator api card expire", "card", c.ID, "error", err)
		}
		return false
	}
	by := out.DecidedBy
	if by == "" {
		by = "api"
	}
	if out.Rejected {
		dc, err := s.Cards.SettleCard(ctx, inv, c.ID, store.CardRejected, by, out.Result, "")
		if err != nil {
			return errors.Is(err, store.ErrCardDecided)
		}
		d := cardDetail(dc)
		d["note"], d["decided_by"] = out.Result, by
		s.note(ctx, id, "card.rejected", &inv, d)
		text := fmt.Sprintf("Carte %s (%s, %s) refusée par %s.", dc.ID, dc.Kind, dc.Target, by)
		if out.Result != "" {
			text += " Raison : " + out.Result
		}
		s.outcome(ctx, inv, dc, text, false)
		return true
	}
	to, kind, word := store.CardExecuted, "card.executed", "exécutée"
	if !out.OK {
		to, kind, word = store.CardFailed, "card.execution_error", "ÉCHEC"
	}
	dc, err := s.Cards.SettleCard(ctx, inv, c.ID, to, by, "validée auprès de l'API", out.Result)
	if err != nil {
		return errors.Is(err, store.ErrCardDecided)
	}
	d := cardDetail(dc)
	d["result"], d["decided_by"] = clipText(out.Result, 500), by
	s.note(ctx, id, kind, &inv, d)
	s.outcome(ctx, inv, dc, fmt.Sprintf("Carte %s (%s, %s) validée par %s auprès de l'API : %s. %s", dc.ID, dc.Kind, dc.Target, by, word, out.Result), !out.OK)
	return true
}

// syncCards : relit les cartes de l'API encore en attente d'un dossier
// (une décision prise dans la console, même par un collègue). Les cartes
// sont relues en parallèle, chacune avec son délai ; deux lectures
// simultanées du même dossier n'en font qu'une.
func (s *Server) syncCards(ctx context.Context, id Identity, inv uuid.UUID) {
	if s.Cards == nil || s.remote(id) == nil {
		return
	}
	_, _, _ = s.syncing.Do(inv.String(), func() (any, error) {
		cards, err := s.Cards.Cards(ctx, inv)
		if err != nil {
			return nil, err
		}
		var open []store.Card
		for _, c := range cards {
			if (c.Status == store.CardProposed || c.Status == store.CardExpired) && IsAPIKind(c.Kind) {
				open = append(open, c)
			}
		}
		fanout.Each(context.WithoutCancel(ctx), open, syncTimeout, func(ctx context.Context, c store.Card) bool {
			return s.syncCard(ctx, id, inv, c)
		})
		return nil, nil
	})
}

// syncTimeout : relecture d'une carte auprès de l'API.
const syncTimeout = 10 * time.Second

// interruptedNote : issue d'une carte validée dont l'exécution n'a pas rendu
// de résultat (processus arrêté pendant l'action).
const interruptedNote = "interrompue : l'Opérateur s'est arrêté pendant l'action ; état réel à vérifier"

// ReapCards : les cartes validées restées sans issue au-delà du délai
// d'exécution passent en échec (journal et dossier), pour que personne ne
// les croie encore en cours.
func (s *Server) ReapCards(ctx context.Context) {
	if s.Cards == nil {
		return
	}
	cards, err := s.Cards.ReapInterrupted(ctx, ExecTimeout+time.Minute, interruptedNote)
	if err != nil {
		s.Logger.Error("operator card reaper", "error", err)
		return
	}
	for _, c := range cards {
		d := cardDetail(c)
		d["result"], d["approved_by"] = interruptedNote, c.DecidedBy
		if err := s.Store.Journal(context.WithoutCancel(ctx), "operator", "card.interrupted", &c.InvestigationID, d); err != nil {
			s.Logger.Error("operator journal", "kind", "card.interrupted", "error", err)
		}
		s.outcome(ctx, c.InvestigationID, c, fmt.Sprintf("Carte %s (%s, %s) validée par %s : %s.", c.ID, c.Kind, c.Target, c.DecidedBy, interruptedNote), true)
		s.Logger.Warn("operator card interrupted", "card", c.ID, "kind", c.Kind)
	}
}

// RunReaper : ReapCards au démarrage puis chaque minute, jusqu'à l'arrêt.
func (s *Server) RunReaper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.ReapCards(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
