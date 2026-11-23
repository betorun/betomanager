// Package repository : VPS, offres, demandes (outbox) et secrets en base.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// GormStore : tables compute_*.
type GormStore struct{ DB *gorm.DB }

var _ service.Store = (*GormStore)(nil)

func (s *GormStore) Offers(ctx context.Context) ([]domain.Offer, error) {
	out := []domain.Offer{}
	return out, s.DB.WithContext(ctx).Order("position, code").Find(&out).Error
}

func (s *GormStore) Offer(ctx context.Context, code string) (*domain.Offer, error) {
	var o domain.Offer
	err := s.DB.WithContext(ctx).Where("code = ?", domain.NormalizeCode(code)).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrOfferNotFound
	}
	return &o, err
}

func (s *GormStore) SaveOffer(ctx context.Context, o *domain.Offer) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(o).Error
}

// EnqueueTx : demande en attente, dans la transaction de l'appelant.
func (s *GormStore) EnqueueTx(tx *gorm.DB, r *domain.Request) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.Status = domain.RequestPending
	if len(r.Payload) == 0 {
		r.Payload = []byte(`{}`)
	}
	err := tx.Create(r).Error
	if isUniqueViolation(err, "uq_compute_requests_active_vps") {
		return domain.ErrBusy
	}
	return err
}

// ClaimPending : FOR UPDATE SKIP LOCKED : deux relais ne prennent jamais la même demande.
func (s *GormStore) ClaimPending(ctx context.Context, limit int, fn func(tx *gorm.DB, rs []domain.Request) error) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rs []domain.Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", domain.RequestPending).Order("created_at").Limit(limit).Find(&rs).Error; err != nil {
			return err
		}
		if len(rs) == 0 {
			return nil
		}
		return fn(tx, rs)
	})
}

func (s *GormStore) MarkStarted(tx *gorm.DB, id uuid.UUID, at time.Time) error {
	return tx.Model(&domain.Request{}).Where("id = ? AND status = ?", id, domain.RequestPending).
		Updates(map[string]any{"status": domain.RequestStarted, "started_at": at, "attempts": gorm.Expr("attempts + 1")}).Error
}

// maxErrorLen : début du message gardé.
const maxErrorLen = 500

func (s *GormStore) MarkAttemptFailed(tx *gorm.DB, id uuid.UUID, errMsg string) error {
	if len(errMsg) > maxErrorLen {
		errMsg = errMsg[:maxErrorLen]
	}
	return tx.Model(&domain.Request{}).Where("id = ?", id).
		Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": errMsg}).Error
}

// FinishRequest : fin d'une demande lancée (par l'activité du workflow) ;
// idempotent : une demande déjà terminée ne change plus.
func (s *GormStore) FinishRequest(ctx context.Context, id uuid.UUID, ok bool, errMsg string, at time.Time) error {
	status := domain.RequestDone
	if !ok {
		status = domain.RequestFailed
	}
	if len(errMsg) > maxErrorLen {
		errMsg = errMsg[:maxErrorLen]
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&domain.Request{}).
			Where("id = ? AND status IN ?", id, []string{domain.RequestPending, domain.RequestStarted}).
			Updates(map[string]any{"status": status, "finished_at": at, "last_error": errMsg})
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		// L'opération du VPS est finie, réussie ou non : il redevient libre.
		return tx.Exec(`UPDATE compute_vps SET operation = '', version = version + 1, updated_at = ?
			WHERE operation <> '' AND id = (SELECT vps_id FROM compute_requests WHERE id = ?)`, at, id).Error
	})
}

