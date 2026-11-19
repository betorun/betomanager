package iam_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/iam"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	projectrepo "github.com/thekrauss/kubemanager/internal/modules/projects/repository"
)

// Politiques de refus sur un vrai Postgres : garde-fou contre le
// verrouillage, etag, membres résolus, effet immédiat, nettoyage.
func TestE2E_DenyPolicies(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	engine := iam.NewEngine(&iam.GormStore{DB: db}, iam.DefaultCacheTTL)
	svc := iam.NewDenyPolicyService(db, engine)

	owner := newUser(t, db, "deny-owner@ex.cg")
	ownerP := iam.UserPrincipal(owner)
	project := &authdomain.Project{Name: "deny-" + owner.String()[:8]}
	require.NoError(t, projectrepo.NewProjectRepository(db).CreateProject(ctx, project, owner.String()))
	org, proj := iam.OrganizationName(project.OrganizationID), iam.ProjectName(project.ID)
	dev := newUser(t, db, "deny-dev@ex.cg")
	require.NoError(t, iam.AddBinding(ctx, db, iam.Binding{Resource: org, Role: iam.RoleEditor, Principal: iam.UserPrincipal(dev)}, "test"))
	var devEmail string
	require.NoError(t, db.Raw(`SELECT email FROM users WHERE id = ?`, dev).Row().Scan(&devEmail))

	// ── Garde-fou : jamais tous les propriétaires privés de la gestion des refus ──
	_, err := svc.Create(ctx, ownerP, org, iam.DenyPolicyInput{DisplayName: "tout", DeniedPermissions: []string{"*"},
		DeniedPrincipals: []iam.Principal{iam.AllUsers}})
	assert.ErrorIs(t, err, iam.ErrDenyPolicyLockout)
	_, err = svc.Create(ctx, ownerP, proj, iam.DenyPolicyInput{DisplayName: "projet", DeniedPermissions: []string{"iam.denyPolicies.*"},
		DeniedPrincipals: []iam.Principal{ownerP}})
	assert.ErrorIs(t, err, iam.ErrDenyPolicyLockout, "verrouiller la gestion des refus d'un projet")
	_, err = svc.Create(ctx, ownerP, org, iam.DenyPolicyInput{DisplayName: "x", DeniedPermissions: []string{"platform.*"},
		DeniedPrincipals: []iam.Principal{iam.AllUsers}})
	assert.ErrorIs(t, err, iam.ErrInvalidDenyPermission)
	_, err = svc.Create(ctx, ownerP, proj.Child("sites", uuid.New()), iam.DenyPolicyInput{DisplayName: "x",
		DeniedPermissions: []string{"*"}, DeniedPrincipals: []iam.Principal{iam.AllUsers}})
	assert.ErrorIs(t, err, iam.ErrDenyPolicyParent)

	// ── Une vraie politique : personne ne supprime, sauf le propriétaire ──
	pol, err := svc.Create(ctx, ownerP, org, iam.DenyPolicyInput{DisplayName: "Pas de suppression",
		DeniedPermissions: []string{"resourcemanager.projects.delete", "workloads.workloads.delete"},
		DeniedPrincipals:  []iam.Principal{iam.Principal("user:" + devEmail)}, ExceptionPrincipals: []iam.Principal{ownerP}})
	require.NoError(t, err)
	assert.Equal(t, []iam.Principal{iam.UserPrincipal(dev)}, pol.DeniedPrincipals, "e-mail résolu")
	assert.False(t, allowed(t, engine, dev, iam.WorkloadsDelete, proj), "effet immédiat, héritage org → projet")
	assert.True(t, allowed(t, engine, dev, iam.WorkloadsUpdate, proj))
	assert.True(t, allowed(t, engine, owner, iam.ProjectsDelete, proj))

	// ── Remplacement : etag obligatoire et à jour ──
	in := iam.DenyPolicyInput{DisplayName: "Pas de suppression de conteneurs", DeniedPermissions: []string{"workloads.workloads.delete"},
		DeniedPrincipals: []iam.Principal{iam.UserPrincipal(dev)}}
	_, err = svc.Update(ctx, ownerP, org, pol.ID, "périmé", in)
	assert.ErrorIs(t, err, iam.ErrDenyPolicyEtag)
	upd, err := svc.Update(ctx, ownerP, org, pol.ID, pol.Etag, in)
	require.NoError(t, err)
	assert.NotEqual(t, pol.Etag, upd.Etag)
	assert.Equal(t, pol.CreatedAt.Unix(), upd.CreatedAt.Unix())

	// Le projet voit la politique héritée.
	eff, err := svc.Effective(ctx, proj)
	require.NoError(t, err)
	require.Len(t, eff, 1)
	assert.Equal(t, org, eff[0].Resource)

	// ── La liste « mes projets » suit les refus ──
	hidden, err := svc.Create(ctx, ownerP, proj, iam.DenyPolicyInput{DisplayName: "caché", DeniedPermissions: []string{"resourcemanager.projects.get"},
		DeniedPrincipals: []iam.Principal{iam.UserPrincipal(dev)}})
	require.NoError(t, err)
	held, err := engine.Holdings(ctx, iam.UserPrincipal(dev), iam.ProjectsGet)
	require.NoError(t, err)
	assert.Contains(t, held.Organizations, project.OrganizationID)
	assert.Equal(t, []uuid.UUID{project.ID}, held.Excluded)
	listed, err := projectrepo.NewProjectRepository(db).ListProjects(ctx, held.Organizations, held.Projects, held.Excluded, 100)
	require.NoError(t, err)
	for _, p := range listed {
		assert.NotEqual(t, project.ID, p.ID, "projet refusé absent de la liste")
	}
	require.NoError(t, svc.Delete(ctx, ownerP, proj, hidden.ID))

	// ── Suppression : le droit revient ──
	require.NoError(t, svc.Delete(ctx, ownerP, org, pol.ID))
	assert.ErrorIs(t, svc.Delete(ctx, ownerP, org, pol.ID), iam.ErrDenyPolicyNotFound)
	assert.True(t, allowed(t, engine, dev, iam.WorkloadsDelete, proj))

	// ── Projet supprimé : ses politiques partent avec lui ──
	_, err = svc.Create(ctx, ownerP, proj, iam.DenyPolicyInput{DisplayName: "p", DeniedPermissions: []string{"sites.*"},
		DeniedPrincipals: []iam.Principal{iam.UserPrincipal(dev)}})
	require.NoError(t, err)
	require.NoError(t, iam.DeleteResourcePolicy(ctx, db, proj))
	left, err := svc.List(ctx, proj)
	require.NoError(t, err)
	assert.Empty(t, left)
}
