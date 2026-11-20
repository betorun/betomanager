package security

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// Journal d'accès aux données et accès du staff (iam.md §10, §11).

const (
	ctxDataAccess         = "auditDataAccess"
	ctxStaffTarget        = "auditStaffTarget"
	ctxStaffJustification = "auditStaffJustification"

	// HeaderJustification : motif d'un accès du staff aux données d'un
	// client, encodé en URL (un en-tête HTTP n'accepte pas l'UTF-8 brut).
	HeaderJustification = "X-Betorun-Justification"

	minJustification = 10
	maxJustification = 300
)

// DataAccessPolicy : l'organisation de r journalise-t-elle les lectures de données ?
type DataAccessPolicy interface {
	DataAccessLogged(ctx context.Context, r iam.ResourceName) (bool, error)
}

// SetDataAccessPolicy : sans politique, les lectures ne sont jamais journalisées.
func (m *MiddlewareManager) SetDataAccessPolicy(p DataAccessPolicy) { m.dataAccess = p }

// MarkDataAccess : la route lit des données du client (logs, variables) ;
// à placer après la garde de permission.
func MarkDataAccess(c *gin.Context) { c.Set(ctxDataAccess, true) }

// dataAccessLogged : lecture autorisée à inscrire au journal ?
func (m *MiddlewareManager) dataAccessLogged(c *gin.Context) bool {
	if !c.GetBool(ctxDataAccess) || m.dataAccess == nil {
		return false
	}
	r := iam.ResourceName(c.GetString("iamResource"))
	if r == "" {
		return false
	}
	logged, err := m.dataAccess.DataAccessLogged(c.Request.Context(), r)
	if err != nil {
		// Doute : on journalise plutôt que de perdre une lecture.
		m.logger.Warnw("data access policy check failed", "resource", r, "error", err)
		return true
	}
	return logged
}

// StaffTarget : ressource du client visée par une route du staff.
type StaffTarget func(c *gin.Context) (iam.ResourceName, uuid.UUID, error)

// StaffOrganization : organisation dans le paramètre d'URL name.
func StaffOrganization(name string) StaffTarget {
	return func(c *gin.Context) (iam.ResourceName, uuid.UUID, error) {
		id, err := uuid.Parse(c.Param(name))
		if err != nil {
			return "", uuid.Nil, err
		}
		return iam.OrganizationName(id), uuid.Nil, nil
	}
}

// StaffProject : projet dans le paramètre d'URL name.
func StaffProject(name string) StaffTarget {
	return func(c *gin.Context) (iam.ResourceName, uuid.UUID, error) {
		id, err := uuid.Parse(c.Param(name))
		if err != nil {
			return "", uuid.Nil, err
		}
		return iam.ProjectName(id), id, nil
	}
}

// StaffAccess : contrôle à placer après la garde de permission plateforme
// d'une route qui touche aux données d'un client. Exige un motif (en-tête
// X-Betorun-Justification) et inscrit l'accès, lecture comprise, au journal
// d'audit du client (« Access Transparency »).
func StaffAccess(target StaffTarget) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(HeaderJustification))
		if decoded, err := url.QueryUnescape(raw); err == nil {
			raw = strings.TrimSpace(decoded)
		}
		if n := utf8.RuneCountInString(raw); n < minJustification || n > maxJustification {
			msg := "a justification of 10 to 300 characters is required to access customer data"
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "JUSTIFICATION_REQUIRED", "error": msg, "message": msg})
			return
		}
		r, project, err := target(c)
		if err != nil {
			msg := "invalid resource reference"
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "INVALID_INPUT", "error": msg, "message": msg})
			return
		}
		c.Set(ctxStaffTarget, r.String())
		c.Set(ctxStaffJustification, raw)
		if project != uuid.Nil {
			c.Set("projectID", project.String())
		}
	}
}
