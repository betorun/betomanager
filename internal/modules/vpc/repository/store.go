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

// RecordTraffic : compteurs relevés sur le réseau d'un projet ; l'écart avec
// le relevé précédent s'ajoute au mois en cours. Un compteur plus bas
// qu'avant (lien refait, passerelle redémarrée) compte depuis zéro.
func (s *Store) RecordTraffic(ctx context.Context, project uuid.UUID, at time.Time, counters map[uuid.UUID]domain.Traffic) error {
	if len(counters) == 0 {
		return nil
	}
	at = at.UTC()
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('vpc:traffic:' || ?::text))`, project.String()).Error; err != nil {
			return err
		}
		var from, to int64
		for vps, c := range counters {
			var last struct{ FromVPSBytes, ToVPSBytes int64 }
			found := tx.Table("vpc_traffic_peers").Select("from_vps_bytes, to_vps_bytes").Where("vps_id = ?", vps).Limit(1).Scan(&last)
			if found.Error != nil {
				return found.Error
			}
			if found.RowsAffected == 0 {
				last.FromVPSBytes, last.ToVPSBytes = 0, 0
			}
			from += delta(c.FromVPS, last.FromVPSBytes)
			to += delta(c.ToVPS, last.ToVPSBytes)
			if err := tx.Exec(`INSERT INTO vpc_traffic_peers (vps_id, project_id, from_vps_bytes, to_vps_bytes, updated_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (vps_id) DO UPDATE SET project_id = EXCLUDED.project_id,
					from_vps_bytes = EXCLUDED.from_vps_bytes, to_vps_bytes = EXCLUDED.to_vps_bytes, updated_at = EXCLUDED.updated_at`,
				vps, project, c.FromVPS, c.ToVPS, at).Error; err != nil {
				return err
			}
		}
		if from == 0 && to == 0 {
			return nil
		}
		return tx.Exec(`INSERT INTO vpc_traffic_monthly (project_id, month, from_vps_bytes, to_vps_bytes, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (project_id, month) DO UPDATE SET
				from_vps_bytes = vpc_traffic_monthly.from_vps_bytes + EXCLUDED.from_vps_bytes,
				to_vps_bytes = vpc_traffic_monthly.to_vps_bytes + EXCLUDED.to_vps_bytes,
				updated_at = EXCLUDED.updated_at`,
			project, month, from, to, at).Error
	})
}

// delta : octets depuis le relevé précédent (compteur remis à zéro : tout
// ce qu'il porte).
func delta(cur, last int64) int64 {
	if cur < 0 {
		return 0
	}
	if cur < last {
		return cur
	}
	return cur - last
}

// Traffic : trafic de chaque projet pour un mois (premier jour, UTC), le
// plus gros d'abord.
func (s *Store) Traffic(ctx context.Context, month time.Time) ([]domain.ProjectTraffic, error) {
	out := []domain.ProjectTraffic{}
	err := s.DB.WithContext(ctx).Raw(`
		SELECT t.project_id, COALESCE(p.name, '') AS project_name,
		       COALESCE(p.organization_id, '00000000-0000-0000-0000-000000000000') AS organization_id,
		       COALESCE(o.display_name, '') AS organization_name,
		       t.from_vps_bytes, t.to_vps_bytes, t.updated_at
		  FROM vpc_traffic_monthly t
		  JOIN projects p ON p.id = t.project_id
		  LEFT JOIN organizations o ON o.id = p.organization_id
		 WHERE t.month = ?
		 ORDER BY t.from_vps_bytes + t.to_vps_bytes DESC, t.project_id`, month.Format("2006-01-02")).Scan(&out).Error
	return out, err
}
