package domain

import (
	"errors"
	"regexp"
	"time"
)

// Agent d'un cluster (socle multi-cluster, lot 3) : la plateforme prépare un
// cluster une fois avec un kubeconfig administrateur, gardé en mémoire le
// temps de l'installation et jamais enregistré. Elle y pose son propre compte
// (droits restreints, sans RBAC) et le watcher avec un fichier de clé neuf,
// puis ne garde que le kubeconfig du compte restreint (chiffré). L'image et
// la clé de l'agent se renouvellent ensuite par ce compte restreint.

const (
	// AgentNamespace : namespace du compte plateforme et du watcher.
	AgentNamespace = "betorun-system"
	// PlatformAccount : compte de service de la plateforme sur le cluster.
	PlatformAccount = "betorun-platform"
	// PlatformTokenSecret : jeton longue durée de ce compte (kubeconfig restreint).
	PlatformTokenSecret = "betorun-platform-token"
	// AgentName : Deployment, compte et rôle du watcher.
	AgentName = "betorun-watcher"
	// AgentKeySecret : fichier de clé du watcher (key.json).
	AgentKeySecret = "betorun-watcher-key"
	// AgentKeyDays : durée de vie d'un fichier de clé posé par la plateforme.
	AgentKeyDays = 365
	// AgentImageMax : longueur maximale d'une référence d'image.
	AgentImageMax = 255
)

var (
	ErrAgentImageMissing  = errors.New("no agent image: give one or configure kubernetes.agent_image")
	ErrInvalidAgentImage  = errors.New("invalid agent image reference")
	ErrAgentAPIURLMissing = errors.New("the platform API address seen from clusters is not configured (builds.public_api_url)")
	// ErrAdminRequired : le kubeconfig de préparation ne peut pas créer de
	// rôles de cluster (il faut celui d'un administrateur).
	ErrAdminRequired = errors.New("this kubeconfig cannot create cluster roles: use an administrator kubeconfig")
	// ErrPlatformTokenPending : Kubernetes n'a pas rempli le jeton du compte plateforme à temps.
	ErrPlatformTokenPending = errors.New("the cluster did not issue the platform account token in time")
	ErrAgentKeysUnavailable = errors.New("cluster agent keys are not available on this server")
	ErrClusterDisabled      = errors.New("the cluster is disabled")
)

// imageRe : [registre[:port]/]chemin[:tag][@sha256:…], sans espace.
var imageRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

// ValidAgentImage : référence d'image acceptable.
func ValidAgentImage(ref string) bool {
	return ref != "" && len(ref) <= AgentImageMax && imageRe.MatchString(ref)
}

// AgentSpec : ce que la plateforme pose pour le watcher.
type AgentSpec struct {
	ClusterID string
	Image     string
	// APIURL : …/kmanager/v1/clusters, joignable depuis le cluster.
	APIURL  string
	KeyFile []byte
}

// AgentState : agent lu sur le cluster (back-office).
type AgentState struct {
	// Installed : le Deployment de l'agent existe.
	Installed bool   `json:"installed"`
	Image     string `json:"image,omitempty"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	// Reason : dernier motif d'un conteneur qui ne démarre pas (ImagePullBackOff…).
	Reason string `json:"reason,omitempty"`
	// LastSeenAt : dernier signe de vie reçu du watcher.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	// InstalledAt : dernière installation ou mise à jour par la plateforme.
	InstalledAt *time.Time `json:"installed_at,omitempty"`
	// ManagedByPlatform : posé par la plateforme (sinon installé à la main).
	ManagedByPlatform bool `json:"managed_by_platform"`
}
