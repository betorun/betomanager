package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// DeletionStore : suppression programmée d'un projet (délai de grâce).
// Chaque passage d'état est conditionnel : une restauration et la purge ne
// peuvent pas réussir toutes les deux.
type DeletionStore interface {
	// ScheduleDeletion : READY ou ERROR → PENDING_DELETION ; false si le
	// projet n'est pas (ou plus) dans l'un de ces états.
	ScheduleDeletion(ctx context.Context, id uuid.UUID, at time.Time, by string) (bool, error)
	// SetDeletionStopped : ce que la demande a arrêté (relancé à la restauration).
	SetDeletionStopped(ctx context.Context, id uuid.UUID, stopped dauth.DeletionStopped) error
	// CancelDeletion : PENDING_DELETION → READY ; rend ce qui avait été
	// arrêté. false si le projet n'était pas en suppression programmée.
	CancelDeletion(ctx context.Context, id uuid.UUID) (*dauth.DeletionStopped, bool, error)
	// DueDeletions : projets dont le délai de grâce est écoulé.
	DueDeletions(ctx context.Context, now time.Time, limit int) ([]dauth.Project, error)
	// MarkDeleting : PENDING_DELETION → DELETING (début de l'effacement) ;
	// false si une restauration est passée avant.
	MarkDeleting(ctx context.Context, id uuid.UUID) (bool, error)
	// ActiveVPS : VPS non supprimés du projet.
	ActiveVPS(ctx context.Context, id uuid.UUID) (int64, error)
	// PendingDeletion : vrai si le projet est en suppression programmée.
	PendingDeletion(ctx context.Context, id uuid.UUID) (bool, error)
}

var _ DeletionStore = (*pgProjectRepo)(nil)

func (r *pgProjectRepo) ScheduleDeletion(ctx context.Context, id uuid.UUID, at time.Time, by string) (bool, error) {
	res := r.db.WithContext(ctx).Exec(`UPDATE projects
		SET status = ?, deletion_scheduled_at = ?, deletion_requested_by = ?, deletion_stopped = NULL, updated_at = now()
		WHERE id = ? AND deleted_at IS NULL AND status IN (?, ?)`,
		utils.ProjectStatusPendingDeletion, at, by, id, utils.ProjectStatusReady, utils.ProjectStatusError)
	return res.RowsAffected == 1, res.Error
}

func (r *pgProjectRepo) SetDeletionStopped(ctx context.Context, id uuid.UUID, stopped dauth.DeletionStopped) error {
	raw, err := json.Marshal(stopped)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Exec(`UPDATE projects SET deletion_stopped = ? WHERE id = ? AND status = ?`,
		string(raw), id, utils.ProjectStatusPendingDeletion).Error
}

func (r *pgProjectRepo) CancelDeletion(ctx context.Context, id uuid.UUID) (*dauth.DeletionStopped, bool, error) {
	var rows []struct{ DeletionStopped []byte }
	// old : valeur avant la remise à zéro, verrouillée jusqu'à la fin.
	err := r.db.WithContext(ctx).Raw(`WITH old AS (
			SELECT id, deletion_stopped FROM projects WHERE id = ? AND status = ? FOR UPDATE
		)
		UPDATE projects
		SET status = ?, deletion_scheduled_at = NULL, deletion_requested_by = NULL, deletion_stopped = NULL, updated_at = now()
		FROM old WHERE projects.id = old.id
		RETURNING old.deletion_stopped AS deletion_stopped`,
		id, utils.ProjectStatusPendingDeletion, utils.ProjectStatusReady).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	stopped := &dauth.DeletionStopped{}
	if len(rows[0].DeletionStopped) > 0 {
		if err := json.Unmarshal(rows[0].DeletionStopped, stopped); err != nil {
			return nil, true, err
		}
	}
	return stopped, true, nil
}

func (r *pgProjectRepo) DueDeletions(ctx context.Context, now time.Time, limit int) ([]dauth.Project, error) {
	var out []dauth.Project
	err := r.db.WithContext(ctx).
		Where("status = ? AND deletion_scheduled_at <= ?", utils.ProjectStatusPendingDeletion, now).
		Order("deletion_scheduled_at").Limit(limit).Find(&out).Error
	return out, err
}

func (r *pgProjectRepo) MarkDeleting(ctx context.Context, id uuid.UUID) (bool, error) {
	res := r.db.WithContext(ctx).Exec(`UPDATE projects SET status = ?, updated_at = now() WHERE id = ? AND status = ?`,
		utils.ProjectStatusDeleting, id, utils.ProjectStatusPendingDeletion)
	return res.RowsAffected == 1, res.Error
}

func (r *pgProjectRepo) ActiveVPS(ctx context.Context, id uuid.UUID) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Raw(`SELECT count(*) FROM compute_vps WHERE project_id = ? AND state <> 'deleted'`, id).Scan(&n).Error
	return n, err
}

func (r *pgProjectRepo) PendingDeletion(ctx context.Context, id uuid.UUID) (bool, error) {
	var status []string
	err := r.db.WithContext(ctx).Raw(`SELECT status FROM projects WHERE id = ? AND deleted_at IS NULL`, id).Scan(&status).Error
	if err != nil {
		return false, err
	}
	if len(status) == 0 {
		return false, nil
	}
	return status[0] == utils.ProjectStatusPendingDeletion, nil
}
