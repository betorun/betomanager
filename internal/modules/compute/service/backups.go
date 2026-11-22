package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	billingDomain "github.com/thekrauss/kubemanager/internal/modules/billing/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
)

// BackupsView : sauvegardes d'un VPS vues par le client (dernier relevé).
type BackupsView struct {
	Automated       bool                   `json:"automated"`
	RestorePoints   []time.Time            `json:"restore_points"`
	Mounted         []domain.MountedBackup `json:"mounted"`
	Snapshot        *domain.SnapshotInfo   `json:"snapshot"`
	SnapshotOption  string                 `json:"snapshot_option"`
	SnapshotMonthly int64                  `json:"snapshot_monthly"`
	Currency        string                 `json:"currency"`
	RefreshedAt     *time.Time             `json:"refreshed_at"`
}

func decodeBackups(b *domain.Backups) (points []time.Time, mounted []domain.MountedBackup, snap *domain.SnapshotInfo) {
	points, mounted = []time.Time{}, []domain.MountedBackup{}
	if b == nil {
		return points, mounted, nil
	}
	_ = json.Unmarshal(b.RestorePoints, &points)
	_ = json.Unmarshal(b.Mounted, &mounted)
	if len(b.Snapshot) > 0 && string(b.Snapshot) != "null" {
		var s domain.SnapshotInfo
		if json.Unmarshal(b.Snapshot, &s) == nil {
			snap = &s
		}
	}
	return points, mounted, snap
}

