package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// ── Lecture publique ──────────────────────────────────────────────────────

// SetPublic : ouvre ou ferme la lecture publique des fichiers du bucket.
func (s *Service) SetPublic(ctx context.Context, projectID, id uuid.UUID, public bool) (*domain.BucketView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if b.Public != public {
		if err := s.Backend.SetPublic(ctx, b.BucketName, public); err != nil {
			return nil, fmt.Errorf("the public access could not be changed: %w", err)
		}
		b.Public = public
		if err := s.Repo.UpdateBucket(ctx, b); err != nil {
			return nil, err
		}
	}
	return s.Get(ctx, projectID, id)
}

// ── Rotation ──────────────────────────────────────────────────────────────

// RotateKey : nouveau secret pour la même clé. Branchement : posé dans le
// conteneur (redéployé) avant que l'ancien soit supprimé, sans coupure.
// Clé manuelle : le nouveau secret est rendu ici, une seule fois ; l'ancien
// cesse aussitôt de fonctionner.
func (s *Service) RotateKey(ctx context.Context, projectID, id, keyID uuid.UUID) (*domain.CreatedKey, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	k, err := s.Repo.GetKey(ctx, b.ID, keyID)
	if err != nil {
		return nil, err
	}
	if k.WorkloadID != nil && s.Workloads == nil {
		return nil, domain.ErrUnavailable
	}
	creds, err := s.Backend.AddAccessKey(ctx, k.Identity())
	if err != nil {
		return nil, fmt.Errorf("the key could not be rotated: %w", err)
	}
	old := k.AccessKeyID
	out := &domain.CreatedKey{Endpoint: s.Endpoint, Region: s.Region, Bucket: b.BucketName}
	name := ""
	if k.WorkloadID != nil {
		_, _, _, akVar, skVar := domain.Variables(k.Prefix)
		redeploy, err := s.Workloads.SetVariables(ctx, *k.WorkloadID, nil,
			map[string]string{akVar: creds.AccessKeyID, skVar: creds.SecretAccessKey}, nil)
		if err != nil {
			_ = s.Backend.DeleteAccessKey(context.WithoutCancel(ctx), k.Identity(), creds.AccessKeyID)
			return nil, err
		}
		out.Redeploy = redeploy
		name, _ = s.Workloads.NameOf(ctx, *k.WorkloadID)
	} else {
		out.SecretAccessKey = creds.SecretAccessKey
	}
	now := time.Now()
	k.AccessKeyID, k.RotatedAt = creds.AccessKeyID, &now
	if err := s.Repo.UpdateKey(ctx, k); err != nil {
		return nil, err
	}
	if err := s.Backend.DeleteAccessKey(ctx, k.Identity(), old); err != nil {
		// La nouvelle clé marche ; l'ancienne reste valide : on le dit.
		s.Logger.Errorw("storage: old access key not deleted after rotation", "key", k.ID, "error", err)
		return nil, fmt.Errorf("the previous secret could not be revoked, retry the rotation: %w", err)
	}
	out.Key = keyView(*k, name)
	return out, nil
}

// ── Explorateur de fichiers ───────────────────────────────────────────────

// ListObjects : contenu d'un dossier du bucket.
func (s *Service) ListObjects(ctx context.Context, projectID, id uuid.UUID, prefix, after string, limit int) (*domain.ObjectPage, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if !domain.ValidPrefixPath(prefix) || (after != "" && !domain.ValidObjectKey(after)) {
		return nil, domain.ErrInvalidObject
	}
	if limit <= 0 {
		limit = domain.DefaultPageSize
	}
	limit = min(limit, domain.MaxPageSize)
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	page, err := s.Backend.ListObjects(ctx, b.BucketName, prefix, after, limit)
	if err != nil {
		return nil, err
	}
	return &page, nil
}

// OpenObject : contenu d'un fichier (à fermer par l'appelant).
func (s *Service) OpenObject(ctx context.Context, projectID, id uuid.UUID, key string) (io.ReadCloser, domain.Object, error) {
	if err := s.available(); err != nil {
		return nil, domain.Object{}, err
	}
	if !domain.ValidObjectKey(key) || strings.HasSuffix(key, "/") {
		return nil, domain.Object{}, domain.ErrInvalidObject
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, domain.Object{}, err
	}
	return s.Backend.GetObject(ctx, b.BucketName, key)
}

// PutObject : envoi d'un fichier depuis la console. Refusé au-delà du
// quota ; l'espace affiché suit tout de suite (le relevé corrige ensuite).
// Un nom terminé par « / » et vide crée un dossier.
func (s *Service) PutObject(ctx context.Context, projectID, id uuid.UUID, key string, r io.Reader, size int64, contentType string) (*domain.Object, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	dir := strings.HasSuffix(key, "/")
	if !domain.ValidObjectKey(key) || (dir && size != 0) {
		return nil, domain.ErrInvalidObject
	}
	if size > domain.MaxConsoleUpload {
		return nil, domain.ErrObjectTooLarge
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if b.OverQuota || b.UsedBytes+size > b.QuotaBytes {
		return nil, domain.ErrOverQuota
	}
	if err := s.Backend.PutObject(ctx, b.BucketName, key, r, size, contentType); err != nil {
		return nil, fmt.Errorf("the file could not be written: %w", err)
	}
	b.UsedBytes += size
	if err := s.Repo.UpdateBucket(ctx, b); err != nil {
		s.Logger.Warnw("storage: usage not updated after upload", "bucket", b.ID, "error", err)
	}
	if dir {
		return &domain.Object{Key: key, Dir: true}, nil
	}
	now := time.Now()
	return &domain.Object{Key: key, Size: size, ContentType: contentType, LastModified: &now}, nil
}

// DeleteObjects : fichiers ou dossiers entiers (nom terminé par « / »),
// puis l'espace utilisé est relevé à nouveau (le quota peut se libérer).
func (s *Service) DeleteObjects(ctx context.Context, projectID, id uuid.UUID, keys []string) (*domain.DeleteObjectsResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if len(keys) == 0 || len(keys) > domain.MaxDeleteBatch {
		return nil, domain.ErrInvalidObject
	}
	for _, k := range keys {
		if !domain.ValidObjectKey(k) {
			return nil, domain.ErrInvalidObject
		}
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	n, err := s.Backend.DeleteObjects(ctx, b.BucketName, keys)
	if rerr := s.refresh(ctx, b); rerr != nil {
		s.Logger.Warnw("storage: usage not refreshed after delete", "bucket", b.ID, "error", rerr)
	}
	if err != nil {
		return nil, fmt.Errorf("some files could not be deleted: %w", err)
	}
	return &domain.DeleteObjectsResponse{Deleted: n}, nil
}

// ShareLink : lien de lecture d'un fichier, valable 1 h à 7 jours, sans
// compte ni clé (URL signée). Le fichier doit exister.
func (s *Service) ShareLink(ctx context.Context, projectID, id uuid.UUID, key string, hours int) (*domain.ShareLink, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if hours == 0 {
		hours = domain.DefaultLinkHours
	}
	if hours < 1 || hours > domain.MaxLinkHours {
		return nil, domain.ErrInvalidExpiry
	}
	rc, _, err := s.OpenObject(ctx, projectID, id, key)
	if err != nil {
		return nil, err
	}
	_ = rc.Close()
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(hours) * time.Hour
	u, err := s.Backend.PresignGet(ctx, b.BucketName, key, ttl)
	if err != nil {
		return nil, err
	}
	return &domain.ShareLink{URL: u, ExpiresAt: time.Now().Add(ttl).UTC()}, nil
}