func (s *GormStore) Request(ctx context.Context, id uuid.UUID) (*domain.Request, error) {
	var r domain.Request
	err := s.DB.WithContext(ctx).Where("id = ?", id).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

// ── Réconciliation ──────────────────────────────────────────────────────

func (s *GormStore) ProviderVPS(ctx context.Context, provider string) ([]domain.VPS, error) {
	out := []domain.VPS{}
	return out, s.DB.WithContext(ctx).Where("provider = ? AND provider_id <> ''", provider).Find(&out).Error
}

// ApplyObserved : écrit seulement si la version n'a pas bougé (une opération
// du client passée entre-temps garde la main).
func (s *GormStore) ApplyObserved(ctx context.Context, id uuid.UUID, version int64, state, ipv4, ipv6 string, at time.Time) (bool, error) {
	res := s.DB.WithContext(ctx).Model(&domain.VPS{}).Where("id = ? AND version = ?", id, version).
		Updates(map[string]any{"state": state, "ipv4": ipv4, "ipv6": ipv6, "version": gorm.Expr("version + 1"), "updated_at": at})
	return res.RowsAffected == 1, res.Error
}

// ReplaceMachines : le relevé du fournisseur remplace le précédent, d'un bloc.
func (s *GormStore) ReplaceMachines(ctx context.Context, provider string, ms []domain.ProviderMachine) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// La première apparition est gardée d'un relevé à l'autre.
		var prev []domain.ProviderMachine
		if err := tx.Where("provider = ?", provider).Find(&prev).Error; err != nil {
			return err
		}
		first := make(map[string]*time.Time, len(prev))
		before := make(map[string]domain.ProviderMachine, len(prev))
		for _, p := range prev {
			first[p.ProviderID] = p.FirstSeenAt
			before[p.ProviderID] = p
		}
		for i := range ms {
			if ms[i].Keep {
				if p, ok := before[ms[i].ProviderID]; ok {
					ms[i] = p
					continue
				}
			}
			if f := first[ms[i].ProviderID]; f != nil {
				ms[i].FirstSeenAt = f
			} else if ms[i].FirstSeenAt == nil {
				seen := ms[i].SeenAt
				ms[i].FirstSeenAt = &seen
			}
		}
		if err := tx.Where("provider = ?", provider).Delete(&domain.ProviderMachine{}).Error; err != nil {
			return err
		}
		if len(ms) == 0 {
			return nil
		}
		return tx.CreateInBatches(ms, 200).Error
	})
}

// purgeBatch : lignes effacées au plus par table et par passe.
const purgeBatch = 1000

func (s *GormStore) Purge(ctx context.Context, now, closedBefore, runsBefore time.Time) error {
	db := s.DB.WithContext(ctx)
	return errors.Join(
		// Un secret lu n'a plus à être gardé, même chiffré.
		db.Exec(`DELETE FROM compute_secrets WHERE id IN (SELECT id FROM compute_secrets
			WHERE read_at IS NOT NULL OR expires_at < ? LIMIT ?)`, now, purgeBatch).Error,
		db.Exec(`DELETE FROM compute_requests WHERE id IN (SELECT id FROM compute_requests
			WHERE status IN ('done', 'failed') AND finished_at < ? LIMIT ?)`, closedBefore, purgeBatch).Error,
		db.Exec(`DELETE FROM compute_reconcile_runs WHERE id IN (SELECT id FROM compute_reconcile_runs
			WHERE started_at < ? LIMIT ?)`, runsBefore, purgeBatch).Error,
	)
}

// BeginRun : verrou de transaction (passes concurrentes sérialisées), puis
// refus si une passe récente n'est pas close.
func (s *GormStore) BeginRun(ctx context.Context, r *domain.ReconcileRun, staleAfter time.Duration) (bool, error) {
	started := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended('compute:reconcile', 0))").Error; err != nil {
			return err
		}
		var running int64
		if err := tx.Model(&domain.ReconcileRun{}).
			Where("provider = ? AND finished_at IS NULL AND started_at > ?", r.Provider, r.StartedAt.Add(-staleAfter)).
			Count(&running).Error; err != nil {
			return err
		}
		if running > 0 {
			return nil
		}
		started = true
		return tx.Create(r).Error
	})
	return started && err == nil, err
}

func (s *GormStore) SaveRun(ctx context.Context, r *domain.ReconcileRun) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(r).Error
}

// ── Back-office ─────────────────────────────────────────────────────────

func (s *GormStore) ListVPS(ctx context.Context, limit int) ([]domain.VPS, error) {
	out := []domain.VPS{}
	return out, s.DB.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&out).Error
}

func (s *GormStore) Machines(ctx context.Context) ([]domain.ProviderMachine, error) {
	out := []domain.ProviderMachine{}
	return out, s.DB.WithContext(ctx).Order("provider, provider_id").Find(&out).Error
}

func (s *GormStore) Runs(ctx context.Context, limit int) ([]domain.ReconcileRun, error) {
	out := []domain.ReconcileRun{}
	return out, s.DB.WithContext(ctx).Order("started_at DESC").Limit(limit).Find(&out).Error
}

func (s *GormStore) OrganizationNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID          uuid.UUID
		DisplayName string
	}
	if err := s.DB.WithContext(ctx).Table("organizations").Select("id, display_name").Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.DisplayName
	}
	return out, nil
}

// ── VPS des clients ─────────────────────────────────────────────────────

func (s *GormStore) Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.DB.WithContext(ctx).Transaction(fn)
}

