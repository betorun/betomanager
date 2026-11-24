// Package domain : stockage objet S3 d'un projet (ADR 0015).
//
// Un bucket appartient à un projet ; son nom S3 réel est généré (l'espace
// des noms S3 est global). Chaque accès est une clé propre, limitée à ce seul
// bucket : une par conteneur branché, ou une clé manuelle (outils, CI) dont
// le secret n'est montré qu'une fois.
package domain

import (
	"crypto/rand"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
)

const (
	StatusReady    = "READY"
	StatusDeleting = "DELETING"

	// Accès d'une clé.
	AccessReadWrite = "read_write"
	AccessReadOnly  = "read_only"
	// AccessNone : identité désactivée (projet en délai de grâce) ; jamais
	// l'accès d'une clé, seulement celui posé sur le S3.
	AccessNone = "none"
	// AccessReadDelete : clé en écriture d'un bucket au-delà de son quota :
	// lire et supprimer (pour faire de la place), plus écrire.
	AccessReadDelete = "read_delete"

	// Origine d'une clé.
	KindAttachment = "attachment" // posée dans un conteneur
	KindManual     = "manual"     // créée à la main (secret montré une fois)

	// DefaultPrefix : préfixe des variables posées dans un conteneur
	// (S3_ENDPOINT, S3_BUCKET…).
	DefaultPrefix = "S3"

	// DefaultQuotaGB : quota d'un nouveau bucket.
	DefaultQuotaGB = 5

	// MaxBucketsPerProject, MaxManualKeys : limites de départ.
	MaxBucketsPerProject = 5
	MaxManualKeys        = 10

	// BucketPrefix : préfixe des noms S3 générés (l'identité d'administration
	// du module ne gère que ceux-là).
	BucketPrefix = "bt-"

	GB = int64(1) << 30
)

// AllowedQuotaGB : quotas proposés, en Go.
var AllowedQuotaGB = map[int]bool{1: true, 5: true, 10: true, 25: true, 50: true, 100: true}

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,28}[a-z0-9]$`)
	prefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)
)

// ValidName : nom lisible d'un bucket (unique dans le projet).
func ValidName(n string) bool { return nameRe.MatchString(n) }

// ValidPrefix : préfixe des variables d'un branchement.
func ValidPrefix(p string) bool { return prefixRe.MatchString(p) }

// ValidKeyName : nom d'une clé manuelle.
func ValidKeyName(n string) bool { return keyRe.MatchString(n) }

// ValidAccess : read_write ou read_only.
func ValidAccess(a string) bool { return a == AccessReadWrite || a == AccessReadOnly }

const nameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewBucketName : nom S3 réel, « bt- » suivi de 12 caractères aléatoires.
func NewBucketName() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = nameAlphabet[int(b[i])%len(nameAlphabet)]
	}
	return BucketPrefix + string(b)
}

// IdentityName : identité S3 d'une clé (une par clé, jamais réutilisée).
func IdentityName(keyID uuid.UUID) string {
	s := keyID.String()
	out := make([]byte, 0, 36)
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			out = append(out, s[i])
		}
	}
	return "btk-" + string(out)
}

var (
	ErrInvalidName      = errors.New("invalid bucket name: 3 to 30 lowercase letters, digits and hyphens, starting with a letter")
	ErrInvalidQuota     = errors.New("invalid quota: 1, 5, 10, 25, 50 or 100 GB")
	ErrInvalidPrefix    = errors.New("invalid variable prefix: uppercase letters, digits and underscores, starting with a letter")
	ErrInvalidKeyName   = errors.New("invalid key name: 1 to 40 lowercase letters, digits and hyphens, starting with a letter")
	ErrInvalidAccess    = errors.New("invalid access: read_write or read_only")
	ErrNameTaken        = errors.New("a bucket with this name already exists in the project")
	ErrNotFound         = errors.New("bucket not found")
	ErrKeyNotFound      = errors.New("key not found")
	ErrWorkloadNotFound = errors.New("container not found in this project")
	ErrAlreadyAttached  = errors.New("this bucket is already attached to this container")
	ErrTooMany          = errors.New("at most 5 buckets per project")
	ErrTooManyKeys      = errors.New("at most 10 manual keys per bucket")
	ErrNotEmpty         = errors.New("the bucket is not empty: confirm with its name to erase its content too")
	ErrDeleting         = errors.New("the bucket is being deleted")
	ErrUnavailable      = errors.New("object storage is not available on this platform")
	ErrSuspended        = errors.New("the project is being deleted: its buckets are suspended")
	ErrInvalidObject    = errors.New("invalid file name: 1 to 1024 characters, no leading slash, no \"..\" segment")
	ErrObjectNotFound   = errors.New("file not found")
	ErrObjectTooLarge   = errors.New("file too large for the console: at most 100 MB (use an S3 tool beyond)")
	ErrOverQuota        = errors.New("the bucket is over its quota: delete files or raise the quota")
	ErrInvalidExpiry    = errors.New("invalid link duration: 1 hour to 7 days")
)

// Bucket : un bucket d'un projet.
type Bucket struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID uuid.UUID `gorm:"type:uuid;not null"`
	// Name : nom lisible (unique dans le projet) ; BucketName : nom S3 réel.
	Name       string `gorm:"not null"`
	BucketName string `gorm:"column:bucket_name;not null"`
	QuotaBytes int64  `gorm:"not null"`
	// UsedBytes / UsageAt : dernier relevé de l'espace utilisé.
	UsedBytes int64
	UsageAt   *time.Time
	// OverQuota : quota dépassé, les clés en écriture sont passées en lecture seule.
	OverQuota bool `gorm:"column:over_quota;not null;default:false"`
	// Public : lecture publique des objets (lister reste refusé).
	Public bool `gorm:"not null;default:false"`
	// Suspended : projet en délai de grâce, toutes les clés sont désactivées.
	Suspended bool   `gorm:"not null;default:false"`
	Backend   string `gorm:"not null"`
	Status    string `gorm:"not null"`
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (Bucket) TableName() string { return "storage_buckets" }

// Key : une clé d'accès à un bucket (une identité S3 limitée à ce bucket).
type Key struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	BucketID  uuid.UUID `gorm:"type:uuid;not null"`
	ProjectID uuid.UUID `gorm:"type:uuid;not null"`
	Name      string    `gorm:"not null"`
	// AccessKeyID : partie publique ; le secret n'est jamais gardé.
	AccessKeyID string `gorm:"column:access_key_id;not null"`
	Access      string `gorm:"not null"`
	Kind        string `gorm:"not null"`
	// WorkloadID / Prefix : conteneur branché et préfixe de ses variables.
	WorkloadID *uuid.UUID `gorm:"type:uuid"`
	Prefix     string
	CreatedBy  string
	CreatedAt  time.Time
	// RotatedAt : dernière rotation du secret.
	RotatedAt *time.Time
}

func (Key) TableName() string { return "storage_keys" }

// Identity : identité S3 de la clé.
func (k Key) Identity() string { return IdentityName(k.ID) }

// Variables : noms posés dans le conteneur pour un préfixe.
func Variables(prefix string) (endpoint, region, bucket, accessKey, secretKey string) {
	return prefix + "_ENDPOINT", prefix + "_REGION", prefix + "_BUCKET", prefix + "_ACCESS_KEY_ID", prefix + "_SECRET_ACCESS_KEY"
}
