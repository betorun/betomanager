package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/operator/action"
)

// Actions de l'API (routes /admin) : l'agent propose, l'API fige la requête
// (POST /admin/operator/cards) ; la personne valide auprès de l'API, avec
// sa session (double authentification pour une carte sensible) ; l'Opérateur
// relit l'issue (GET /admin/operator/cards/{id}). Il n'a jamais de quoi
// écrire dans l'API : son jeton est en lecture.

// apiKindPrefix : préfixe des actions de l'API.
const apiKindPrefix = "api."

// IsAPIKind : action jouée par l'API.
func IsAPIKind(kind string) bool { return strings.HasPrefix(kind, apiKindPrefix) }

type apiCardTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Risk        string          `json:"risk"`
	Strong      bool            `json:"strong"`
}

// apiActions : action.Provider et action.Remote pour une personne.
type apiActions struct {
	api    *APITools
	token  string
	reason string
	inv    *uuid.UUID
	cards  []apiCardTool
}

type apiCardRef struct {
	Card uuid.UUID `json:"api_card"`
}

// apiCardView : réponse de l'API (operatoraccess.CardView).
type apiCardView struct {
	ID         uuid.UUID      `json:"id"`
	Tool       string         `json:"tool"`
	Summary    string         `json:"summary"`
	Target     string         `json:"target"`
	Details    map[string]any `json:"details"`
	Risk       string         `json:"risk"`
	Strong     bool           `json:"strong"`
	Status     string         `json:"status"`
	DecidedBy  string         `json:"decided_by"`
	HTTPStatus int            `json:"http_status"`
	Message    string         `json:"message"`
	Result     string         `json:"result"`
}

func (a *apiActions) Actions() []action.Spec {
	out := make([]action.Spec, 0, len(a.cards))
	for _, c := range a.cards {
		desc := c.Description
		if c.Strong {
			desc += " Carte sensible : la personne confirme avec sa double authentification."
		}
		out = append(out, action.Spec{Kind: apiKindPrefix + c.Name, Description: desc, Parameters: c.Parameters,
			Capability: action.CapAPI, External: true})
	}
	return out
}

func (a *apiActions) Prepare(ctx context.Context, kind string, params json.RawMessage) (action.Prepared, error) {
	tool := strings.TrimPrefix(kind, apiKindPrefix)
	if !json.Valid(params) {
		params = json.RawMessage("{}")
	}
	status, body, err := a.api.do(ctx, http.MethodPost, "/admin/operator/cards", a.token, a.reason,
		map[string]any{"tool": tool, "arguments": params, "investigation_id": a.inv})
	if err != nil {
		return action.Prepared{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return action.Prepared{}, fmt.Errorf("%w (HTTP %d) : %s", action.ErrRefused, status, clip(body))
	}
	var v apiCardView
	if err := json.Unmarshal(body, &v); err != nil || v.ID == uuid.Nil {
		return action.Prepared{}, errors.New("réponse de l'API illisible")
	}
	ref, _ := json.Marshal(apiCardRef{Card: v.ID})
	target := v.Target
	if target == "" {
		target = pathTarget(v)
	}
	state := ""
	if len(v.Details) > 0 {
		b, _ := json.Marshal(v.Details)
		state = string(b)
	}
	risk := action.Risk(v.Risk)
	if risk.Rank() == 0 {
		risk = action.RiskHigh
	}
	return action.Prepared{Target: target, Summary: v.Summary, Params: ref, Risk: risk, State: state}, nil
}

// pathTarget : à défaut de nom, l'outil.
func pathTarget(v apiCardView) string { return v.Tool }

func (a *apiActions) Execute(context.Context, string, json.RawMessage, action.Approval) (string, error) {
	return "", errors.New("operator: une action de l'API se valide auprès de l'API, jamais par l'Opérateur")
}

func (a *apiActions) card(ctx context.Context, params json.RawMessage) (apiCardView, error) {
	var ref apiCardRef
	if err := json.Unmarshal(params, &ref); err != nil || ref.Card == uuid.Nil {
		return apiCardView{}, errors.New("carte de l'API absente")
	}
	status, body, err := a.api.do(ctx, http.MethodGet, "/admin/operator/cards/"+url.PathEscape(ref.Card.String()), a.token, "", nil)
	if err != nil {
		return apiCardView{}, err
	}
	if status != http.StatusOK {
		return apiCardView{}, fmt.Errorf("carte de l'API (HTTP %d) : %s", status, clip(body))
	}
	var v apiCardView
	if err := json.Unmarshal(body, &v); err != nil {
		return apiCardView{}, errors.New("réponse de l'API illisible")
	}
	return v, nil
}

// Outcome : action.Remote.
func (a *apiActions) Outcome(ctx context.Context, _ string, params json.RawMessage) (action.Outcome, error) {
	v, err := a.card(ctx, params)
	if err != nil {
		return action.Outcome{}, err
	}
	switch v.Status {
	case "executed":
		res := fmt.Sprintf("Fait par l'API (HTTP %d).", v.HTTPStatus)
		if v.Result != "" {
			res += " Réponse : " + clipText(v.Result, 400)
		}
		return action.Outcome{Done: true, OK: true, DecidedBy: v.DecidedBy, Result: res}, nil
	case "failed":
		return action.Outcome{Done: true, DecidedBy: v.DecidedBy, Result: fmt.Sprintf("Refusé par la route (HTTP %d) : %s", v.HTTPStatus, v.Message)}, nil
	case "rejected":
		return action.Outcome{Done: true, Rejected: true, DecidedBy: v.DecidedBy, Result: v.Message}, nil
	case "expired":
		return action.Outcome{Done: true, Expired: true}, nil
	}
	return action.Outcome{}, nil // proposée ou en cours
}

// Reject : action.Remote.
func (a *apiActions) Reject(ctx context.Context, _ string, params json.RawMessage, note string) error {
	var ref apiCardRef
	if err := json.Unmarshal(params, &ref); err != nil || ref.Card == uuid.Nil {
		return errors.New("carte de l'API absente")
	}
	status, body, err := a.api.do(ctx, http.MethodPost, "/admin/operator/cards/"+url.PathEscape(ref.Card.String())+"/reject", a.token, "",
		map[string]string{"note": note})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("refus auprès de l'API (HTTP %d) : %s", status, clip(body))
	}
	return nil
}

// APICards : les actions de l'API d'une personne, sans catalogue (relire
// une issue, refuser).
func (a *APITools) APICards(id Identity) action.Remote {
	if a == nil || a.BaseURL == "" || id.Rescue {
		return nil
	}
	return &apiActions{api: a, token: id.Token}
}
