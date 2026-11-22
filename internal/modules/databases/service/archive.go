package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

// Copie finale avant suppression (ADR 0012, données d'une organisation
// suspendue trop longtemps) : même chemin que la copie de la remise à zéro
// (Job pg_dump → URL signée de l'API → stockage d'objets), sans rien vider.
// Sans copie arrivée, la base n'est pas supprimée.

// ArchiveStore : copies finales (nil : suppression par la plateforme impossible).
type ArchiveStore interface {
	SaveArchive(ctx context.Context, a *domain.Archive) error
	ExpiredArchives(ctx context.Context, now time.Time) ([]domain.Archive, error)
	DeleteArchive(ctx context.Context, id uuid.UUID) error
}

// ErrArchiveUnavailable : pas de stockage pour la copie finale ; la base
// n'est pas supprimée.
var ErrArchiveUnavailable = errors.New("the final copy of the database cannot be stored")

func archiveKey(org, databaseID uuid.UUID, taskID string) string {
	return "database-archives/" + org.String() + "/" + databaseID.String() + "/" + taskID + ".pgc"
}

// ArchiveAndDelete : copie finale de la base, gardée jusqu'à until, puis
// suppression de la base. Une base déjà en cours de suppression est laissée.
func (s *Service) ArchiveAndDelete(ctx context.Context, org, projectID, id uuid.UUID, reason string, until time.Time) error {
	if err := s.shellReady(); err != nil {
		return err
	}
	if !s.Imports.ready() || s.Archives == nil {
		return ErrArchiveUnavailable
	}
	t, err := s.readyDatabase(ctx, projectID, id)
	if err != nil {
		return err
	}
	if t.db.Status == domain.StatusDeleting {
		return nil
	}
	unlock := lockDatabase(t.db.ID)
	taskID := uuid.New()
	key := archiveKey(org, t.db.ID, taskID.String())
	up := &pendingUpload{key: key}
	pendingUploads.Store(taskID, up)
	exp := s.now().Add(snapshotURLValidity).Unix()
	upload := &SnapshotUpload{
		URL: fmt.Sprintf("%s/databases/snapshots/%s/upload?expires=%d&sig=%s",
			strings.TrimRight(s.Imports.PublicAPIURL, "/"), taskID, exp, s.Imports.Signer.SnapshotSignature(taskID, exp)),
		FetchImage: s.Imports.FetchImage, EgressAllow: s.Imports.EgressAllow,
	}
	dumpErr := s.Shell.Dump(ctx, t.ns, t.TaskTarget, upload)
	pendingUploads.Delete(taskID)
	up.mu.Lock()
	size, received := up.size, up.done
	up.mu.Unlock()
	unlock()
	keep := context.WithoutCancel(ctx)
	if !received {
		s.deleteObject(keep, key)
		if dumpErr == nil {
			dumpErr = errors.New("the final copy did not reach the API")
		}
		return fmt.Errorf("the database was not deleted: %w", dumpErr)
	}
	a := &domain.Archive{ID: taskID, OrganizationID: org, ProjectID: projectID, DatabaseID: t.db.ID, DatabaseName: t.db.Name,
		ObjectKey: key, SizeBytes: size, Reason: reason, CreatedAt: s.now(), ExpiresAt: until}
	if err := s.Archives.SaveArchive(keep, a); err != nil {
		s.deleteObject(keep, key)
		return fmt.Errorf("the final copy could not be recorded, the database was not deleted: %w", err)
	}
	return s.Delete(keep, projectID, t.db.ID)
}

// PurgeExpiredArchives : efface les copies finales arrivées à échéance.
func (s *Service) PurgeExpiredArchives(ctx context.Context) (int, error) {
	if s.Archives == nil {
		return 0, nil
	}
	expired, err := s.Archives.ExpiredArchives(ctx, s.now())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range expired {
		s.deleteObject(ctx, a.ObjectKey)
		if err := s.Archives.DeleteArchive(ctx, a.ID); err != nil {
			s.warn("expired final copy row not deleted", "archive", a.ID, "error", err)
			continue
		}
		n++
	}
	return n, nil
}

// ── Stockage ────────────────────────────────────────────────────────────