// CreateVPSTx : nom déjà pris dans le projet → domain.ErrNameTaken.
func (s *GormStore) CreateVPSTx(tx *gorm.DB, v *domain.VPS) error {
	err := tx.Create(v).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) || isUniqueViolation(err, "uq_compute_vps_name") {
		return domain.ErrNameTaken
	}
	return err
}

func (s *GormStore) UpdateVPSTx(tx *gorm.DB, id uuid.UUID, fields map[string]any) error {
	f := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		f[k] = v
	}
	f["version"] = gorm.Expr("version + 1")
	if _, ok := f["updated_at"]; !ok {
		f["updated_at"] = time.Now().UTC()
	}
	return tx.Model(&domain.VPS{}).Where("id = ?", id).Updates(f).Error
}

func (s *GormStore) LockVPSIfTx(tx *gorm.DB, id uuid.UUID, state string, paidUntil time.Time) (bool, error) {
	var n int64
	err := tx.Raw(`SELECT COUNT(*) FROM (SELECT id FROM compute_vps WHERE id = ? AND state = ? AND paid_until = ? FOR UPDATE) t`,
		id, state, paidUntil).Row().Scan(&n)
	return n == 1, err
}

func (s *GormStore) UpdateVPS(ctx context.Context, id uuid.UUID, fields map[string]any) error {
	return s.UpdateVPSTx(s.DB.WithContext(ctx), id, fields)
}

func (s *GormStore) VPS(ctx context.Context, id uuid.UUID) (*domain.VPS, error) {
	var v domain.VPS
	err := s.DB.WithContext(ctx).Where("id = ?", id).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrVPSNotFound
	}
	return &v, err
}

func (s *GormStore) OrganizationVPS(ctx context.Context, org uuid.UUID) ([]domain.VPS, error) {
	out := []domain.VPS{}
	return out, s.DB.WithContext(ctx).Where("organization_id = ? AND state <> ?", org, domain.StateDeleted).
		Order("created_at DESC").Find(&out).Error
}

func (s *GormStore) DueVPS(ctx context.Context, now time.Time, after *service.DueCursor, limit int) ([]domain.VPS, error) {
	out := []domain.VPS{}
	q := s.DB.WithContext(ctx).
		Where("(state IN ('running', 'stopped') OR (state = 'suspended' AND failure_code = 'unpaid'))").
		Where("paid_until IS NOT NULL AND paid_until <= ?", now)
	if after != nil {
		q = q.Where("(paid_until, id) > (?, ?)", after.PaidUntil, after.ID)
	}
	return out, q.Order("paid_until, id").Limit(limit).Find(&out).Error
}

func (s *GormStore) ProjectOrganization(ctx context.Context, project uuid.UUID) (uuid.UUID, error) {
	var org uuid.UUID
	err := s.DB.WithContext(ctx).Raw(`SELECT organization_id FROM projects WHERE id = ?`, project).Row().Scan(&org)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, domain.ErrProjectNotInOrganization
	}
	return org, err
}

// isUniqueViolation : code 23505 sur cette contrainte.
func isUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && (constraint == "" || pg.ConstraintName == constraint)
}

// ── Sauvegardes ─────────────────────────────────────────────────────────

func (s *GormStore) SaveBackups(ctx context.Context, b *domain.Backups) error {
	if len(b.RestorePoints) == 0 {
		b.RestorePoints = []byte(`[]`)
	}
	if len(b.Mounted) == 0 {
		b.Mounted = []byte(`[]`)
	}
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(b).Error
}

// Backups : nil si jamais relevé.
func (s *GormStore) Backups(ctx context.Context, vps uuid.UUID) (*domain.Backups, error) {
	var b domain.Backups
	err := s.DB.WithContext(ctx).Where("vps_id = ?", vps).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &b, err
}

// ── Secrets ─────────────────────────────────────────────────────────────

// SaveSecret : un seul secret de ce type par VPS (le précédent est effacé).
func (s *GormStore) SaveSecret(ctx context.Context, sec *domain.Secret) error {
	if sec.ID == uuid.Nil {
		sec.ID = uuid.New()
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("vps_id = ? AND kind = ?", sec.VPSID, sec.Kind).Delete(&domain.Secret{}).Error; err != nil {
			return err
		}
		return tx.Create(sec).Error
	})
}

// TakeSecret : lu une seule fois (marqué lu dans la même requête : deux
// lectures simultanées n'en obtiennent qu'une) ; nil si absent, lu ou expiré.
func (s *GormStore) TakeSecret(ctx context.Context, vps uuid.UUID, kind string, now time.Time) (*domain.Secret, error) {
	var out domain.Secret
	res := s.DB.WithContext(ctx).Raw(`UPDATE compute_secrets SET read_at = ?
		WHERE id = (SELECT id FROM compute_secrets WHERE vps_id = ? AND kind = ? AND read_at IS NULL AND expires_at > ?
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE)
		RETURNING *`, now, vps, kind, now).Scan(&out)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 || out.ID == uuid.Nil {
		return nil, nil
	}
	return &out, nil
}

