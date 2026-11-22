// Package organizations : racine de la hiérarchie de ressources
// (docs/architecture/iam.md §3). Chaque utilisateur a une organisation
// personnelle, créée à la première occasion (inscription, premier projet).
package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/organizations/domain"
)

// EnsurePersonal renvoie l'organisation personnelle de l'utilisateur, en la
// créant au besoin avec un binding roles/owner. Idempotent et sûr en
// concurrence (index unique partiel sur owner_user_id).
func EnsurePersonal(ctx context.Context, db *gorm.DB, userID uuid.UUID, displayName string) (uuid.UUID, error) {
	if id, err := findPersonal(ctx, db, userID); err == nil {
		return id, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, err
	}

	if displayName == "" {
		displayName = domain.DefaultPersonalOrgName
	}
	org := domain.Organization{ID: uuid.New(), DisplayName: displayName, Kind: domain.KindPersonal, OwnerUserID: &userID}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&org).Error; err != nil {
			return err
		}
		return iam.AddBinding(ctx, tx, iam.Binding{
			Resource:  iam.OrganizationName(org.ID),
			Role:      iam.RoleOwner,
			Principal: iam.UserPrincipal(userID),
		}, iam.ActorSystemOrganizations)
	})
	if err != nil {
		// Créée entre-temps par une requête concurrente : on la relit.
		if id, findErr := findPersonal(ctx, db, userID); findErr == nil {
			return id, nil
		}
		return uuid.Nil, err
	}
	return org.ID, nil
}

func findPersonal(ctx context.Context, db *gorm.DB, userID uuid.UUID) (uuid.UUID, error) {
	var org domain.Organization
	err := db.WithContext(ctx).
		Where("kind = ? AND owner_user_id = ?", domain.KindPersonal, userID).
		First(&org).Error
	return org.ID, err
}
