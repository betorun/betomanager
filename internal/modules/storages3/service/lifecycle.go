package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Délai de grâce d'un projet (ADR 0015, point 8) : à la suppression, les
// clés sont désactivées et la lecture publique fermée, les données gardées ;
// à la restauration, tout revient ; à la purge, contenu, buckets et
// identités sont effacés. Les trois sont idempotents (repris en cas d'échec).

// SuspendProject : clés désactivées, lecture publique fermée.
func (s *Service) SuspendProject(ctx context.Context, projectID uuid.UUID) error {
	return s.setSuspended(ctx, projectID, true)
}

// ResumeProject : accès des clés et lecture publique rétablis.
func (s *Service) ResumeProject(ctx context.Context, projectID uuid.UUID) error {
	return s.setSuspended(ctx, projectID, false)
}

func (s *Service) setSuspended(ctx context.Context, projectID uuid.UUID, suspended bool) error {
	if s.available() != nil {
		return nil // pas de stockage : rien à faire
	}
	list, err := s.Repo.ProjectBuckets(ctx, projectID)
	if err != nil {
		return err
	}
	var errs []error
	for i := range list {
		b := &list[i]
		b.Suspended = suspended
		if err := s.applyAccess(ctx, b); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.BucketName, err))
			continue
		}
		if b.Public {
			// Public pendant la grâce : non ; le réglage est gardé pour la restauration.
			if err := s.Backend.SetPublic(ctx, b.BucketName, !suspended); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", b.BucketName, err))
				continue
			}
		}
		if err := s.Repo.UpdateBucket(ctx, b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// PurgeProject : identités, contenu et buckets du projet effacés (avant la
// suppression des lignes du projet, qui emporterait celles des buckets).
// Les variables des conteneurs ne sont pas retirées : ils sont supprimés
// avec le projet.
func (s *Service) PurgeProject(ctx context.Context, projectID uuid.UUID) error {
	if s.available() != nil {
		return nil
	}
	list, err := s.Repo.ProjectBuckets(ctx, projectID)
	if err != nil {
		return err
	}
	var errs []error
	for i := range list {
		if err := s.purgeBucket(ctx, list[i].ID, list[i].BucketName); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", list[i].BucketName, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) purgeBucket(ctx context.Context, id uuid.UUID, bucket string) error {
	keys, err := s.Repo.ListKeys(ctx, id)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.Backend.DeleteIdentity(ctx, k.Identity()); err != nil {
			return err
		}
		if err := s.Repo.DeleteKey(ctx, k.ID); err != nil {
			return err
		}
	}
	if err := s.Backend.DeleteBucket(ctx, bucket, true); err != nil {
		return err
	}
	return s.Repo.DeleteBucket(ctx, id)
}

// ProjectUsage : espace occupé par les buckets d'un projet, en octets
// (dernier relevé ; facturation).
func (s *Service) ProjectUsage(ctx context.Context, projectID uuid.UUID) (int64, error) {
	if s.Repo == nil {
		return 0, nil
	}
	list, err := s.Repo.ProjectBuckets(ctx, projectID)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, b := range list {
		total += b.UsedBytes
	}
	return total, nil
}
