package service

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/repository"
)

// Multi-projets (Phase 2) : lecture, renommage, plafond par organisation.

var (
	// ErrProjectLimit : l'organisation a déjà son nombre maximal de projets.
	ErrProjectLimit = repository.ErrProjectLimit
	// ErrInvalidProjectName : nom vide, trop court, trop long ou avec des
	// caractères de contrôle.
	ErrInvalidProjectName = errors.New("the project name must be 3 to 30 characters, without control characters")
	// ErrProjectNotFound : projet inconnu.
	ErrProjectNotFound = errors.New("project not found")
)

// cleanName : nom affiché, libre (il ne sert plus d'espace Kubernetes),
// espaces de bord retirés.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 3 || n > 30 {
		return "", ErrInvalidProjectName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidProjectName
		}
	}
	return name, nil
}

func summary(p *authdomain.Project) typesproject.ProjectSummary {
	return typesproject.ProjectSummary{
		ProjectID:      p.ID.String(),
		Name:           p.Name,
		Description:    p.Description,
		Status:         p.Status,
		OrganizationID: p.OrganizationID.String(),
		CreatedAt:      p.CreatedAt,
	}
}

// GetProject : un projet et ses plafonds.
func (s *ProjectService) GetProject(ctx context.Context, projectID string) (*typesproject.ProjectDetail, error) {
	p, err := s.Repos.GetProjectByID(ctx, projectID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	return &typesproject.ProjectDetail{
		ProjectSummary: summary(p),
		Limits:         typesproject.UsageDTO{CPU: p.CpuLimit, Memory: p.MemoryLimit, Storage: p.StorageLimit},
	}, nil
}

// UpdateProject : nom et description. L'espace Kubernetes, fixé à la
// création, ne change pas : renommer ne touche à rien dans le cluster.
func (s *ProjectService) UpdateProject(ctx context.Context, in typesproject.UpdateProjectRequest) (*typesproject.ProjectDetail, error) {
	id, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, ErrProjectNotFound
	}
	p, err := s.Repos.GetProjectByID(ctx, id.String())
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, err
	}
	name, description := p.Name, p.Description
	if in.Name != nil {
		if name, err = cleanName(*in.Name); err != nil {
			return nil, err
		}
	}
	if in.Description != nil {
		description = strings.TrimSpace(*in.Description)
	}
	if err := s.Repos.UpdateProject(ctx, id, name, description); err != nil {
		if errors.Is(err, repository.ErrNameTaken) {
			return nil, ErrProjectNameTaken
		}
		return nil, err
	}
	return s.GetProject(ctx, id.String())
}

// OrganizationQuotas : organisations où l'appelant peut créer un projet.
func (s *ProjectService) OrganizationQuotas(ctx context.Context, orgIDs []uuid.UUID) ([]typesproject.OrganizationQuota, error) {
	rows, err := s.Repos.OrganizationQuotas(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	out := make([]typesproject.OrganizationQuota, 0, len(rows))
	for _, r := range rows {
		out = append(out, typesproject.OrganizationQuota{
			OrganizationID: r.ID.String(), DisplayName: r.DisplayName, Projects: r.Projects, MaxProjects: r.MaxProjects,
		})
	}
	return out, nil
}

// SetMaxProjects : plafond de projets d'une organisation (staff). Il peut
// être inférieur au nombre actuel : seules les nouvelles créations sont
// refusées.
func (s *ProjectService) SetMaxProjects(ctx context.Context, orgID uuid.UUID, max int) (*typesproject.OrganizationQuota, error) {
	if err := s.Repos.SetMaxProjects(ctx, orgID, max); err != nil {
		return nil, err
	}
	rows, err := s.OrganizationQuotas(ctx, []uuid.UUID{orgID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rows[0], nil
}
