package service

import (
	"context"

	"github.com/google/uuid"

	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
)

func (s *ProjectService) ListProjects(ctx context.Context, orgIDs, projectIDs, excluded []uuid.UUID) (*typesproject.ListProjectsResponse, error) {
	rows, err := s.Repos.ListProjects(ctx, orgIDs, projectIDs, excluded, typesproject.MaxListedProjects+1)
	if err != nil {
		return nil, err
	}
	out := &typesproject.ListProjectsResponse{Projects: []typesproject.ProjectSummary{}}
	if len(rows) > typesproject.MaxListedProjects {
		rows = rows[:typesproject.MaxListedProjects]
		out.Truncated = true
	}
	for _, p := range rows {
		out.Projects = append(out.Projects, typesproject.ProjectSummary{
			ProjectID:      p.ID.String(),
			Name:           p.Name,
			Description:    p.Description,
			Status:         p.Status,
			OrganizationID: p.OrganizationID.String(),
			CreatedAt:      p.CreatedAt,
		})
	}
	return out, nil
}
