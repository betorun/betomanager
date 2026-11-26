// Package repository : VPC, rattachements et jetons d'inscription (Postgres).
package repository

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

// Store : accès Postgres du module vpc.
type Store struct{ DB *gorm.DB }

// Network : VPC d'un projet (nil s'il n'existe pas encore).
func (s *Store) Network(ctx context.Context, project uuid.UUID) (*domain.Network, error) {
	var n domain.Network
	err := s.DB.WithContext(ctx).Where("project_id = ?", project).Take(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// Attachment : rattachement d'un VPS (nil s'il n'y en a pas).
func (s *Store) Attachment(ctx context.Context, vps uuid.UUID) (*domain.Attachment, error) {
	var a domain.Attachment
	err := s.DB.WithContext(ctx).Where("vps_id = ?", vps).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Attachments : VPS rattachés au VPC d'un projet, par adresse.
func (s *Store) Attachments(ctx context.Context, project uuid.UUID) ([]domain.Attachment, error) {
	var out []domain.Attachment
	err := s.DB.WithContext(ctx).Where("project_id = ?", project).Order("address").Find(&out).Error
	return out, err
}

// Attach : VPC du projet créé au besoin (plage libre du cluster), puis
// adresse libre donnée au VPS, dans une seule transaction. Les verrous
// consultatifs sérialisent l'attribution par cluster et par projet.
func (s *Store) Attach(ctx context.Context, project uuid.UUID, cluster string, a *domain.Attachment) (*domain.Network, error) {
	var net domain.Network
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('vpc:project:' || ?::text))`, project.String()).Error; err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&domain.Attachment{}).Where("vps_id = ?", a.VPSID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return domain.ErrAlreadyAttached
		}
		err := tx.Where("project_id = ?", project).Take(&net).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('vpc:cluster:' || ?::text))`, cluster).Error; err != nil {
				return err
			}
			var used []string
			if err := tx.Model(&domain.Network{}).Where("cluster_id = ?", cluster).Pluck("cidr::text", &used).Error; err != nil {
				return err
			}
			p, err := domain.NextRange(parsePrefixes(used))
			if err != nil {
				return err
			}
			net = domain.Network{ProjectID: project, ClusterID: cluster, CIDR: p.String(), CreatedAt: a.CreatedAt}
			if err := tx.Create(&net).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		prefix, err := netip.ParsePrefix(net.CIDR)
		if err != nil {
			return err
		}
		var taken []string
		if err := tx.Model(&domain.Attachment{}).Where("project_id = ?", project).Pluck("host(address)", &taken).Error; err != nil {
			return err
		}
		addr, err := domain.NextAddress(prefix, parseAddrs(taken))
		if err != nil {
			return err
		}
		a.ProjectID, a.Address = project, addr.String()
		return tx.Create(a).Error
	})
	if err != nil {
		return nil, err
	}
	return &net, nil
}

// SetState : rattachement relié ou non (clé publique, date de rattachement).
func (s *Store) SetState(ctx context.Context, vps uuid.UUID, state, publicKey string, at time.Time) error {
	fields := map[string]any{"state": state}
	if state == domain.StateAttached {
		fields["attached_at"] = at
		fields["last_contact_at"] = at
	}
	if publicKey != "" {
		fields["public_key"] = publicKey
	}
	return s.DB.WithContext(ctx).Model(&domain.Attachment{}).Where("vps_id = ?", vps).Updates(fields).Error
}

// Detach : rattachement supprimé (ses jetons avec) ; rend le nombre de VPS
// encore rattachés au VPC.
func (s *Store) Detach(ctx context.Context, vps, project uuid.UUID) (int64, error) {
	var left int64
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("vps_id = ?", vps).Delete(&domain.Attachment{}).Error; err != nil {
			return err
		}
		return tx.Model(&domain.Attachment{}).Where("project_id = ?", project).Count(&left).Error
	})
	return left, err
}

// NewEnrollment : jeton du VPS ; les jetons non utilisés d'avant sont retirés
// (un seul jeton valable à la fois).
func (s *Store) NewEnrollment(ctx context.Context, e *domain.Enrollment) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("vps_id = ? AND used_at IS NULL", e.VPSID).Delete(&domain.Enrollment{}).Error; err != nil {
			return err
		}
		return tx.Create(e).Error
	})
}

// ConsumeEnrollment : jeton valable marqué utilisé, en une seule requête
// (deux inscriptions simultanées : une seule réussit). nil : jeton inconnu,
// expiré ou déjà utilisé.
func (s *Store) ConsumeEnrollment(ctx context.Context, hash []byte, now time.Time) (*domain.Enrollment, error) {
	var out []domain.Enrollment
	res := s.DB.WithContext(ctx).Model(&out).Clauses(clause.Returning{}).
		Where("token_hash = ? AND used_at IS NULL AND expires_at > ?", hash, now).
		Update("used_at", now)
	if res.Error != nil {
		return nil, res.Error
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func parsePrefixes(in []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func parseAddrs(in []string) []netip.Addr {
	out := make([]netip.Addr, 0, len(in))
	for _, s := range in {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// Reset : rattachement gardé (même adresse) mais à refaire par le VPS : clé
// retirée (disque effacé à la réinstallation), en attente d'inscription.
func (s *Store) Reset(ctx context.Context, vps uuid.UUID) error {
	return s.DB.WithContext(ctx).Model(&domain.Attachment{}).Where("vps_id = ?", vps).
		Updates(map[string]any{"state": domain.StatePending, "public_key": "", "attached_at": nil}).Error
}

// SetWanted : choix du client retenu sur le VPS (réseau privé voulu ou non),
// relu à chaque installation.
func (s *Store) SetWanted(ctx context.Context, vps uuid.UUID, wanted bool) error {
	return s.DB.WithContext(ctx).Table("compute_vps").Where("id = ?", vps).Update("private_network", wanted).Error
}

// Networks : tous les VPC (réconciliation).
func (s *Store) Networks(ctx context.Context) ([]domain.Network, error) {
	var out []domain.Network
	return out, s.DB.WithContext(ctx).Order("project_id").Find(&out).Error
}

// Namespace : espace Kubernetes du projet ("" s'il n'existe pas).
func (s *Store) Namespace(ctx context.Context, project uuid.UUID) (string, error) {
	var ns string
	err := s.DB.WithContext(ctx).Table("projects").Where("id = ?", project).Select("namespace").Scan(&ns).Error
	return ns, err
}

// IfEmpty : fn exécutée sous le verrou consultatif du projet (celui de
// Attach), seulement si le projet n'a aucun VPS rattaché.
func (s *Store) IfEmpty(ctx context.Context, project uuid.UUID, fn func() error) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('vpc:project:' || ?::text))`, project.String()).Error; err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&domain.Attachment{}).Where("project_id = ?", project).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		return fn()
	})
}
