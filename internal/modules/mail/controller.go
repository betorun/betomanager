// Package mail : e-mails de betorun, routes HTTP (docs/modules/mail.md).
// L'envoi lui-même est dans service (file asynq, livraison par le worker).
package mail

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/thekrauss/kubemanager/internal/modules/mail/domain"
	"github.com/thekrauss/kubemanager/internal/modules/mail/service"
)

// Controller : webhook SendGrid (public, signé) et adresses mises de côté (équipe).
type Controller struct {
	// SendGrid nil : webhook désactivé (clé absente).
	SendGrid *service.SendGridWebhook
	Admin    *service.Admin
}

// sendGridMaxBody : SendGrid regroupe ses événements en lots de 768 Ko au plus.
const sendGridMaxBody = 1 << 20

// SendGridEvents : handler brut (la signature porte sur le corps exact). Une
// erreur 5xx fait renvoyer le lot par SendGrid ; une signature fausse, non.
func (h *Controller) SendGridEvents(c *gin.Context) {
	if h.SendGrid == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, sendGridMaxBody+1))
	if err != nil || len(body) > sendGridMaxBody {
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}
	_, err = h.SendGrid.Handle(c.Request.Context(), c.GetHeader(service.SendGridSignatureHeader),
		c.GetHeader(service.SendGridTimestampHeader), body, time.Now())
	switch {
	case errors.Is(err, service.ErrBadSignature):
		c.AbortWithStatus(http.StatusUnauthorized)
	case err != nil:
		c.AbortWithStatus(http.StatusServiceUnavailable)
	default:
		c.Status(http.StatusNoContent)
	}
}

type ListSuppressionsRequest struct {
	Search string `query:"search" description:"Partie de l'adresse e-mail"`
}

type SuppressionsResponse struct {
	Suppressions []domain.Suppression `json:"suppressions"`
}

func (h *Controller) ListSuppressions(c *gin.Context, in *ListSuppressionsRequest) (*SuppressionsResponse, error) {
	list, err := h.Admin.List(c.Request.Context(), in.Search)
	if err != nil {
		return nil, err
	}
	return &SuppressionsResponse{Suppressions: list}, nil
}

type RemoveSuppressionRequest struct {
	Email string `query:"email" binding:"required" description:"Adresse à relâcher"`
}

func (h *Controller) RemoveSuppression(c *gin.Context, in *RemoveSuppressionRequest) error {
	return h.Admin.Remove(c.Request.Context(), in.Email)
}