// Backups : relevé du VPS et prix de l'option snapshot dans la monnaie du client.
func (s *Service) Backups(ctx context.Context, org, id uuid.UUID) (*BackupsView, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	b, err := s.Store.Backups(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	points, mounted, snap := decodeBackups(b)
	out := &BackupsView{RestorePoints: points, Mounted: mounted, Snapshot: snap, SnapshotOption: v.SnapshotOption, Currency: v.Currency}
	if b != nil {
		out.Automated = b.Automated
		at := b.RefreshedAt
		out.RefreshedAt = &at
	}
	if s.Catalogs != nil {
		if o, err := s.Store.Offer(ctx, v.Offer); err == nil {
			if rate, err := s.Catalogs.Rate(ctx, org); err == nil {
				out.SnapshotMonthly, out.Currency = o.SnapshotPrice(rate), rate.Currency
			}
		}
	}
	return out, nil
}

// RestoreRequest : point de restauration et mode.
type RestoreRequest struct {
	RestorePoint time.Time `json:"restore_point" binding:"required" description:"Point de restauration (date de la sauvegarde)"`
	Mode         string    `json:"mode" description:"full : remplace tout le disque ; mount : attache la sauvegarde en disque supplémentaire"`
}

// UnmountRequest : point monté à détacher.
type UnmountRequest struct {
	RestorePoint time.Time `json:"restore_point" binding:"required" description:"Point de restauration monté"`
}

// SnapshotRequest : description du snapshot.
type SnapshotRequest struct {
	Description string `json:"description" description:"Description du snapshot (facultative, 100 caractères au plus)"`
}

// SubscribeRequest : prix annoncé de l'option.
type SubscribeRequest struct {
	MonthlyPrice int64 `json:"monthly_price" binding:"required" description:"Prix mensuel affiché de l'option, dans la monnaie de l'organisation"`
}

func (s *Service) loadReady(ctx context.Context, org, id uuid.UUID) (*domain.VPS, []time.Time, []domain.MountedBackup, *domain.SnapshotInfo, *domain.Backups, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if err := ready(v); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	b, err := s.Store.Backups(ctx, v.ID)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	points, mounted, snap := decodeBackups(b)
	return v, points, mounted, snap, b, nil
}

func samePoint(a, b time.Time) bool { return a.Equal(b) || a.Unix() == b.Unix() }

// Restore : remplace le disque (full) ou attache la sauvegarde (mount).
func (s *Service) Restore(ctx context.Context, org, id uuid.UUID, req RestoreRequest, by string) (*domain.VPS, error) {
	v, points, mounted, _, b, err := s.loadReady(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if b == nil || !b.Automated {
		return nil, domain.ErrBackupsUnavailable
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = domain.RestoreFull
	}
	if mode != domain.RestoreFull && mode != domain.RestoreMount {
		return nil, domain.ErrRestorePointNotFound
	}
	found := false
	for _, p := range points {
		if samePoint(p, req.RestorePoint) {
			found, req.RestorePoint = true, p
		}
	}
	if !found {
		return nil, domain.ErrRestorePointNotFound
	}
	if mode == domain.RestoreMount {
		for _, m := range mounted {
			if samePoint(m.RestorePoint, req.RestorePoint) {
				return nil, domain.ErrBusy // déjà monté
			}
		}
	}
	payload, _ := json.Marshal(domain.RestorePayload{RestorePoint: req.RestorePoint, Mode: mode})
	op := domain.OpRestoring
	if mode == domain.RestoreMount {
		op = domain.OpMounting
	}
	return s.enqueueOp(ctx, v, domain.KindRestore, op, payload, by, nil, nil)
}

// Unmount : détache une sauvegarde montée.
func (s *Service) Unmount(ctx context.Context, org, id uuid.UUID, req UnmountRequest, by string) (*domain.VPS, error) {
	v, _, mounted, _, _, err := s.loadReady(ctx, org, id)
	if err != nil {
		return nil, err
	}
	for _, m := range mounted {
		if samePoint(m.RestorePoint, req.RestorePoint) {
			payload, _ := json.Marshal(domain.RestorePayload{RestorePoint: m.RestorePoint, Mode: domain.RestoreMount})
			return s.enqueueOp(ctx, v, domain.KindUnmount, domain.OpUnmounting, payload, by, nil, nil)
		}
	}
	return nil, domain.ErrRestorePointNotFound
}

// CreateSnapshot : un seul snapshot à la fois (option active).
func (s *Service) CreateSnapshot(ctx context.Context, org, id uuid.UUID, req SnapshotRequest, by string) (*domain.VPS, error) {
	v, _, _, snap, _, err := s.loadReady(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if v.SnapshotOption != domain.SnapshotActive {
		return nil, domain.ErrSnapshotNotSubscribed
	}
	if snap != nil {
		return nil, domain.ErrSnapshotExists
	}
	desc := strings.TrimSpace(req.Description)
	if r := []rune(desc); len(r) > 100 {
		desc = string(r[:100])
	}
	payload, _ := json.Marshal(domain.SnapshotPayload{Description: desc})
	return s.enqueueOp(ctx, v, domain.KindSnapshotCreate, domain.OpSnapshotting, payload, by, nil, nil)
}

// RevertSnapshot : le disque revient à l'état du snapshot.
func (s *Service) RevertSnapshot(ctx context.Context, org, id uuid.UUID, by string) (*domain.VPS, error) {
	return s.snapshotOp(ctx, org, id, domain.KindSnapshotRevert, domain.OpReverting, by)
}

func (s *Service) DeleteSnapshot(ctx context.Context, org, id uuid.UUID, by string) (*domain.VPS, error) {
	return s.snapshotOp(ctx, org, id, domain.KindSnapshotDelete, domain.OpDeletingSnapshot, by)
}

func (s *Service) snapshotOp(ctx context.Context, org, id uuid.UUID, kind, op, by string) (*domain.VPS, error) {
	v, _, _, snap, _, err := s.loadReady(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if v.SnapshotOption != domain.SnapshotActive {
		return nil, domain.ErrSnapshotNotSubscribed
	}
	if snap == nil {
		return nil, domain.ErrNoSnapshot
	}
	return s.enqueueOp(ctx, v, kind, op, []byte(`{}`), by, nil, nil)
}

// SubscribeSnapshot : un mois de l'option débité maintenant, commande de
// l'option déposée ; ensuite renouvelée avec le VPS.
func (s *Service) SubscribeSnapshot(ctx context.Context, org, id uuid.UUID, req SubscribeRequest, by string) (*domain.VPS, error) {
	if s.Wallet == nil || s.Catalogs == nil {
		return nil, domain.ErrUnavailable
	}
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if err := ready(v); err != nil {
		return nil, err
	}
	if v.SnapshotOption != "" {
		return nil, domain.ErrBusy
	}
	o, err := s.Store.Offer(ctx, v.Offer)
	if err != nil {
		return nil, err
	}
	rate, err := s.Catalogs.Rate(ctx, org)
	if err != nil {
		return nil, err
	}
	price := o.SnapshotPrice(rate)
	if price <= 0 {
		return nil, domain.ErrSnapshotNotSold
	}
	if req.MonthlyPrice != price {
		return nil, domain.ErrPriceChanged
	}
	period := snapshotPeriod(v.ID, s.now())
	amount := price * domain.MilliPerUnit
	payload, _ := json.Marshal(domain.SubscribePayload{Period: period, AmountMilli: amount})
	out, err := s.enqueueOp(ctx, v, domain.KindSnapshotOrder, domain.OpSubscribing, payload, by,
		map[string]any{"snapshot_option": domain.SnapshotOrdering}, func(tx *gorm.DB) error {
			_, err := s.Wallet.ChargeComputeTx(ctx, tx, org, billingDomain.Milli(amount), period, "Snapshot du VPS "+v.Name+", 1 mois", by)
			return err
		})
	if err == nil {
		s.Wallet.AfterComputeCharge(ctx, org)
	}
	return out, err
}

// UnsubscribeSnapshot : l'option est rendue (le snapshot est supprimé) ; le
// mois entamé n'est pas remboursé.
func (s *Service) UnsubscribeSnapshot(ctx context.Context, org, id uuid.UUID, by string) (*domain.VPS, error) {
	v, err := s.Get(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if err := ready(v); err != nil {
		return nil, err
	}
	if v.SnapshotOption != domain.SnapshotActive {
		return nil, domain.ErrSnapshotNotSubscribed
	}
	return s.enqueueOp(ctx, v, domain.KindSnapshotRelease, domain.OpUnsubscribing, []byte(`{}`), by,
		map[string]any{"snapshot_option": domain.SnapshotReleasing}, nil)
}

// snapshotPeriod : référence d'un mois de l'option.
func snapshotPeriod(vps uuid.UUID, from time.Time) string {
	return domain.Period(vps, from) + ":snapshot"
}

// SaveBackups : relevé du fournisseur → table lue par l'API ; rend vrai si
// le contenu a changé depuis le relevé précédent (la console est prévenue).
func SaveBackups(ctx context.Context, store Store, vps uuid.UUID, pb *ProviderBackups, at time.Time) (bool, error) {
	points := pb.RestorePoints
	if points == nil {
		points = []time.Time{}
	}
	mounted := pb.Mounted
	if mounted == nil {
		mounted = []domain.MountedBackup{}
	}
	rp, _ := json.Marshal(points)
	mo, _ := json.Marshal(mounted)
	var snap json.RawMessage
	if pb.Snapshot != nil {
		snap, _ = json.Marshal(pb.Snapshot)
	}
	prev, err := store.Backups(ctx, vps)
	if err != nil {
		return false, err
	}
	changed := prev == nil || prev.Automated != pb.Automated || !sameJSON(prev.RestorePoints, rp) ||
		!sameJSON(prev.Mounted, mo) || !sameJSON(prev.Snapshot, snap)
	return changed, store.SaveBackups(ctx, &domain.Backups{VPSID: vps, Automated: pb.Automated, RestorePoints: rp, Mounted: mo, Snapshot: snap, RefreshedAt: at})
}

// sameJSON : même valeur JSON (formes différentes tolérées : espaces, ordre des clés, null).
func sameJSON(a, b []byte) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
