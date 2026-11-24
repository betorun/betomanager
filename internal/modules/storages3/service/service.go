package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// Service : cycle de vie des buckets. Backend nil = fonction indisponible.
type Service struct {
	Repo      Repository
	Backend   Backend
	Projects  Projects
	Workloads Workloads
	// Endpoint / Region : adresse S3 donnée aux applications.
	Endpoint string
	Region   string
	// Alerts : alertes de quota (nil : aucune).
	Alerts QuotaAlerts
	Logger *zap.SugaredLogger
}

func (s *Service) available() error {
	if s.Backend == nil || s.Repo == nil {
		return domain.ErrUnavailable
	}
	return nil
}

// Create : bucket S3 au nom généré, puis sa ligne.
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in domain.CreateBucketRequest, creator string) (*domain.BucketView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if !domain.ValidName(name) {
		return nil, domain.ErrInvalidName
	}
	quota := in.QuotaGB
	if quota == 0 {
		quota = domain.DefaultQuotaGB
	}
	if !domain.AllowedQuotaGB[quota] {
		return nil, domain.ErrInvalidQuota
	}
	if err := s.Projects.ProjectReady(ctx, projectID); err != nil {
		return nil, err
	}
	existing, err := s.Repo.ListBuckets(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, b := range existing {
		if b.Name == name {
			return nil, domain.ErrNameTaken
		}
	}
	if len(existing) >= domain.MaxBucketsPerProject {
		return nil, domain.ErrTooMany
	}
	b := &domain.Bucket{ID: uuid.New(), ProjectID: projectID, Name: name, BucketName: domain.NewBucketName(),
		QuotaBytes: int64(quota) * domain.GB, Backend: s.Backend.Name(), Status: domain.StatusReady, CreatedBy: creator}
	if err := s.Backend.CreateBucket(ctx, b.BucketName); err != nil {
		return nil, fmt.Errorf("the bucket could not be created: %w", err)
	}
	if err := s.Repo.CreateBucket(ctx, b); err != nil {
		// Pas de bucket orphelin : on retire celui qu'on vient de créer.
		_ = s.Backend.DeleteBucket(context.WithoutCancel(ctx), b.BucketName, false)
		return nil, err
	}
	v := s.view(ctx, *b, nil)
	return &v, nil
}

// List : buckets du projet et leurs clés.
func (s *Service) List(ctx context.Context, projectID uuid.UUID) ([]domain.BucketView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	list, err := s.Repo.ListBuckets(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.BucketView, 0, len(list))
	for _, b := range list {
		keys, err := s.Repo.ListKeys(ctx, b.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, s.view(ctx, b, keys))
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, projectID, id uuid.UUID) (*domain.BucketView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	b, err := s.Repo.GetBucket(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	keys, err := s.Repo.ListKeys(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	v := s.view(ctx, *b, keys)
	return &v, nil
}

// SetQuota : nouvelle taille maximale ; l'état « quota dépassé » suit au
// relevé suivant (ou tout de suite si le dernier relevé tient dans le quota).
func (s *Service) SetQuota(ctx context.Context, projectID, id uuid.UUID, quotaGB int) (*domain.BucketView, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if !domain.AllowedQuotaGB[quotaGB] {
		return nil, domain.ErrInvalidQuota
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	b.QuotaBytes = int64(quotaGB) * domain.GB
	if err := s.Repo.UpdateBucket(ctx, b); err != nil {
		return nil, err
	}
	if err := s.enforce(ctx, b); err != nil {
		s.Logger.Warnw("storage: quota state not applied", "bucket", b.ID, "error", err)
	}
	return s.Get(ctx, projectID, id)
}

// Delete : clés (variables retirées des conteneurs), bucket S3, puis la
// ligne. Bucket non vide : il faut confirmer par son nom.
func (s *Service) Delete(ctx context.Context, projectID, id uuid.UUID, confirm string) error {
	if err := s.available(); err != nil {
		return err
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return err
	}
	purge := strings.TrimSpace(confirm) == b.Name
	if !purge {
		used, err := s.Backend.Usage(ctx, b.BucketName)
		if err != nil {
			return err
		}
		if used > 0 {
			return domain.ErrNotEmpty
		}
	}
	b.Status = domain.StatusDeleting
	if err := s.Repo.UpdateBucket(ctx, b); err != nil {
		return err
	}
	keys, err := s.Repo.ListKeys(ctx, b.ID)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.dropKey(ctx, k); err != nil {
			return err
		}
	}
	if err := s.Backend.DeleteBucket(ctx, b.BucketName, purge); err != nil {
		return fmt.Errorf("the bucket could not be deleted: %w", err)
	}
	return s.Repo.DeleteBucket(ctx, b.ID)
}

// Attach : une clé propre au conteneur, posée dans ses variables (endpoint,
// région et nom visibles ; identifiants en secrets), puis redéploiement.
func (s *Service) Attach(ctx context.Context, projectID, id, workloadID uuid.UUID, prefix, access, creator string) (*domain.AttachResponse, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if s.Workloads == nil {
		return nil, domain.ErrUnavailable
	}
	if prefix == "" {
		prefix = domain.DefaultPrefix
	}
	if !domain.ValidPrefix(prefix) {
		return nil, domain.ErrInvalidPrefix
	}
	if access == "" {
		access = domain.AccessReadWrite
	}
	if !domain.ValidAccess(access) {
		return nil, domain.ErrInvalidAccess
	}
	owner, err := s.Workloads.ProjectOf(ctx, workloadID)
	if err != nil || owner != projectID {
		return nil, domain.ErrWorkloadNotFound
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	keys, err := s.Repo.ListKeys(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.WorkloadID != nil && *k.WorkloadID == workloadID {
			return nil, domain.ErrAlreadyAttached
		}
	}
	name, _ := s.Workloads.NameOf(ctx, workloadID)
	if name == "" {
		name = workloadID.String()
	}
	wid := workloadID
	k := &domain.Key{ID: uuid.New(), BucketID: b.ID, ProjectID: projectID, Name: name, Access: access,
		Kind: domain.KindAttachment, WorkloadID: &wid, Prefix: prefix, CreatedBy: creator}
	creds, err := s.newIdentity(ctx, b, k)
	if err != nil {
		return nil, err
	}
	endpoint, region, bucket, akVar, skVar := domain.Variables(prefix)
	redeploy, err := s.Workloads.SetVariables(ctx, workloadID,
		map[string]string{endpoint: s.Endpoint, region: s.Region, bucket: b.BucketName},
		map[string]string{akVar: creds.AccessKeyID, skVar: creds.SecretAccessKey}, nil)
	if err != nil {
		s.undoKey(ctx, k)
		return nil, err
	}
	return &domain.AttachResponse{Key: keyView(*k, name), Variables: []string{endpoint, region, bucket, akVar, skVar}, Redeploy: redeploy}, nil
}

// CreateKey : clé manuelle ; son secret n'est rendu qu'ici.
func (s *Service) CreateKey(ctx context.Context, projectID, id uuid.UUID, name, access, creator string) (*domain.CreatedKey, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if !domain.ValidKeyName(name) {
		return nil, domain.ErrInvalidKeyName
	}
	if access == "" {
		access = domain.AccessReadWrite
	}
	if !domain.ValidAccess(access) {
		return nil, domain.ErrInvalidAccess
	}
	b, err := s.ready(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	keys, err := s.Repo.ListKeys(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	manual := 0
	for _, k := range keys {
		if k.Kind == domain.KindManual {
			manual++
		}
	}
	if manual >= domain.MaxManualKeys {
		return nil, domain.ErrTooManyKeys
	}
	k := &domain.Key{ID: uuid.New(), BucketID: b.ID, ProjectID: projectID, Name: name, Access: access,
		Kind: domain.KindManual, CreatedBy: creator}
	creds, err := s.newIdentity(ctx, b, k)
	if err != nil {
		return nil, err
	}
	return &domain.CreatedKey{Key: keyView(*k, ""), SecretAccessKey: creds.SecretAccessKey,
		Endpoint: s.Endpoint, Region: s.Region, Bucket: b.BucketName}, nil
}

// DeleteKey : révoque une clé ; branchement : variables retirées du conteneur.
func (s *Service) DeleteKey(ctx context.Context, projectID, id, keyID uuid.UUID) error {
	if err := s.available(); err != nil {
		return err
	}
	b, err := s.Repo.GetBucket(ctx, projectID, id)
	if err != nil {
		return err
	}
	k, err := s.Repo.GetKey(ctx, b.ID, keyID)
	if err != nil {
		return err
	}
	return s.dropKey(ctx, *k)
}

// ready : bucket du projet, hors suppression et hors délai de grâce.
func (s *Service) ready(ctx context.Context, projectID, id uuid.UUID) (*domain.Bucket, error) {
	b, err := s.Repo.GetBucket(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if b.Status == domain.StatusDeleting {
		return nil, domain.ErrDeleting
	}
	if b.Suspended {
		return nil, domain.ErrSuspended
	}
	return b, nil
}

// access : accès posé sur le S3 pour une clé, selon l'état du bucket.
// Délai de grâce : aucun. Quota dépassé : une clé en écriture peut encore
// lire et supprimer. Sinon, l'accès choisi pour la clé.
func access(b *domain.Bucket, k domain.Key) string {
	switch {
	case b.Suspended:
		return domain.AccessNone
	case k.Access == domain.AccessReadOnly:
		return domain.AccessReadOnly
	case b.OverQuota:
		return domain.AccessReadDelete
	}
	return domain.AccessReadWrite
}

// newIdentity : identité S3 puis ligne de la clé, avec l'accès que l'état
// du bucket permet (quota dépassé : l'écriture revient avec la place).
func (s *Service) newIdentity(ctx context.Context, b *domain.Bucket, k *domain.Key) (Credentials, error) {
	creds, err := s.Backend.CreateIdentity(ctx, k.Identity(), b.BucketName, access(b, *k))
	if err != nil {
		return Credentials{}, fmt.Errorf("the key could not be created: %w", err)
	}
	k.AccessKeyID = creds.AccessKeyID
	if err := s.Repo.CreateKey(ctx, k); err != nil {
		_ = s.Backend.DeleteIdentity(context.WithoutCancel(ctx), k.Identity())
		return Credentials{}, err
	}
	k.CreatedAt = time.Now()
	return creds, nil
}

func (s *Service) undoKey(ctx context.Context, k *domain.Key) {
	ctx = context.WithoutCancel(ctx)
	_ = s.Backend.DeleteIdentity(ctx, k.Identity())
	_ = s.Repo.DeleteKey(ctx, k.ID)
}

// dropKey : variables du conteneur retirées (au mieux : conteneur supprimé
// entre-temps), identité S3 supprimée, puis la ligne.
func (s *Service) dropKey(ctx context.Context, k domain.Key) error {
	if k.WorkloadID != nil && s.Workloads != nil {
		endpoint, region, bucket, akVar, skVar := domain.Variables(k.Prefix)
		if _, err := s.Workloads.SetVariables(ctx, *k.WorkloadID, nil, nil, []string{endpoint, region, bucket, akVar, skVar}); err != nil {
			s.Logger.Warnw("storage: container variables not removed", "key", k.ID, "workload", *k.WorkloadID, "error", err)
		}
	}
	if err := s.Backend.DeleteIdentity(ctx, k.Identity()); err != nil {
		return fmt.Errorf("the key could not be revoked: %w", err)
	}
	return s.Repo.DeleteKey(ctx, k.ID)
}

func (s *Service) view(ctx context.Context, b domain.Bucket, keys []domain.Key) domain.BucketView {
	v := domain.BucketView{ID: b.ID.String(), Name: b.Name, Bucket: b.BucketName, Endpoint: s.Endpoint, Region: s.Region,
		QuotaBytes: b.QuotaBytes, UsedBytes: b.UsedBytes, UsageAt: b.UsageAt, OverQuota: b.OverQuota,
		Public: b.Public, Suspended: b.Suspended, Status: b.Status, CreatedAt: b.CreatedAt,
		Keys: make([]domain.KeyView, 0, len(keys))}
	if b.Public {
		v.PublicURL = strings.TrimRight(s.Endpoint, "/") + "/" + b.BucketName + "/"
	}
	for _, k := range keys {
		name := ""
		if k.WorkloadID != nil && s.Workloads != nil {
			name, _ = s.Workloads.NameOf(ctx, *k.WorkloadID)
		}
		v.Keys = append(v.Keys, keyView(k, name))
	}
	return v
}

func keyView(k domain.Key, workloadName string) domain.KeyView {
	v := domain.KeyView{ID: k.ID.String(), Name: k.Name, AccessKeyID: k.AccessKeyID, Access: k.Access,
		Kind: k.Kind, Prefix: k.Prefix, CreatedAt: k.CreatedAt, RotatedAt: k.RotatedAt, WorkloadName: workloadName}
	if k.WorkloadID != nil {
		v.WorkloadID = k.WorkloadID.String()
	}
	return v
}

// ── Espace utilisé et quota ───────────────────────────────────────────────

// UsageEvery : relevé de l'espace utilisé.
const UsageEvery = 15 * time.Minute

// RefreshUsage : relève chaque bucket, applique le quota (lecture et
// suppression au-delà, accès d'origine rendu en dessous) et prévient à 80 %
// puis au quota atteint.
func (s *Service) RefreshUsage(ctx context.Context) error {
	if err := s.available(); err != nil {
		return err
	}
	list, err := s.Repo.AllBuckets(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range list {
		if err := s.refresh(ctx, &list[i]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", list[i].BucketName, err))
		}
	}
	return errors.Join(errs...)
}

// refresh : relevé d'un bucket, quota appliqué, alerte éventuelle.
func (s *Service) refresh(ctx context.Context, b *domain.Bucket) error {
	used, err := s.Backend.Usage(ctx, b.BucketName)
	if err != nil {
		return err
	}
	now := time.Now()
	b.UsedBytes, b.UsageAt = used, &now
	enforceErr := s.enforce(ctx, b)
	if err := s.Repo.UpdateBucket(ctx, b); err != nil {
		return errors.Join(enforceErr, err)
	}
	s.alert(ctx, b)
	return enforceErr
}

// alert : 80 % puis 100 % du quota (filtré par le module d'alertes : une
// fois par jour et par seuil au plus). Rien pendant le délai de grâce.
func (s *Service) alert(ctx context.Context, b *domain.Bucket) {
	if s.Alerts == nil || b.Suspended || b.QuotaBytes <= 0 {
		return
	}
	level := 0
	switch {
	case b.UsedBytes >= b.QuotaBytes:
		level = 100
	case b.UsedBytes*100 >= b.QuotaBytes*80:
		level = 80
	}
	if level > 0 {
		s.Alerts.BucketQuota(ctx, b.ProjectID, b.ID, level, b.Name, b.UsedBytes, b.QuotaBytes)
	}
}

// enforce : quota dépassé → clés en écriture limitées à lire et supprimer ;
// revenu sous le quota → accès d'origine. Ne change OverQuota qu'une fois
// toutes les clés à jour (un échec sera repris au relevé suivant).
func (s *Service) enforce(ctx context.Context, b *domain.Bucket) error {
	over := b.UsedBytes >= b.QuotaBytes
	if over == b.OverQuota {
		return nil
	}
	next := *b
	next.OverQuota = over
	if err := s.applyAccess(ctx, &next); err != nil {
		return err
	}
	b.OverQuota = over
	return s.Repo.UpdateBucket(ctx, b)
}

// applyAccess : accès de chaque clé posé selon l'état du bucket.
func (s *Service) applyAccess(ctx context.Context, b *domain.Bucket) error {
	keys, err := s.Repo.ListKeys(ctx, b.ID)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.Backend.SetIdentityAccess(ctx, k.Identity(), b.BucketName, access(b, k)); err != nil {
			return err
		}
	}
	return nil
}

// StartUsage : relevé périodique, jusqu'à l'arrêt du contexte.
func (s *Service) StartUsage(ctx context.Context) {
	if s.available() != nil {
		return
	}
	go func() {
		t := time.NewTicker(UsageEvery)
		defer t.Stop()
		for {
			if err := s.RefreshUsage(ctx); err != nil {
				s.Logger.Warnw("storage: usage refresh", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Key : une clé du bucket (contrôle des droits avant une rotation).
func (s *Service) Key(ctx context.Context, projectID, id, keyID uuid.UUID) (*domain.Key, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	b, err := s.Repo.GetBucket(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	return s.Repo.GetKey(ctx, b.ID, keyID)
}
