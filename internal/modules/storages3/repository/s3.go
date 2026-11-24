package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
	"github.com/thekrauss/kubemanager/internal/modules/storages3/service"
)

// S3Config : S3 de la plateforme et identité d'administration du module
// (jamais celle des sites).
type S3Config struct {
	Endpoint  string // host:port, sans schéma
	UseSSL    bool
	Region    string
	AccessKey string
	SecretKey string
	// PublicEndpoint : adresse donnée aux applications et aux liens
	// (https://s3.<domaine>) ; vide : celle de Endpoint.
	PublicEndpoint string
}

// S3 : buckets par l'API S3, identités par l'API IAM (même port, ADR 0015).
type S3 struct {
	client *minio.Client
	// public : même identité, adresse publique (URL signées).
	public *minio.Client
	iam    *iamClient
	region string
}

func NewS3(c S3Config) (*S3, error) {
	if c.Endpoint == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("storage_s3: endpoint, access_key and secret_key are required")
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	cl, err := minio.New(c.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: c.UseSSL, Region: region})
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}
	public := cl
	if c.PublicEndpoint != "" {
		// Les URL signées portent l'hôte public : la signature en dépend.
		// Client hors ligne (région fixée : aucune requête de localisation).
		u, err := url.Parse(c.PublicEndpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("storage_s3: invalid public_endpoint %q", c.PublicEndpoint)
		}
		public, err = minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
			Secure: u.Scheme == "https", Region: region})
		if err != nil {
			return nil, err
		}
	}
	return &S3{client: cl, public: public, region: region,
		iam: &iamClient{endpoint: scheme + "://" + c.Endpoint, region: region, accessKey: c.AccessKey, secretKey: c.SecretKey}}, nil
}

func (s *S3) Name() string { return "platform" }

// guard : le module ne touche qu'aux buckets qu'il a nommés.
func guard(bucket string) error {
	if !strings.HasPrefix(bucket, domain.BucketPrefix) {
		return fmt.Errorf("storage_s3: refusing to manage bucket %q", bucket)
	}
	return nil
}

func (s *S3) CreateBucket(ctx context.Context, bucket string) error {
	if err := guard(bucket); err != nil {
		return err
	}
	return s.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: s.region})
}

func (s *S3) DeleteBucket(ctx context.Context, bucket string, purge bool) error {
	if err := guard(bucket); err != nil {
		return err
	}
	if purge {
		objects := s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true})
		for e := range s.client.RemoveObjects(ctx, bucket, objects, minio.RemoveObjectsOptions{}) {
			if e.Err != nil {
				return fmt.Errorf("remove %s: %w", e.ObjectName, e.Err)
			}
		}
	}
	err := s.client.RemoveBucket(ctx, bucket)
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch {
		case resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound:
			return nil // déjà supprimé
		case resp.Code == "BucketNotEmpty":
			return domain.ErrNotEmpty
		}
	}
	return err
}

func (s *S3) Usage(ctx context.Context, bucket string) (int64, error) {
	var total int64
	for o := range s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if o.Err != nil {
			return 0, o.Err
		}
		total += o.Size
	}
	return total, nil
}

// policyName : politique unique de chaque identité du module.
const policyName = "bucket"

// disabledResource : ressource qui ne peut pas exister (les noms générés
// n'ont que 12 lettres et chiffres après « bt- ») : la SeaweedFS n'accepte
// que des « Allow », une identité désactivée n'est donc autorisée qu'ici.
const disabledResource = "arn:aws:s3:::bt-disabled/none"

// bucketPolicy : accès au seul bucket. Lecture seule = lister et lire ;
// quota dépassé = lire et supprimer (le bucket doit pouvoir se vider) ;
// aucun accès = identité désactivée (délai de grâce).
func bucketPolicy(bucket, access string) map[string]any {
	resources := []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"}
	if access == domain.AccessNone {
		return map[string]any{
			"Version":   "2012-10-17",
			"Statement": []map[string]any{{"Effect": "Allow", "Action": []string{"s3:GetObject"}, "Resource": []string{disabledResource}}},
		}
	}
	actions := []string{"s3:GetObject", "s3:ListBucket", "s3:GetObjectTagging", "s3:ListMultipartUploadParts"}
	switch access {
	case domain.AccessReadWrite:
		actions = append(actions, "s3:PutObject", "s3:DeleteObject", "s3:PutObjectTagging",
			"s3:AbortMultipartUpload", "s3:ListBucketMultipartUploads")
	case domain.AccessReadDelete:
		actions = append(actions, "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListBucketMultipartUploads")
	}
	return map[string]any{
		"Version":   "2012-10-17",
		"Statement": []map[string]any{{"Effect": "Allow", "Action": actions, "Resource": resources}},
	}
}

func (s *S3) CreateIdentity(ctx context.Context, identity, bucket, access string) (service.Credentials, error) {
	if err := guard(bucket); err != nil {
		return service.Credentials{}, err
	}
	if err := s.iam.createUser(ctx, identity); err != nil {
		return service.Credentials{}, err
	}
	// Politique avant la clé : une clé n'existe jamais sans limite.
	if err := s.iam.putUserPolicy(ctx, identity, policyName, bucketPolicy(bucket, access)); err != nil {
		_ = s.DeleteIdentity(context.WithoutCancel(ctx), identity)
		return service.Credentials{}, err
	}
	id, secret, err := s.iam.createAccessKey(ctx, identity)
	if err != nil {
		_ = s.DeleteIdentity(context.WithoutCancel(ctx), identity)
		return service.Credentials{}, err
	}
	return service.Credentials{AccessKeyID: id, SecretAccessKey: secret}, nil
}

func (s *S3) SetIdentityAccess(ctx context.Context, identity, bucket, access string) error {
	if err := guard(bucket); err != nil {
		return err
	}
	return s.iam.putUserPolicy(ctx, identity, policyName, bucketPolicy(bucket, access))
}

func (s *S3) AddAccessKey(ctx context.Context, identity string) (service.Credentials, error) {
	id, secret, err := s.iam.createAccessKey(ctx, identity)
	if err != nil {
		return service.Credentials{}, err
	}
	return service.Credentials{AccessKeyID: id, SecretAccessKey: secret}, nil
}

func (s *S3) DeleteAccessKey(ctx context.Context, identity, accessKeyID string) error {
	if err := s.iam.deleteAccessKey(ctx, identity, accessKeyID); err != nil && !isNoSuchEntity(err) {
		return err
	}
	return nil
}

func (s *S3) DeleteIdentity(ctx context.Context, identity string) error {
	keys, err := s.iam.listAccessKeys(ctx, identity)
	if isNoSuchEntity(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.iam.deleteAccessKey(ctx, identity, k); err != nil && !isNoSuchEntity(err) {
			return err
		}
	}
	if err := s.iam.deleteUserPolicy(ctx, identity, policyName); err != nil && !isNoSuchEntity(err) {
		return err
	}
	if err := s.iam.deleteUser(ctx, identity); err != nil && !isNoSuchEntity(err) {
		return err
	}
	return nil
}
