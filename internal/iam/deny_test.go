package iam

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// denyMemStore : memStore et ses politiques de refus.
type denyMemStore struct {
	*memStore
	denies    []DenyPolicy
	denyCalls int
}

func (s *denyMemStore) DenyPoliciesOn(_ context.Context, rs []ResourceName) ([]DenyPolicy, error) {
	s.denyCalls++
	want := map[ResourceName]bool{}
	for _, r := range rs {
		want[r] = true
	}
	var out []DenyPolicy
	for _, d := range s.denies {
		if want[d.Resource] {
			out = append(out, d)
		}
	}
	return out, nil
}

func denyFixture() (*fixture, *denyMemStore) {
	f := newFixture()
	ds := &denyMemStore{memStore: f.store}
	f.engine = NewEngine(ds, time.Minute)
	return f, ds
}

func TestDeny_BeatsAnyRoleOnDescendants(t *testing.T) {
	f, ds := denyFixture()
	f.grant(OrganizationName(f.org), RoleOwner, f.alice)
	f.grant(OrganizationName(f.org), RoleOwner, f.bob)
	ds.denies = []DenyPolicy{{ID: uuid.New(), Resource: OrganizationName(f.org), DisplayName: "Pas de suppression",
		DeniedPermissions: []string{"resourcemanager.projects.delete", "databases.*"},
		DeniedPrincipals:  []Principal{AllUsers}, ExceptionPrincipals: []Principal{f.bob}}}
	f.engine.Invalidate(AllUsers)

	d, err := f.engine.Check(context.Background(), f.alice, ProjectsDelete, ProjectName(f.project))
	require.NoError(t, err)
	assert.False(t, d.Allowed, "un refus l'emporte sur roles/owner")
	require.NotNil(t, d.DenyPolicy)
	assert.Equal(t, "Pas de suppression", d.DenyPolicy.Name)
	assert.False(t, f.allowed(t, f.alice, DatabasesDelete, ProjectName(f.project).Child("databases", uuid.New())), "motif databases.*")
	assert.True(t, f.allowed(t, f.alice, WorkloadsDelete, ProjectName(f.project)), "le reste n'est pas touché")
	assert.True(t, f.allowed(t, f.bob, ProjectsDelete, ProjectName(f.project)), "exception")
	assert.False(t, f.allowed(t, f.alice, ProjectsDelete, ProjectName(f.otherProject)), "autre organisation : rien d'accordé")
}

func TestDeny_GroupsAndExceptionsByGroup(t *testing.T) {
	f, ds := denyFixture()
	sre := GroupPrincipal(uuid.New())
	f.grant(ProjectName(f.project), RoleEditor, f.alice)
	ds.denies = []DenyPolicy{{ID: uuid.New(), Resource: ProjectName(f.project), DisplayName: "SRE seulement",
		DeniedPermissions: []string{"workloads.workloads.delete"}, DeniedPrincipals: []Principal{AllUsers}, ExceptionPrincipals: []Principal{sre}}}
	f.engine.Invalidate(AllUsers)
	assert.False(t, f.allowed(t, f.alice, WorkloadsDelete, ProjectName(f.project)))
	// Même refus dans TestPermissions (la console grise le bouton).
	got, err := f.engine.TestPermissions(context.Background(), f.alice, ProjectName(f.project), []Permission{WorkloadsDelete, WorkloadsGet})
	require.NoError(t, err)
	assert.Equal(t, []Permission{WorkloadsGet}, got)
}

func TestDeny_ExpiredConditionAndPlatformStaff(t *testing.T) {
	f, ds := denyFixture()
	f.grant(OrganizationName(f.org), RoleOwner, f.alice)
	f.grant(Platform, RolePlatformAdmin, f.staff)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	ds.denies = []DenyPolicy{
		{ID: uuid.New(), Resource: OrganizationName(f.org), DisplayName: "gel terminé", DeniedPermissions: []string{"*"},
			DeniedPrincipals: []Principal{AllUsers}, Condition: &Condition{Title: "fin", Expression: "request.time < timestamp('" + past + "')"}},
		{ID: uuid.New(), Resource: OrganizationName(f.org), DisplayName: "staff", DeniedPermissions: []string{"*"},
			DeniedPrincipals: []Principal{f.staff}},
	}
	f.engine.Invalidate(AllUsers)
	assert.True(t, f.allowed(t, f.alice, ProjectsDelete, ProjectName(f.project)), "politique expirée")
	assert.True(t, f.allowed(t, f.staff, PlatformProjectsSetLimits, Platform), "un accès accordé sur platform n'est pas concerné")
}

