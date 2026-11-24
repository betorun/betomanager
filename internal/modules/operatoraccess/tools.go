package operatoraccess

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/iam/sts"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Outils du chemin normal (ADR 0016 §4) : des routes /admin en lecture,
// déclarées dans operator_decisions.go. L'Opérateur ne fabrique jamais
// d'URL : il demande le catalogue, puis l'API rejoue l'outil choisi avec le
// jeton délégué de la personne. Gardes, justification et audit s'appliquent
// comme pour un appel de la console.

// Catalog : outils et permission de leur route, posés au démarrage.
type Catalog struct {
	Tools   *tools.Catalog
	Perms   map[string]iam.Permission
	Handler http.Handler
}

type catalogSlot struct {
	mu  sync.RWMutex
	cat *Catalog
}

// Bind : le catalogue, une fois toutes les routes enregistrées.
func (h *Controller) Bind(c *Catalog) {
	h.slot.mu.Lock()
	h.slot.cat = c
	h.slot.mu.Unlock()
}

func (h *Controller) catalog() (*Catalog, error) {
	h.slot.mu.RLock()
	defer h.slot.mu.RUnlock()
	if h.slot.cat == nil {
		return nil, betoerrors.New(betoerrors.CodeInternal, "the Operator tools are not available")
	}
	return h.slot.cat, nil
}

// ToolView : un outil tel que le modèle le voit.
type ToolView struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// CardToolView : une action que l'agent peut proposer (jamais exécuter).
type CardToolView struct {
	ToolView
	Risk   string `json:"risk"`
	Strong bool   `json:"strong"`
}

// ToolsResponse : outils ouverts à la personne : lectures (items) et
// actions proposables sur carte (cards).
type ToolsResponse struct {
	Items []ToolView     `json:"items"`
	Cards []CardToolView `json:"cards"`
}

// operatorCaller : la requête vient d'un jeton délégué à l'Opérateur.
func operatorCaller(c *gin.Context) (iam.Principal, error) {
	actor, _, ok := security.Delegation(c)
	p, okP := security.PrincipalFromContext(c)
	if !ok || actor != sts.ActorOperator || !okP {
		return "", betoerrors.New(betoerrors.CodeForbidden, "an Operator token is required")
	}
	return p, nil
}

// Tools : le catalogue, réduit aux outils dont la personne a la permission.
func (h *Controller) Tools(c *gin.Context) (*ToolsResponse, error) {
	p, err := operatorCaller(c)
	if err != nil {
		return nil, err
	}
	cat, err := h.catalog()
	if err != nil {
		return nil, err
	}
	var wanted []iam.Permission
	for _, perm := range cat.Perms {
		wanted = append(wanted, perm)
	}
	granted, err := h.IAM.TestPermissions(c.Request.Context(), p, iam.Platform, append(wanted, iam.PlatformOperatorUse))
	if err != nil {
		return nil, err
	}
	has := map[iam.Permission]bool{}
	for _, g := range granted {
		has[g] = true
	}
	if !has[iam.PlatformOperatorUse] {
		return nil, betoerrors.New(betoerrors.CodeForbidden, "access to the Operator has been withdrawn")
	}
	out := ToolsResponse{Items: []ToolView{}, Cards: []CardToolView{}}
	for _, d := range cat.Tools.Definitions() {
		if !has[cat.Perms[d.Name]] {
			continue
		}
		v := ToolView{Name: d.Name, Description: d.Description, Parameters: d.Parameters}
		if t, ok := cat.Tools.Lookup(d.Name); ok && t.Write {
			// Les cartes ne sont proposées que si l'API peut les garder.
			if h.Cards != nil {
				out.Cards = append(out.Cards, CardToolView{ToolView: v, Risk: string(t.Risk), Strong: t.Strong})
			}
			continue
		}
		out.Items = append(out.Items, v)
	}
	return &out, nil
}

// CallInput : un appel d'outil.
type CallInput struct {
	Name      string          `path:"name" description:"Nom de l'outil"`
	Arguments json.RawMessage `json:"arguments" description:"Arguments (objet JSON, selon le schéma de l'outil)"`
}

// CallResponse : résultat, prêt pour le modèle.
type CallResponse struct {
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
	Truncated bool   `json:"truncated"`
	Status    int    `json:"status"`
}

// Call : rejoue l'outil avec le jeton de la requête (délégué, en lecture) et
// le motif du dossier (X-Betorun-Justification). Un refus de la route (droit,
// motif absent) revient en résultat d'erreur, lisible par le modèle.
func (h *Controller) Call(c *gin.Context, in *CallInput) (*CallResponse, error) {
	if _, err := operatorCaller(c); err != nil {
		return nil, err
	}
	cat, err := h.catalog()
	if err != nil {
		return nil, err
	}
	if _, ok := cat.Perms[in.Name]; !ok {
		return nil, betoerrors.New(betoerrors.CodeNotFound, "unknown Operator tool")
	}
	token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	res := cat.Tools.Call(c.Request.Context(), tools.Invoker{
		Handler: cat.Handler, Token: token, ClientIP: c.ClientIP(),
		AcceptLanguage: c.GetHeader("Accept-Language"),
		Justification:  c.GetHeader(security.HeaderJustification),
	}, in.Name, in.Arguments)
	return &CallResponse{Content: res.Content, IsError: res.IsError, Truncated: res.Truncated, Status: res.Status}, nil
}
