package iam_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/iam"
)

// Politiques d'organisation étendues (phase C) : domaines autorisés, clés
// interdites, journal d'accès aux données.
func TestE2E_OrgPolicyExtended(t *testing.T) {
	db := e2eDB(t)
	ctx := context.Background()
	engine := iam.NewEngine(&iam.GormStore{DB: db}, 0)
	svc := iam.NewOrgPolicyService(db, engine, true)
	policies := &iam.PolicyService{DB: db, Engine: engine, Members: svc}
	groups := &iam.GroupService{DB: db, Engine: engine, Members: svc}
	sas, err := iam.NewServiceAccountService(db, engine, pepper)
	require.NoError(t, err)
	sas.KeyPolicy = svc

	awa := iam.UserPrincipal(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	projectID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	project := iam.ProjectName(projectID)
	ancestry, err := engine.Ancestry(ctx, project)
	require.NoError(t, err)
	orgID, ok := ancestry[1].Organization()
	require.True(t, ok)
	t.Cleanup(func() {
		empty, off := []string{}, false
		_, _ = svc.Update(context.Background(), awa, orgID, iam.OrgPolicyUpdate{AllowedMemberDomains: &empty,
			DisableKeyCreation: &off, DataAccessLogs: &off})
	})

	// ── Domaines : normalisés, validés ──
	bad := []string{"pas un domaine"}
	_, err = svc.Update(ctx, awa, orgID, iam.OrgPolicyUpdate{AllowedMemberDomains: &bad})
	assert.ErrorIs(t, err, iam.ErrInvalidDomain)
	domains := []string{"Ex.SN", "@ex.sn", " corp.ex.sn. "}
	p, err := svc.Update(ctx, awa, orgID, iam.OrgPolicyUpdate{AllowedMemberDomains: &domains})
	require.NoError(t, err)
	assert.Equal(t, []string{"corp.ex.sn", "ex.sn"}, p.AllowedMemberDomains)
	assert.False(t, p.DisableKeyCreation, "les autres contraintes ne bougent pas")

	inside := newUser(t, db, "dev@team.ex.sn") // sous-domaine accepté
	outside := newUser(t, db, "x@gmail.com")
	grant := func(member iam.Principal) error {
		view, err := policies.Get(ctx, project)
		require.NoError(t, err)
		next := view.Policy
		next.Bindings = append(next.Bindings, iam.PolicyBinding{Role: "roles/viewer", Members: []iam.Principal{member}})
		_, err = policies.Set(ctx, awa, project, next)
		return err
	}
	require.NoError(t, grant(iam.UserPrincipal(inside)))
	assert.ErrorIs(t, grant(iam.UserPrincipal(outside)), iam.ErrMemberDomainNotAllowed)
	assert.ErrorIs(t, grant(iam.AllUsers), iam.ErrPublicMemberNotAllowed)
	assert.True(t, allowed(t, engine, inside, iam.ProjectsGet, project))
	assert.False(t, allowed(t, engine, outside, iam.ProjectsGet, project))

	g, err := groups.Create(ctx, awa, orgID, "dom-"+uuid.NewString()[:6], "Domaine", "")
	require.NoError(t, err)
	_, err = groups.AddMember(ctx, awa, orgID, g.ID, iam.UserPrincipal(outside))
	assert.ErrorIs(t, err, iam.ErrMemberDomainNotAllowed, "pas d'entrée par un groupe")
	_, err = groups.AddMember(ctx, awa, orgID, g.ID, iam.UserPrincipal(inside))
	require.NoError(t, err)

	// Domaines retirés : tout le monde de nouveau.
	none := []string{}
	_, err = svc.Update(ctx, awa, orgID, iam.OrgPolicyUpdate{AllowedMemberDomains: &none})
	require.NoError(t, err)
	require.NoError(t, grant(iam.UserPrincipal(outside)))

	// ── Clés de compte de service ──
	sa, err := sas.Create(ctx, awa, projectID, "pol-"+uuid.NewString()[:8], "Politique", "")
	require.NoError(t, err)
	on := true
	_, err = svc.Update(ctx, awa, orgID, iam.OrgPolicyUpdate{DisableKeyCreation: &on})
	require.NoError(t, err)
	_, err = sas.CreateKey(ctx, awa, projectID, sa.ID, iam.CreateKeyRequest{})
	assert.ErrorIs(t, err, iam.ErrKeyCreationDisabled)
	_, err = sas.CreateSAKey(ctx, awa, projectID, sa.ID, iam.CreateSAKeyRequest{})
	assert.ErrorIs(t, err, iam.ErrKeyCreationDisabled)
	off := false
	_, err = svc.Update(ctx, awa, orgID, iam.OrgPolicyUpdate{DisableKeyCreation: &off})
	require.NoError(t, err)
	_, err = sas.CreateKey(ctx, awa, projectID, sa.ID, iam.CreateKeyRequest{})
	require.NoError(t, err)

	// ── Journal d'accès aux données : hérité par les ressources des projets ──
	logged, err := svc.DataAccessLogged(ctx, project.Child("workloads", uuid.New()))
	require.NoError(t, err)
	assert.False(t, logged)
	_, err = svc.Update(ctx, awa, orgID, iam.OrgPolicyUpdate{DataAccessLogs: &on})
	require.NoError(t, err)
	logged, err = svc.DataAccessLogged(ctx, project.Child("workloads", uuid.New()))
	require.NoError(t, err)
	assert.True(t, logged)
}
