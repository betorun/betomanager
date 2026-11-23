package copilot

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/copilot/service"
)

// Plans « mettre en ligne » (lot B) : une carte, une validation, des étapes
// jouées à la suite par le serveur ; la console suit l'état.
//
//	POST /copilot/actions/:token/approve   (carte de plan) → 200 {status: running, plan}
//	GET  /copilot/plans/:token             → 200 PlanState
//	POST /copilot/plans/:token/pause       → 202 PlanState

// approvePlan : la carte est réservée (une seule exécution), l'état initial
// enregistré, puis le plan part en arrière-plan ; réponse immédiate.
func (h *Controller) approvePlan(c *gin.Context, p *service.Proposal, user, session uuid.UUID, acr string) {
	s := h.Service
	if err := h.claim(c, p); err != nil {
		writeActionError(c, err)
		return
	}
	ctx := c.Request.Context()
	st, err := s.StartPlan(ctx, p, service.PlanCaller{UserID: user, SessionID: session, ACR: acr,
		ClientIP: c.ClientIP(), AcceptLanguage: c.GetHeader("Accept-Language")})
	if err != nil {
		// Rien n'a commencé : la carte redevient validable.
		s.ProposalsStore().Release(context.WithoutCancel(ctx), p.Token)
		s.Logger.Errorw("copilot: plan not started", "error", err)
		writeActionError(c, err)
		return
	}
	s.ProposalsStore().Drop(context.WithoutCancel(ctx), p.Token)
	actionsTotal.WithLabelValues(p.Request.Tool, "approved").Inc()
	c.JSON(http.StatusOK, ActionResult{Status: service.PlanRunning, Plan: st})
}

// GetPlan : GET /copilot/plans/:token, état d'un plan de la personne.
func (h *Controller) GetPlan(c *gin.Context) {
	user, _, _, err := h.person(c)
	if err != nil {
		writeActionError(c, err)
		return
	}
	st, err := h.Service.LoadPlan(c.Request.Context(), c.Param("token"), user.String())
	if err != nil {
		writePlanError(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// PausePlan : POST /copilot/plans/:token/pause. Le plan finit l'étape en
// cours puis passe « paused » ; les étapes restantes sont sautées.
func (h *Controller) PausePlan(c *gin.Context) {
	user, _, _, err := h.person(c)
	if err != nil {
		writeActionError(c, err)
		return
	}
	st, err := h.Service.PausePlan(c.Request.Context(), c.Param("token"), user.String())
	if err != nil {
		writePlanError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, st)
}

// writePlanError : plan d'autrui ou expiré : introuvable (on ne révèle pas
// l'existence d'un plan d'une autre personne).
func writePlanError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrPlanNotFound) {
		jsonError(c, http.StatusNotFound, "PLAN_NOT_FOUND", "this plan has expired or does not exist")
		return
	}
	writeActionError(c, err)
}
