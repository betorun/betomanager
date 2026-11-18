// Package images : taille des images construites, ménage des anciennes
// versions et quota d'images par projet.
package images

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/modules/builds/domain"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/builds/repository"
)

// Réglages par défaut.
const (
	// DefaultKeep : images gardées par dépôt (en plus de celles en ligne).
	DefaultKeep = 5
	// DefaultQuota : octets d'images par projet.
	DefaultQuota int64 = 5 << 30
)

// Registry : opérations sur le registre de la plateforme (registry.Client).
type Registry interface {
	ImageSize(ctx context.Context, project uuid.UUID, repo, digest string) (int64, error)
	Delete(ctx context.Context, project uuid.UUID, repo, digest string) error
}

// Keeper : tient les images d'un projet dans ses limites.
type Keeper struct {
	Store    repository.ImageStore
	Registry Registry
	Host     string // hôte du registre dans les noms d'images
	Keep     int    // DefaultKeep si nul
	Quota    int64  // DefaultQuota si nul ; négatif : pas de quota
	Logger   *zap.SugaredLogger
}

// CheckQuota : domain.ErrImageQuota si les images du projet occupent déjà
// tout le quota.
func (k *Keeper) CheckQuota(ctx context.Context, project uuid.UUID) error {
	quota := k.quota()
	if k == nil || quota < 0 {
		return nil
	}
	used, err := k.Store.Usage(ctx, project)
	if err != nil || used < quota {
		return err
	}
	// Quota atteint : avant de refuser, ne garder par dépôt que la dernière
	// image et celles en ligne.
	if err := k.compact(ctx, project); err != nil {
		return err
	}
	if used, err = k.Store.Usage(ctx, project); err != nil {
		return err
	}
	if used >= quota {
		return domain.ErrImageQuota
	}
	return nil
}

func (k *Keeper) compact(ctx context.Context, project uuid.UUID) error {
	names, err := k.Store.Names(ctx, project)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := k.prune(ctx, project, name, 1); err != nil {
			return err
		}
	}
	return nil
}

// AfterBuild : build réussi. Enregistre la taille de son image puis retire
// du registre les plus anciennes images du même dépôt. Au mieux : un échec
// est journalisé, le build reste réussi.
func (k *Keeper) AfterBuild(ctx context.Context, b *domain.Build) {
	if k == nil || b.Status != domain.StatusSucceeded || b.Digest == "" {
		return
	}
	repo := registry.Repository(k.Host, b.Image)
	if repo == "" {
		return
	}
	size, err := k.Registry.ImageSize(ctx, b.ProjectID, repo, b.Digest)
	if err == nil {
		err = k.Store.SetImageSize(ctx, b.ID, size)
	}
	if err != nil {
		k.log().Warnw("image size not recorded", "build", b.ID, "error", err)
	}
	if err := k.Prune(ctx, b.ProjectID, b.Name); err != nil {
		k.log().Warnw("image pruning failed", "project", b.ProjectID, "name", b.Name, "error", err)
	}
}

// Prune : garde les Keep images les plus récentes du dépôt et celles des
// conteneurs du projet ; retire les autres. Un digest encore porté par une
// image gardée n'est pas supprimé (la suppression vaut pour toutes ses
// étiquettes) : la ligne est seulement marquée.
func (k *Keeper) Prune(ctx context.Context, project uuid.UUID, name string) error {
	return k.prune(ctx, project, name, k.keep())
}

func (k *Keeper) prune(ctx context.Context, project uuid.UUID, name string, keep int) error {
	rows, err := k.Store.Images(ctx, project, name)
	if err != nil || len(rows) <= keep {
		return err
	}
	inUse, err := k.Store.WorkloadImages(ctx, project)
	if err != nil {
		return err
	}
	used := func(b domain.Build) bool {
		for _, img := range inUse {
			if img == b.Image || strings.HasSuffix(img, "@"+b.Digest) {
				return true
			}
		}
		return false
	}
	kept := map[string]bool{}
	var old []domain.Build
	for i, b := range rows {
		// Image d'un autre registre (ancien hôte) : laissée telle quelle.
		if registry.Repository(k.Host, b.Image) == "" {
			continue
		}
		if i < keep || used(b) {
			kept[b.Digest] = true
			continue
		}
		old = append(old, b)
	}
	deleted := map[string]bool{}
	for _, b := range old {
		if !kept[b.Digest] && !deleted[b.Digest] {
			if err := k.Registry.Delete(ctx, project, registry.Repository(k.Host, b.Image), b.Digest); err != nil {
				return err
			}
			deleted[b.Digest] = true
		}
		if err := k.Store.MarkPruned(ctx, b.ID); err != nil {
			return err
		}
	}
	return nil
}

func (k *Keeper) keep() int {
	if k.Keep > 0 {
		return k.Keep
	}
	return DefaultKeep
}

func (k *Keeper) quota() int64 {
	if k == nil {
		return -1
	}
	if k.Quota != 0 {
		return k.Quota
	}
	return DefaultQuota
}

func (k *Keeper) log() *zap.SugaredLogger {
	if k.Logger == nil {
		return zap.NewNop().Sugar()
	}
	return k.Logger
}