func TestDeny_FreshnessAndInvalidation(t *testing.T) {
	f, ds := denyFixture()
	f.grant(ProjectName(f.project), RoleOwner, f.alice)
	pol := DenyPolicy{ID: uuid.New(), Resource: ProjectName(f.project), DisplayName: "gel", DeniedPermissions: []string{"*"}, DeniedPrincipals: []Principal{f.alice}}
	ds.denies = []DenyPolicy{pol}
	assert.False(t, f.allowed(t, f.alice, ProjectsGet, ProjectName(f.project)))

	// Retirée par un autre process : un refus n'est jamais prononcé sur le cache.
	ds.denies = nil
	assert.True(t, f.allowed(t, f.alice, ProjectsGet, ProjectName(f.project)))
	calls := ds.denyCalls
	assert.True(t, f.allowed(t, f.alice, ProjectsGet, ProjectName(f.project)))
	assert.Equal(t, calls, ds.denyCalls, "les accès autorisés restent servis par le cache")

	// Posée : visible après l'invalidation diffusée (AllUsers).
	ds.denies = []DenyPolicy{pol}
	f.engine.Invalidate(AllUsers)
	assert.False(t, f.allowed(t, f.alice, ProjectsGet, ProjectName(f.project)))
}

func TestDeny_PatternsAndValidation(t *testing.T) {
	assert.True(t, permissionMatches("*", WorkloadsGet))
	assert.True(t, permissionMatches("workloads.*", BuildsCreate))
	assert.True(t, permissionMatches("workloads.builds.*", BuildsCreate))
	assert.False(t, permissionMatches("workloads.builds.*", WorkloadsGet))
	assert.False(t, permissionMatches("work.*", WorkloadsGet), "préfixe par segment entier")
	for _, ok := range []string{"*", "databases.*", "iam.groups.*", "sites.sites.delete"} {
		assert.NoError(t, ValidateDenyPermission(ok), ok)
	}
	for _, bad := range []string{"platform.*", "platform.billingCatalogs.update", "nope.*", "sites.sites.explode", ""} {
		assert.ErrorIs(t, ValidateDenyPermission(bad), ErrInvalidDenyPermission, bad)
	}
}

func TestDeny_BrokenRowNeverBlocksItsOwnFix(t *testing.T) {
	d := denyPolicyRow{ID: uuid.New(), ResourceName: "organizations/" + uuid.NewString(), DeniedPermissions: []byte("{bad"),
		DeniedPrincipals: []byte(`["allUsers"]`)}.policy()
	ps := []Principal{UserPrincipal(uuid.New()), AllUsers}
	assert.True(t, d.Denies(ps, ProjectsDelete, time.Now()), "illisible : tout est refusé")
	assert.False(t, d.Denies(ps, DenyPoliciesUpdate, time.Now()), "sauf de quoi la corriger")
}

func (s *denyMemStore) DenyPoliciesInOrganizations(_ context.Context, orgs []uuid.UUID) ([]DenyPolicy, error) {
	var out []DenyPolicy
	for _, d := range s.denies {
		for _, o := range orgs {
			if d.Resource == OrganizationName(o) {
				out = append(out, d)
			}
			if id, ok := d.Resource.Project(); ok && s.orgs[id] == o {
				out = append(out, d)
			}
		}
	}
	return out, nil
}

// La liste « mes projets » suit les refus, comme l'accès.
func TestDeny_Holdings(t *testing.T) {
	f, ds := denyFixture()
	direct := uuid.New()
	f.store.orgs[direct] = f.org
	f.grant(OrganizationName(f.org), RoleViewer, f.alice)
	f.grant(ProjectName(direct), RoleViewer, f.bob)
	ds.denies = []DenyPolicy{{ID: uuid.New(), Resource: ProjectName(f.project), DisplayName: "caché",
		DeniedPermissions: []string{"resourcemanager.projects.get"}, DeniedPrincipals: []Principal{AllUsers}}}

	h, err := f.engine.Holdings(context.Background(), f.alice, ProjectsGet)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{f.org}, h.Organizations)
	assert.Equal(t, []uuid.UUID{f.project}, h.Excluded, "projet refusé retiré malgré l'organisation")

	// Refus posé sur l'organisation : elle disparaît, et un projet accordé directement aussi.
	ds.denies = []DenyPolicy{{ID: uuid.New(), Resource: OrganizationName(f.org), DisplayName: "org",
		DeniedPermissions: []string{"resourcemanager.*"}, DeniedPrincipals: []Principal{AllUsers}}}
	h, err = f.engine.Holdings(context.Background(), f.alice, ProjectsGet)
	require.NoError(t, err)
	assert.Empty(t, h.Organizations)
	h, err = f.engine.Holdings(context.Background(), f.bob, ProjectsGet)
	require.NoError(t, err)
	assert.Empty(t, h.Projects)
}
