package security

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
)

// Projet en suppression programmée : tout est figé jusqu'à sa restauration
// ou son effacement. Les lectures passent (la console affiche le projet et
// son bandeau) ; les écritures sous le projet sont refusées en 409
// PROJECT_PENDING_DELETION, sauf la restauration et la suppression elle-même
// (idempotente).

// ProjectStateChecker : état d'un projet (service des projets).
type ProjectStateChecker interface {
	PendingDeletion(ctx context.Context, project uuid.UUID) (bool, error)
}

// SetProjectState : active la garde.
func (m *MiddlewareManager) SetProjectState(c ProjectStateChecker) { m.projectState = c }

// codeProjectPendingDeletion : code d'erreur lu par la console et la CLI.
const codeProjectPendingDeletion = "PROJECT_PENDING_DELETION"

// checkProjectState : true si la requête peut continuer ; sinon la réponse
// est écrite. Échec fermé : état illisible → 503.
func (m *MiddlewareManager) checkProjectState(c *gin.Context, perm iam.Permission, r iam.ResourceName) bool {
	if m.projectState == nil || perm.ReadOnly() || perm == iam.ProjectsDelete || perm == iam.ProjectsUndelete {
		return true
	}
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	project, ok := r.Project()
	if !ok {
		return true
	}
	pending, err := m.projectState.PendingDeletion(c.Request.Context(), project)
	if err != nil {
		m.logger.Errorw("project state check failed", "project", project, "error", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "authorization unavailable"})
		return false
	}
	if pending {
		const msg = "this project is scheduled for deletion: restore it before changing anything"
		c.Set(ctxIAMDecision, audit.DecisionDeny)
		audit.Annotate(c.Request.Context(), "reason", "project_pending_deletion")
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"code": codeProjectPendingDeletion, "error": msg, "message": msg})
		return false
	}
	return true
}
