package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/core/pgerr"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/organizations"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

type ProjectRepository interface {
	CreateProject(ctx context.Context, project *dauth.Project, ownerID string) error
	DeleteProject(ctx context.Context, projectID string) error
	GetProjectByID(ctx context.Context, id string) (*dauth.Project, error)
	// UpdateProjectStatus : statut de provisionnement (PENDING, READY, ERROR).
	UpdateProjectStatus(ctx context.Context, id, status string) error
	// UpdateProjectLimits : plafonds CPU, mémoire et stockage du projet.
	UpdateProjectLimits(ctx context.Context, id, cpu, memory, storage string) error
	// ListProjects : projets de ids, ou des organisations orgIDs, les plus
	// récents d'abord, au plus limit (+1 pour savoir s'il en reste).
	// excluded : retirés même si leur organisation est listée.
	ListProjects(ctx context.Context, orgIDs, ids, excluded []uuid.UUID, limit int) ([]dauth.Project, error)
	// UpdateProject : nom et description (l'espace Kubernetes ne change pas).
	UpdateProject(ctx context.Context, id uuid.UUID, name, description string) error
	// OrganizationQuotas : nom, nombre de projets et plafond de chaque organisation.
	OrganizationQuotas(ctx context.Context, orgIDs []uuid.UUID) ([]OrgQuota, error)
	// SetMaxProjects : plafond de projets d'une organisation.
	SetMaxProjects(ctx context.Context, orgID uuid.UUID, max int) error
}

// ErrNameTaken : un projet de l'organisation porte déjà ce nom.
var ErrNameTaken = errors.New("a project with this name already exists in this organization")

// ErrProjectLimit : l'organisation a déjà son nombre maximal de projets.
var ErrProjectLimit = errors.New("this organization already has its maximum number of projects")

// nameTaken : violation de l'unicité du nom dans l'organisation.
func nameTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerr.UniqueViolation && pgErr.ConstraintName == "uq_projects_org_name"
}

type pgProjectRepo struct {
	db *gorm.DB
}

func NewProjectRepository(db *gorm.DB) ProjectRepository {
	return &pgProjectRepo{db: db}
}

// CreateProject crée le projet dans son organisation (à défaut, celle,
// personnelle, du propriétaire, créée au besoin) et accorde roles/owner,
// atomiquement. Le plafond de projets de l'organisation est vérifié sous
// verrou : deux créations simultanées ne le dépassent pas.
func (r *pgProjectRepo) CreateProject(ctx context.Context, project *dauth.Project, ownerID string) error {
	ownerUUID, err := uuid.Parse(ownerID)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if project.OrganizationID == uuid.Nil {
			orgID, err := organizations.EnsurePersonal(ctx, tx, ownerUUID, "")
			if err != nil {
				return err
			}
			project.OrganizationID = orgID
		}
		var max int
		if err := tx.Raw(`SELECT max_projects FROM organizations WHERE id = ? AND deleted_at IS NULL FOR UPDATE`,
			project.OrganizationID).Scan(&max).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&dauth.Project{}).Where("organization_id = ?", project.OrganizationID).Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(max) {
			return ErrProjectLimit
		}
		// Espace Kubernetes : fixé une fois pour toutes, tiré de l'ID.
		if project.ID == uuid.Nil {
			project.ID = uuid.New()
		}
		if project.Namespace == "" {
			project.Namespace = utils.NewProjectNamespace(project.ID)
		}
		if err := tx.Create(project).Error; err != nil {
			if nameTaken(err) {
				return ErrNameTaken
			}
			return err
		}
		if err := iam.AddBinding(ctx, tx, iam.Binding{
			Resource:  iam.ProjectName(project.ID),
			Role:      iam.RoleOwner,
			Principal: iam.UserPrincipal(ownerUUID),
		}, iam.KindUser+iam.PrincipalSeparator+ownerID); err != nil {
			return err
		}
		return nil
	})
}

// DeleteProject : la ligne et tous les droits IAM portés par le projet et
// ses ressources (sinon des bindings orphelins laissent croire à leurs
// membres qu'ils ont encore ce projet).
func (r *pgProjectRepo) DeleteProject(ctx context.Context, projectID string) error {
	id, err := uuid.Parse(projectID)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("id = ?", id).Delete(&domain.Project{}).Error; err != nil {
			return err
		}
		return iam.DeleteResourcePolicy(ctx, tx, iam.ProjectName(id))
	})
}

func (r *pgProjectRepo) GetProjectByID(ctx context.Context, id string) (*dauth.Project, error) {
	var project dauth.Project

	err := r.db.WithContext(ctx).
		Where("id = ?", id).
		First(&project).Error

	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (r *pgProjectRepo) UpdateProjectStatus(ctx context.Context, id, status string) error {
	return r.db.WithContext(ctx).Model(&dauth.Project{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": time.Now()}).Error
}

func (r *pgProjectRepo) UpdateProjectLimits(ctx context.Context, id, cpu, memory, storage string) error {
	res := r.db.WithContext(ctx).Model(&dauth.Project{}).Where("id = ?", id).
		Updates(map[string]any{"cpu_limit": cpu, "memory_limit": memory, "storage_limit": storage, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (r *pgProjectRepo) ListProjects(ctx context.Context, orgIDs, ids, excluded []uuid.UUID, limit int) ([]dauth.Project, error) {
	if len(orgIDs) == 0 && len(ids) == 0 {
		return nil, nil
	}
	q := r.db.WithContext(ctx).Model(&dauth.Project{})
	switch {
	case len(orgIDs) > 0 && len(ids) > 0:
		q = q.Where("organization_id IN ? OR id IN ?", orgIDs, ids)
	case len(orgIDs) > 0:
		q = q.Where("organization_id IN ?", orgIDs)
	default:
		q = q.Where("id IN ?", ids)
	}
	if len(excluded) > 0 {
		q = q.Where("id NOT IN ?", excluded)
	}
	var out []dauth.Project
	err := q.Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

func (r *pgProjectRepo) UpdateProject(ctx context.Context, id uuid.UUID, name, description string) error {
	res := r.db.WithContext(ctx).Model(&dauth.Project{}).Where("id = ?", id).
		Updates(map[string]any{"name": name, "description": description, "updated_at": time.Now()})
	if res.Error != nil {
		if nameTaken(res.Error) {
			return ErrNameTaken
		}
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// OrgQuota : projets d'une organisation et son plafond.
type OrgQuota struct {
	ID          uuid.UUID
	DisplayName string
	Projects    int
	MaxProjects int
}

func (r *pgProjectRepo) OrganizationQuotas(ctx context.Context, orgIDs []uuid.UUID) ([]OrgQuota, error) {
	if len(orgIDs) == 0 {
		return nil, nil
	}
	var out []OrgQuota
	err := r.db.WithContext(ctx).Raw(`
		SELECT o.id, o.display_name, o.max_projects,
		       (SELECT count(*) FROM projects p WHERE p.organization_id = o.id AND p.deleted_at IS NULL) AS projects
		FROM organizations o WHERE o.id IN ? AND o.deleted_at IS NULL
		ORDER BY o.display_name, o.id`, orgIDs).Scan(&out).Error
	return out, err
}

func (r *pgProjectRepo) SetMaxProjects(ctx context.Context, orgID uuid.UUID, max int) error {
	res := r.db.WithContext(ctx).Exec(`UPDATE organizations SET max_projects = ?, updated_at = now() WHERE id = ? AND deleted_at IS NULL`, max, orgID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
