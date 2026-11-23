package domain

import "time"

type CreateProjectRequest struct {
	Name        string `json:"name" binding:"required,min=3,max=30"`
	Description string `json:"description"`
	// OrganizationID : organisation du projet (vide : organisation
	// personnelle). Il faut y détenir resourcemanager.projects.create.
	OrganizationID string `json:"organization_id" description:"Organisation du projet (vide : votre organisation personnelle)"`
	// Region : région du projet (GET /regions) ; vide : choisie par la plateforme.
	Region string `json:"region" description:"Région du projet (voir GET /regions) ; vide : choisie par la plateforme"`
	// Pas de limites : elles sont fixées par la plateforme, jamais par le
	// client (sinon chacun se donnerait les ressources qu'il veut).
}

type ProjectResponse struct {
	ProjectID  string `json:"project_id,omitempty"`
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

type ProjectStatusResponse struct {
	ProjectID string           `json:"project_id"`
	Status    string           `json:"status"`
	Phase     string           `json:"phase"`
	K8sInfo   *NamespaceStatus `json:"k8s_info,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

type NamespaceStatus struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	Phase  string `json:"phase"`
}

type NamespaceMetrics struct {
	CPUUsage      string   `json:"cpu_usage_actual"`    // Le réel (mClient)
	MemoryUsage   string   `json:"memory_usage_actual"` // Le réel (mClient)
	ReservedUsage UsageDTO `json:"reserved_usage"`      // Le provisionné (DB)
	// Limits : plafonds du projet (cpu_limit, memory_limit, storage_limit).
	Limits UsageDTO `json:"limits"`
	// Images : place occupée au registre de la plateforme ; absent si les
	// builds ne sont pas activés.
	Images *ImagesUsage `json:"images,omitempty"`
}

// ImagesUsage : images du projet au registre de la plateforme, en octets.
type ImagesUsage struct {
	UsedBytes int64 `json:"used_bytes"`
	// QuotaBytes : 0 si le projet n'a pas de quota d'images.
	QuotaBytes int64 `json:"quota_bytes"`
}

type UsageDTO struct {
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	Storage string `json:"storage"`
}

type AssignRoleRequest struct {
	UserID    string `json:"user_id" validate:"required,uuid"`
	ProjectID string `json:"project_id" validate:"required,uuid"`
	RoleName  string `json:"role_name" validate:"required"`
}

// ProjectSummary : un projet dans une liste.
type ProjectSummary struct {
	ProjectID      string    `json:"project_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	OrganizationID string    `json:"organization_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListProjectsResponse : projets accessibles. Truncated : il y en a plus que
// MaxListedProjects (la liste rend les plus récents).
type ListProjectsResponse struct {
	Projects  []ProjectSummary `json:"projects"`
	Truncated bool             `json:"truncated"`
	// Organizations : organisations où l'appelant peut créer un projet, avec
	// leur plafond (la console sait si « Nouveau projet » est possible).
	Organizations []OrganizationQuota `json:"organizations"`
}

// OrganizationQuota : projets d'une organisation et son plafond.
type OrganizationQuota struct {
	OrganizationID string `json:"organization_id"`
	DisplayName    string `json:"display_name"`
	Projects       int    `json:"projects"`
	MaxProjects    int    `json:"max_projects"`
}

// UpdateProjectRequest : nom et description (l'espace Kubernetes ne change pas).
type UpdateProjectRequest struct {
	ProjectID   string  `path:"id" binding:"required" json:"-" description:"ID du projet"`
	Name        *string `json:"name" description:"Nouveau nom (3 à 30 caractères, unique dans l'organisation)"`
	Description *string `json:"description" description:"Nouvelle description"`
}

// ProjectDetail : un projet et ses plafonds.
type ProjectDetail struct {
	ProjectSummary
	Limits UsageDTO `json:"limits"`
}

// MaxListedProjects : borne de GET /projects.
const MaxListedProjects = 200

// ProjectLimits : plafonds d'un projet, en quantités Kubernetes ; un champ
// vide garde la valeur actuelle.
type ProjectLimits struct {
	CPU     string `json:"cpu_limit,omitempty" description:"Plafond CPU (ex. 4000m)"`
	Memory  string `json:"memory_limit,omitempty" description:"Plafond mémoire (ex. 8Gi)"`
	Storage string `json:"storage_limit,omitempty" description:"Plafond de stockage (ex. 20Gi)"`
}

// ProjectAddonsLimit : plafond de services additionnels d'un projet.
type ProjectAddonsLimit struct {
	ProjectID string `json:"project_id"`
	MaxAddons int    `json:"max_addons"`
}

// ProjectLimitsResponse : limites enregistrées ; le quota Kubernetes suit
// via le workflow WorkflowID.
type ProjectLimitsResponse struct {
	ProjectID  string   `json:"project_id"`
	Limits     UsageDTO `json:"limits"`
	WorkflowID string   `json:"workflow_id"`
}
