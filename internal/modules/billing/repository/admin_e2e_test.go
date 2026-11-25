package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/billing/repository"
)

// Recherche du back-office : l'identifiant d'un projet trouve son
// organisation (le staff a souvent l'un sous la main, pas l'autre) ; le
// plafond de projets est rendu.
func TestE2E_AdminSearchOrgs(t *testing.T) {
	db, org, project := e2e(t)
	ctx := context.Background()
	admin := repository.NewAdmin(db)
	require.NoError(t, db.Exec(`UPDATE organizations SET max_projects = 7 WHERE id = ?`, org).Error)

	for _, q := range []string{project.String(), strings.ToUpper(project.String()), org.String()} {
		rows, err := admin.SearchOrgs(ctx, q, 20)
		require.NoError(t, err)
		require.Len(t, rows, 1, q)
		require.Equal(t, org, rows[0].ID)
		require.Equal(t, 7, rows[0].MaxProjects)
		require.Equal(t, 1, rows[0].Projects)
	}
	// Un morceau d'identifiant de projet ne trouve rien (correspondance exacte).
	rows, err := admin.SearchOrgs(ctx, project.String()[:8]+"-zz", 20)
	require.NoError(t, err)
	require.Empty(t, rows)
}
