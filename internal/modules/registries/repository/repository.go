// Package repository : accès chiffrés aux registres externes.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
)

// Sealer : chiffrement authentifié (auth.Sealer).
type Sealer interface {
	Seal(plain []byte, associated string) ([]byte, error)
	Open(sealed []byte, associated string) ([]byte, error)
}

// Store : accès d'un projet à ses registres externes.
type Store interface {
	// List : vue de l'API, sans jeton.
	List(ctx context.Context, project uuid.UUID) ([]domain.View, error)
	// Put : crée ou remplace l'accès à un hôte ; renvoie la vue enregistrée.
	Put(ctx context.Context, project uuid.UUID, host, username, password string, by *uuid.UUID) (domain.View, error)
	// Delete : domain.ErrNotFound si absent.
	Delete(ctx context.Context, project uuid.UUID, host string) error
	// Has : le projet a-t-il un accès à cet hôte ?
	Has(ctx context.Context, project uuid.UUID, host string) (bool, error)
	// Auths : identifiants en clair (worker seulement) ; host vide : tous.
	Auths(ctx context.Context, project uuid.UUID, host string) ([]domain.Auth, error)
}

type gormStore struct {
	db     *gorm.DB
	sealer Sealer
}

// New : stockage en base.
func New(db *gorm.DB, sealer Sealer) Store {
	return &gormStore{db: db, sealer: sealer}
}

// associated : lie le chiffré à sa ligne et à son hôte. Recopié sur une autre
// ligne ou un autre hôte, il ne s'ouvre plus.
func associated(id uuid.UUID, host string) string {
	return "registry-credential|" + id.String() + "|" + host
}

func (s *gormStore) List(ctx context.Context, project uuid.UUID) ([]domain.View, error) {
	var rows []domain.Credential
	if err := s.db.WithContext(ctx).Where("project_id = ?", project).Order("host").Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]domain.View, 0, len(rows))
	for _, r := range rows {
		views = append(views, domain.View{Host: r.Host, Username: r.Username, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return views, nil
}

func (s *gormStore) Put(ctx context.Context, project uuid.UUID, host, username, password string, by *uuid.UUID) (domain.View, error) {
	var view domain.View
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row domain.Credential
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND host = ?", project, host).First(&row).Error
		creating := errors.Is(err, gorm.ErrRecordNotFound)
		if err != nil && !creating {
			return err
		}
		now := time.Now()
		if creating {
			row = domain.Credential{ID: uuid.New(), ProjectID: project, Host: host, CreatedBy: by, CreatedAt: now}
		}
		sealed, err := s.sealer.Seal([]byte(password), associated(row.ID, host))
		if err != nil {
			return err
		}
		row.Username, row.SealedPassword, row.UpdatedAt = username, sealed, now
		view = domain.View{Host: host, Username: username, CreatedAt: row.CreatedAt, UpdatedAt: now}
		if creating {
			// Deux créations simultanées : la seconde remplace la première.
			return tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "project_id"}, {Name: "host"}},
				DoUpdates: clause.AssignmentColumns([]string{"username", "sealed_password", "updated_at", "id"}),
			}).Create(&row).Error
		}
		return tx.Save(&row).Error
	})
	return view, err
}

func (s *gormStore) Delete(ctx context.Context, project uuid.UUID, host string) error {
	res := s.db.WithContext(ctx).Where("project_id = ? AND host = ?", project, host).Delete(&domain.Credential{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *gormStore) Has(ctx context.Context, project uuid.UUID, host string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&domain.Credential{}).Where("project_id = ? AND host = ?", project, host).Count(&n).Error
	return n > 0, err
}

func (s *gormStore) Auths(ctx context.Context, project uuid.UUID, host string) ([]domain.Auth, error) {
	q := s.db.WithContext(ctx).Where("project_id = ?", project)
	if host != "" {
		q = q.Where("host = ?", host)
	}
	var rows []domain.Credential
	if err := q.Order("host").Find(&rows).Error; err != nil {
		return nil, err
	}
	auths := make([]domain.Auth, 0, len(rows))
	for _, r := range rows {
		plain, err := s.sealer.Open(r.SealedPassword, associated(r.ID, r.Host))
		if err != nil {
			return nil, err
		}
		auths = append(auths, domain.Auth{Host: r.Host, Username: r.Username, Password: string(plain)})
	}
	return auths, nil
}
