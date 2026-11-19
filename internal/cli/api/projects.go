package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// ProjectSummary : un projet accessible.
type ProjectSummary struct {
	ProjectID      string    `json:"project_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	OrganizationID string    `json:"organization_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// OrganizationQuota : organisation où l'on peut créer un projet.
type OrganizationQuota struct {
	OrganizationID string `json:"organization_id"`
	DisplayName    string `json:"display_name"`
	Projects       int    `json:"projects"`
	MaxProjects    int    `json:"max_projects"`
}

// ProjectList : réponse de GET /projects.
type ProjectList struct {
	Projects      []ProjectSummary    `json:"projects"`
	Truncated     bool                `json:"truncated"`
	Organizations []OrganizationQuota `json:"organizations"`
}

// CreatedProject : création lancée (le projet est prêt en quelques secondes).
type CreatedProject struct {
	ProjectID  string `json:"project_id"`
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
}

func (c *Client) ListProjects(ctx context.Context) (*ProjectList, error) {
	var out ProjectList
	return &out, c.Do(ctx, http.MethodGet, pathProjects, nil, &out)
}

// CreateProject : organisationID vide = organisation personnelle.
func (c *Client) CreateProject(ctx context.Context, name, description, organizationID string) (*CreatedProject, error) {
	body := map[string]string{"name": name}
	if description != "" {
		body["description"] = description
	}
	if organizationID != "" {
		body["organization_id"] = organizationID
	}
	var out CreatedProject
	return &out, c.Do(ctx, http.MethodPost, pathProjects, body, &out)
}

// RenameProject : change le nom (et la description si non nil).
func (c *Client) RenameProject(ctx context.Context, id, name string, description *string) (*ProjectSummary, error) {
	body := map[string]any{"name": name}
	if description != nil {
		body["description"] = *description
	}
	var out ProjectSummary
	return &out, c.Do(ctx, http.MethodPatch, pathProjects+"/"+url.PathEscape(id), body, &out)
}
