// Package domain : accès d'un projet à des registres d'images externes
// (docs/adr/0008-registres-externes.md).
package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DockerHub : hôte des images sans hôte (« nginx », « moi/app »). Docker Hub
// sert ses images sur ce nom, pas sur docker.io.
const DockerHub = "docker.io"

// dockerHubAuth : clé attendue par containerd dans un Secret de pull pour
// Docker Hub.
const dockerHubAuth = "https://index.docker.io/v1/"

// Limites des champs (colonnes de la migration 000023).
const (
	maxUsername = 255
	maxPassword = 4096
)

// Credential : ligne chiffrée.
type Credential struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID      uuid.UUID `gorm:"type:uuid;not null"`
	Host           string    `gorm:"not null"`
	Username       string    `gorm:"not null"`
	SealedPassword []byte    `gorm:"not null"`
	CreatedBy      *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Credential) TableName() string { return "project_registry_credentials" }

// View : ce que l'API montre. Le jeton ne se relit jamais, il se remplace.
type View struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Auth : identifiants en clair, pour le Secret de pull (worker seulement).
type Auth struct {
	Host     string
	Username string
	Password string
}

// ConfigKey : clé de l'hôte dans un fichier .dockerconfigjson.
func (a Auth) ConfigKey() string {
	if a.Host == DockerHub {
		return dockerHubAuth
	}
	return a.Host
}

// ProjectPath : registres d'un projet.
type ProjectPath struct {
	ProjectID string `path:"id" binding:"required"`
}

// HostPath : un registre d'un projet.
type HostPath struct {
	ProjectID string `path:"id" binding:"required"`
	Host      string `path:"host" binding:"required" description:"Hôte du registre (ghcr.io, docker.io, registry.gitlab.com…)"`
}

// PutRequest : créer ou remplacer l'accès à un registre.
type PutRequest struct {
	ProjectID string `path:"id" binding:"required"`
	Host      string `path:"host" binding:"required" description:"Hôte du registre (ghcr.io, docker.io, registry.gitlab.com…)"`
	Username  string `json:"username" binding:"required" description:"Utilisateur du registre"`
	Password  string `json:"password" binding:"required" description:"Jeton d'accès, en lecture seule de préférence. Jamais relisible ensuite"`
}

// ListResponse : accès du projet.
type ListResponse struct {
	Registries []View `json:"registries"`
}

// TestResponse : résultat du test d'un accès.
type TestResponse struct {
	// Result : ok, denied (identifiants refusés) ou unreachable.
	Result string `json:"result"`
}

// Résultats d'un test.
const (
	TestOK          = "ok"
	TestDenied      = "denied"
	TestUnreachable = "unreachable"
)

var (
	ErrInvalidHost      = errors.New("invalid registry host: give a host name such as ghcr.io or registry.gitlab.com, without https:// or path")
	ErrPlatformRegistry = errors.New("the betorun registry needs no access: images built by betorun are always available to their project")
	ErrInvalidUsername  = errors.New("invalid registry username")
	ErrInvalidPassword  = errors.New("invalid registry token: it must not be empty (4096 characters maximum)")
	ErrNotFound         = errors.New("no access to this registry in this project")
)

// hostPattern : nom d'hôte, port facultatif. Pas de schéma, de chemin ni
// d'identifiants.
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)

// NormalizeHost : hôte en minuscules ; les alias de Docker Hub deviennent
// docker.io. Vide si invalide.
func NormalizeHost(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	switch h {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "hub.docker.com":
		h = DockerHub
	}
	if len(h) > 253 || !hostPattern.MatchString(h) || !strings.Contains(h, ".") && !strings.Contains(h, ":") {
		return ""
	}
	return h
}

// ImageHost : hôte d'une référence d'image, selon les règles de Docker : le
// premier segment est un hôte s'il contient un point ou un port, ou s'il vaut
// localhost ; sinon l'image vient de Docker Hub.
func ImageHost(image string) string {
	ref := strings.TrimSpace(image)
	first, _, found := strings.Cut(ref, "/")
	if !found || !strings.ContainsAny(first, ".:") && first != "localhost" {
		return DockerHub
	}
	return NormalizeHost(first)
}

// ValidateCredentials : utilisateur et jeton.
func ValidateCredentials(username, password string) error {
	u := strings.TrimSpace(username)
	if u == "" || len(u) > maxUsername || strings.ContainsAny(u, ":\n\r") {
		return ErrInvalidUsername
	}
	if password == "" || len(password) > maxPassword || strings.ContainsAny(password, "\n\r") {
		return ErrInvalidPassword
	}
	return nil
}
