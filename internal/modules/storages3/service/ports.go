// Package service : buckets S3 des projets, leurs clés et leur quota (ADR 0015).
package service

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// Credentials : clé d'accès S3 (le secret n'est jamais gardé par l'API).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Backend : le S3 de la plateforme (SeaweedFS en V1). Le service ne parle
// qu'à cette interface : un autre S3 pourra s'y brancher.
type Backend interface {
	// Name : emplacement enregistré avec chaque bucket.
	Name() string
	CreateBucket(ctx context.Context, bucket string) error
	// DeleteBucket : purge=true efface d'abord le contenu ; sinon le bucket
	// doit être vide (domain.ErrNotEmpty).
	DeleteBucket(ctx context.Context, bucket string, purge bool) error
	// Usage : octets occupés.
	Usage(ctx context.Context, bucket string) (int64, error)
	// CreateIdentity : identité limitée au seul bucket, et sa clé. access :
	// domain.AccessReadWrite, AccessReadOnly ou AccessNone (désactivée).
	CreateIdentity(ctx context.Context, identity, bucket, access string) (Credentials, error)
	// SetIdentityAccess : accès posé sur le S3 (quota dépassé : lecture
	// seule ; délai de grâce : aucun).
	SetIdentityAccess(ctx context.Context, identity, bucket, access string) error
	// AddAccessKey / DeleteAccessKey : rotation du secret d'une identité
	// (nouvelle clé d'abord, l'ancienne supprimée ensuite).
	AddAccessKey(ctx context.Context, identity string) (Credentials, error)
	DeleteAccessKey(ctx context.Context, identity, accessKeyID string) error
	// DeleteIdentity : identité et ses clés ; absente : pas d'erreur.
	DeleteIdentity(ctx context.Context, identity string) error
	// SetPublic : lecture publique des objets (lister reste refusé).
	SetPublic(ctx context.Context, bucket string, public bool) error

	// Objets (explorateur de la console).
	ListObjects(ctx context.Context, bucket, prefix, after string, limit int) (domain.ObjectPage, error)
	// GetObject : contenu et description ; absent : domain.ErrObjectNotFound.
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, domain.Object, error)
	PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error
	// DeleteObjects : un nom terminé par « / » efface tout le dossier ;
	// rend le nombre d'objets effacés.
	DeleteObjects(ctx context.Context, bucket string, keys []string) (int, error)
	// PresignGet : URL de lecture d'un objet, signée pour ttl, à l'adresse publique.
	PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)
}

// Repository : buckets et clés en base.
type Repository interface {
	CreateBucket(ctx context.Context, b *domain.Bucket) error
	ListBuckets(ctx context.Context, projectID uuid.UUID) ([]domain.Bucket, error)
	GetBucket(ctx context.Context, projectID, id uuid.UUID) (*domain.Bucket, error)
	UpdateBucket(ctx context.Context, b *domain.Bucket) error
	DeleteBucket(ctx context.Context, id uuid.UUID) error
	// AllBuckets : tous les buckets prêts (relevé de l'espace utilisé).
	AllBuckets(ctx context.Context) ([]domain.Bucket, error)

	CreateKey(ctx context.Context, k *domain.Key) error
	ListKeys(ctx context.Context, bucketID uuid.UUID) ([]domain.Key, error)
	GetKey(ctx context.Context, bucketID, id uuid.UUID) (*domain.Key, error)
	UpdateKey(ctx context.Context, k *domain.Key) error
	DeleteKey(ctx context.Context, id uuid.UUID) error
	// ProjectBuckets : tous les buckets d'un projet, en suppression compris
	// (délai de grâce et purge).
	ProjectBuckets(ctx context.Context, projectID uuid.UUID) ([]domain.Bucket, error)
}

// Projects : un projet existe et n'est pas en cours de suppression.
type Projects interface {
	ProjectReady(ctx context.Context, id uuid.UUID) error
}

// Workloads : conteneurs (projet d'appartenance, variables posées avec redéploiement).
type Workloads interface {
	ProjectOf(ctx context.Context, workloadID uuid.UUID) (uuid.UUID, error)
	NameOf(ctx context.Context, workloadID uuid.UUID) (string, error)
	// SetVariables : env (visibles) et secrets posés, unset retirés, en un
	// seul redéploiement.
	SetVariables(ctx context.Context, workloadID uuid.UUID, env, secrets map[string]string, unset []string) (redeploy bool, err error)
}

// QuotaAlerts : alertes aux propriétaires et éditeurs (80 % puis quota
// atteint) ; la répétition est filtrée par le module d'alertes.
type QuotaAlerts interface {
	BucketQuota(ctx context.Context, project, bucket uuid.UUID, level int, name string, used, quota int64)
}

// ErrProjectNotReady : projet absent de l'état READY (création en cours…).
var ErrProjectNotReady = errors.New("project is not ready")
