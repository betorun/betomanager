package controller

import (
	"github.com/gin-gonic/gin"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"

	"github.com/thekrauss/kubemanager/internal/middleware"
	"github.com/thekrauss/kubemanager/internal/middleware/security"
	"github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/auth/service"
)

type IAPIKeyController interface {
	ListKeys(c *gin.Context) ([]service.APIKeyDTO, error)
	GetKey(c *gin.Context, in *GetKeyInput) (*service.APIKeyDTO, error)
	ListAudit(c *gin.Context) ([]service.APIKeyAuditEntry, error)
	CreateKey(c *gin.Context, input *service.CreateAPIKeyInput) (*service.APIKeyCreatedResponse, error)
	RevokeKey(c *gin.Context, in *RevokeKeyInput) error
}

type IRBACController interface {
	ListAllRoles(c *gin.Context) ([]domain.RoleDTO, error)
	AssignRole(c *gin.Context, in *domain.AssignRoleRequest) error
	ListProjectMembers(c *gin.Context, in *ListMembersInput) ([]domain.ProjectMemberDTO, error)
	RevokeProjectAccess(c *gin.Context, in *RevokeAccessInput) error
}

type APIKeyController struct {
	Service *service.APIKeyService
}

type RBACController struct {
	RBACService *service.RBACService
}

func NewAPIKeyController(s *service.APIKeyService) *APIKeyController {
	return &APIKeyController{Service: s}
}

func NewRBACController(s *service.RBACService) *RBACController {
	return &RBACController{RBACService: s}
}

type ListMembersInput struct {
	ProjectID string `path:"projectID" validate:"required,uuid"`
}

type RevokeAccessInput struct {
	ProjectID string `path:"projectID" validate:"required,uuid"`
	UserID    string `path:"userID" validate:"required,uuid"`
}

func (ctrl *APIKeyController) ListKeys(c *gin.Context) ([]service.APIKeyDTO, error) {
	userID := c.GetString(middleware.ContextKeyUserID)
	if userID == "" {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "unauthorized")
	}
	return ctrl.Service.ListUserKeys(c.Request.Context(), userID)
}

func (ctrl *APIKeyController) CreateKey(c *gin.Context, input *service.CreateAPIKeyInput) (*service.APIKeyCreatedResponse, error) {
	userID := c.GetString(middleware.ContextKeyUserID)
	if userID == "" {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "unauthorized")
	}
	input.UserID = userID

	return ctrl.Service.CreateAPIKey(c.Request.Context(), *input)
}

type RevokeKeyInput struct {
	KeyID string `path:"id"`
}

func (ctrl *APIKeyController) RevokeKey(c *gin.Context, in *RevokeKeyInput) error {
	userID := c.GetString(middleware.ContextKeyUserID)
	return ctrl.Service.RevokeKey(c.Request.Context(), userID, in.KeyID)
}

type GetKeyInput struct {
	KeyID string `path:"id"`
}

// GetKey : introspection d'une clé (détails hors hash). Réservé au propriétaire de la clé.
func (ctrl *APIKeyController) GetKey(c *gin.Context, in *GetKeyInput) (*service.APIKeyDTO, error) {
	userID := c.GetString(middleware.ContextKeyUserID)
	if userID == "" {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "unauthorized")
	}
	return ctrl.Service.GetKey(c.Request.Context(), userID, in.KeyID)
}

// ListAudit : vue "audit" de toutes les clés du user (usage_count, status, last_used_at).
func (ctrl *APIKeyController) ListAudit(c *gin.Context) ([]service.APIKeyAuditEntry, error) {
	userID := c.GetString(middleware.ContextKeyUserID)
	if userID == "" {
		return nil, betoerrors.New(betoerrors.CodeUnauthorized, "unauthorized")
	}
	return ctrl.Service.ListAudit(c.Request.Context(), userID)
}

func (ctrl *RBACController) ListAllRoles(c *gin.Context) ([]domain.RoleDTO, error) {
	return ctrl.RBACService.ListAllRoles(c.Request.Context())
}

func (ctrl *RBACController) AssignRole(c *gin.Context, in *domain.AssignRoleRequest) error {
	caller, ok := security.PrincipalFromContext(c)
	if !ok {
		return betoerrors.New(betoerrors.CodeUnauthorized, "authentication required")
	}
	return ctrl.RBACService.AssignProjectRole(c.Request.Context(), caller, in)
}

func (ctrl *RBACController) ListProjectMembers(c *gin.Context, in *ListMembersInput) ([]domain.ProjectMemberDTO, error) {
	return ctrl.RBACService.ListProjectMembers(c.Request.Context(), in.ProjectID)
}

func (ctrl *RBACController) RevokeProjectAccess(c *gin.Context, in *RevokeAccessInput) error {
	caller, ok := security.PrincipalFromContext(c)
	if !ok {
		return betoerrors.New(betoerrors.CodeUnauthorized, "authentication required")
	}
	return ctrl.RBACService.RevokeProjectAccess(c.Request.Context(), caller, in.ProjectID, in.UserID)
}