func (s *GormStore) DeleteSecrets(ctx context.Context, vps uuid.UUID, kind string) error {
	return s.DB.WithContext(ctx).Where("vps_id = ? AND kind = ?", vps, kind).Delete(&domain.Secret{}).Error
}

// ── Stock, quotas, abus, e-mails du fournisseur ────────────────────────

func (s *GormStore) ReplaceStock(ctx context.Context, offer string, rows []domain.Stock) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("offer = ?", offer).Delete(&domain.Stock{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
}

func (s *GormStore) Stock(ctx context.Context) ([]domain.Stock, error) {
	out := []domain.Stock{}
	return out, s.DB.WithContext(ctx).Order("offer, location").Find(&out).Error
}

func (s *GormStore) Limits(ctx context.Context) (*domain.Limits, error) {
	var l domain.Limits
	return &l, s.DB.WithContext(ctx).First(&l, "id = 1").Error
}

func (s *GormStore) SaveLimits(ctx context.Context, l *domain.Limits) error {
	l.ID = 1
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(l).Error
}

// OrgLimits : nil si l'organisation suit les réglages de la plateforme.
func (s *GormStore) OrgLimits(ctx context.Context, org uuid.UUID) (*domain.OrgLimits, error) {
	var l domain.OrgLimits
	err := s.DB.WithContext(ctx).First(&l, "organization_id = ?", org).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &l, err
}

func (s *GormStore) SaveOrgLimits(ctx context.Context, l *domain.OrgLimits) error {
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(l).Error
}

func (s *GormStore) DeleteOrgLimits(ctx context.Context, org uuid.UUID) error {
	return s.DB.WithContext(ctx).Where("organization_id = ?", org).Delete(&domain.OrgLimits{}).Error
}

func (s *GormStore) CountVPS(ctx context.Context, org uuid.UUID) (int, error) {
	var n int64
	err := s.DB.WithContext(ctx).Model(&domain.VPS{}).
		Where("organization_id = ? AND state NOT IN ?", org, []string{domain.StateDeleted, domain.StateFailed}).Count(&n).Error
	return int(n), err
}

func (s *GormStore) LockAndCountVPSTx(tx *gorm.DB, org uuid.UUID) (int, error) {
	if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, "compute:order:"+org.String()).Error; err != nil {
		return 0, err
	}
	var n int64
	err := tx.Model(&domain.VPS{}).
		Where("organization_id = ? AND state NOT IN ?", org, []string{domain.StateDeleted, domain.StateFailed}).Count(&n).Error
	return int(n), err
}

func (s *GormStore) OrganizationCreatedAt(ctx context.Context, org uuid.UUID) (time.Time, error) {
	var at time.Time
	err := s.DB.WithContext(ctx).Raw(`SELECT created_at FROM organizations WHERE id = ?`, org).Row().Scan(&at)
	return at, err
}

func (s *GormStore) HasPaidTopup(ctx context.Context, org uuid.UUID) (bool, error) {
	var n int64
	err := s.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM billing_topups WHERE organization_id = ? AND status = 'succeeded'
		AND settled_at IS NOT NULL AND environment <> 'sandbox'`, org).Row().Scan(&n)
	return n > 0, err
}

// SaveProviderMail : un e-mail déjà rangé (même empreinte) est ignoré.
func (s *GormStore) SaveProviderMail(ctx context.Context, m *domain.ProviderMail) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.Fingerprint == "" {
		return s.DB.WithContext(ctx).Create(m).Error
	}
	return s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "fingerprint"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "fingerprint <> ''"}}},
		DoNothing:   true,
	}).Create(m).Error
}

func (s *GormStore) ProviderMails(ctx context.Context, openOnly bool, limit int) ([]domain.ProviderMail, error) {
	out := []domain.ProviderMail{}
	q := s.DB.WithContext(ctx).Order("received_at DESC").Limit(limit)
	if openOnly {
		q = q.Where("handled_at IS NULL")
	}
	return out, q.Find(&out).Error
}

func (s *GormStore) HandleProviderMail(ctx context.Context, id uuid.UUID, by string, at time.Time) error {
	return s.DB.WithContext(ctx).Model(&domain.ProviderMail{}).Where("id = ? AND handled_at IS NULL", id).
		Updates(map[string]any{"handled_at": at, "handled_by": by}).Error
}
