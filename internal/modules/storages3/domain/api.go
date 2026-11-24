package domain

import "time"

type ProjectPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
}

type BucketPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
}

type KeyPath struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	KeyID     string `path:"keyID" binding:"required" description:"ID de la clé"`
}

type CreateBucketRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	Name      string `json:"name" binding:"required" description:"Nom du bucket, unique dans le projet (le nom S3 réel est généré)"`
	QuotaGB   int    `json:"quota_gb" description:"Taille maximale en Go : 1, 5 (défaut), 10, 25, 50 ou 100"`
}

type UpdateBucketRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	QuotaGB   int    `json:"quota_gb" binding:"required" description:"Nouvelle taille maximale en Go : 1, 5, 10, 25, 50 ou 100"`
}

type DeleteBucketRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Confirm   string `query:"confirm" description:"Nom du bucket : efface aussi son contenu s'il n'est pas vide"`
}

type AttachRequest struct {
	ProjectID  string `path:"id" binding:"required" description:"ID du projet"`
	BucketID   string `path:"bucketID" binding:"required" description:"ID du bucket"`
	WorkloadID string `json:"workload_id" binding:"required" description:"ID du conteneur qui utilisera le bucket"`
	Prefix     string `json:"prefix" description:"Préfixe des variables (défaut : S3 → S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY_ID…)"`
	Access     string `json:"access" description:"read_write (défaut) ou read_only"`
}

type CreateKeyRequest struct {
	ProjectID string `path:"id" binding:"required" description:"ID du projet"`
	BucketID  string `path:"bucketID" binding:"required" description:"ID du bucket"`
	Name      string `json:"name" binding:"required" description:"Nom de la clé (ex. ci, laptop)"`
	Access    string `json:"access" description:"read_write (défaut) ou read_only"`
}

// BucketView : un bucket et ses clés (jamais de secret).
type BucketView struct {
	ID string `json:"id"`
	// Name : nom lisible ; Bucket : nom S3 réel, celui des applications.
	Name       string     `json:"name"`
	Bucket     string     `json:"bucket"`
	Endpoint   string     `json:"endpoint"`
	Region     string     `json:"region"`
	QuotaBytes int64      `json:"quota_bytes"`
	UsedBytes  int64      `json:"used_bytes"`
	UsageAt    *time.Time `json:"usage_at,omitempty"`
	// OverQuota : quota dépassé, le bucket est en lecture seule.
	OverQuota bool `json:"over_quota"`
	// Public / PublicURL : lecture publique ouverte et adresse de base des
	// fichiers (PublicURL + nom du fichier).
	Public    bool   `json:"public"`
	PublicURL string `json:"public_url,omitempty"`
	// Suspended : projet en délai de grâce, clés désactivées.
	Suspended bool      `json:"suspended"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Keys      []KeyView `json:"keys"`
}

// KeyView : une clé (partie publique seulement).
type KeyView struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	AccessKeyID  string     `json:"access_key_id"`
	Access       string     `json:"access"`
	Kind         string     `json:"kind"`
	WorkloadID   string     `json:"workload_id,omitempty"`
	WorkloadName string     `json:"workload_name,omitempty"`
	Prefix       string     `json:"prefix,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	RotatedAt    *time.Time `json:"rotated_at,omitempty"`
}

// CreatedKey : clé manuelle et son secret, montré cette seule fois. Après la
// rotation d'un branchement, pas de secret : il est posé dans le conteneur
// (Redeploy).
type CreatedKey struct {
	Key             KeyView `json:"key"`
	SecretAccessKey string  `json:"secret_access_key,omitempty"`
	Endpoint        string  `json:"endpoint"`
	Region          string  `json:"region"`
	Bucket          string  `json:"bucket"`
	Redeploy        bool    `json:"redeploy,omitempty"`
}

// AttachResponse : variables posées dans le conteneur (redéployé).
type AttachResponse struct {
	Key       KeyView  `json:"key"`
	Variables []string `json:"variables"`
	Redeploy  bool     `json:"redeploy"`
}
