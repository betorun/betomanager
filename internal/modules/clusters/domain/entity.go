package domain

import (
	"time"

	"github.com/google/uuid"
)

// Cluster : registre des clusters K3s gérés par kubemanager.
//
// Alimenté par `Enroll` (au démarrage du watcher) et touché à chaque
// `onPodEvent` via `LastSeenAt`. L'absence de `LastSeenAt` récent peut
// indiquer un watcher en panne → base pour des alertes futures.
type Cluster struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`

	// ClusterID : identifiant logique choisi par l'admin (ex: prod-fr-north-1).
	// Unique — un seul watcher par cluster logique. Utilisé dans les URLs
	// /ConfigService/:clusterID/Get et /ProjectService/:clusterID/onPodEvent.
	ClusterID string `gorm:"type:varchar(100);uniqueIndex;not null" json:"cluster_id"`

	// Version : version du binaire watcher (issue de son Enroll).
	Version string `gorm:"type:varchar(50)" json:"version"`

	// LastSeenAt : heure du dernier échange réussi avec ce cluster.
	// Base pour les alertes staleness (watcher down > 5min → warning).
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`

	// EnrolledAt : premier Enroll réussi (nil : déclaré, watcher pas encore venu).
	EnrolledAt *time.Time `json:"enrolled_at,omitempty"`

	// Registre (déclaré par l'équipe, lot 1 du socle multi-cluster).
	Name         string `gorm:"not null;default:''" json:"name"`
	Provider     string `gorm:"not null;default:'other'" json:"provider"`
	Distribution string `gorm:"not null;default:'k3s'" json:"distribution"`
	Region       string `gorm:"not null;default:''" json:"region"`
	State        string `gorm:"not null;default:'active'" json:"state"`
	IsDefault    bool   `gorm:"not null;default:false" json:"is_default"`
	Notes        string `gorm:"not null;default:''" json:"notes"`
	// KubeconfigCiphertext : jamais rendu (chiffré, AAD = cluster_id).
	KubeconfigCiphertext []byte     `gorm:"column:kubeconfig_ciphertext" json:"-"`
	APIServer            string     `gorm:"not null;default:''" json:"api_server"`
	KubeconfigSetAt      *time.Time `json:"kubeconfig_set_at,omitempty"`
	KubeconfigSetBy      string     `gorm:"not null;default:''" json:"-"`
	DeclaredBy           string     `gorm:"not null;default:''" json:"-"`

	// Réglages propres au cluster (vides : ceux de la configuration du
	// déploiement, pour le cluster par défaut).
	IngressClass     string `gorm:"not null;default:''" json:"ingress_class"`
	IngressNamespace string `gorm:"not null;default:''" json:"ingress_namespace"`
	IngressTarget    string `gorm:"not null;default:''" json:"ingress_target"`
	StorageClass     string `gorm:"not null;default:''" json:"storage_class"`
	// EgressGateway : passerelle de sortie ("" : aucune, cilium) ; IP de
	// sortie dédiées des projets (ADR 0014).
	EgressGateway string `gorm:"not null;default:''" json:"egress_gateway"`

	// Agent installé par la plateforme (lot 3) : image, fichier de clé en
	// service (identifiant public seulement), date et auteur.
	AgentImage       string     `gorm:"not null;default:''" json:"agent_image"`
	AgentKeyID       string     `gorm:"not null;default:''" json:"-"`
	AgentInstalledAt *time.Time `json:"agent_installed_at,omitempty"`
	AgentInstalledBy string     `gorm:"not null;default:''" json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
