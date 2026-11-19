package iam

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestMemberChanges(t *testing.T) {
	r := ProjectName(uuid.New())
	caller, awa, kofi, ama := UserPrincipal(uuid.New()), UserPrincipal(uuid.New()), UserPrincipal(uuid.New()), UserPrincipal(uuid.New())
	sa := ServiceAccountPrincipal(uuid.New())
	before := []Binding{
		{Resource: r, Role: RoleOwner, Principal: caller},
		{Resource: r, Role: RoleViewer, Principal: awa},
		{Resource: r, Role: RoleEditor, Principal: kofi},
		{Resource: r, Role: RoleViewer, Principal: ama},
	}
	after := []Binding{
		{Resource: r, Role: RoleOwner, Principal: caller},
		{Resource: r, Role: RoleEditor, Principal: awa}, // modifié
		{Resource: r, Role: RoleViewer, Principal: ama}, // inchangé
		{Resource: r, Role: RoleViewer, Principal: ama}, // doublon
		{Resource: r, Role: RoleEditor, Principal: sa},  // pas une personne
		{Resource: r, Role: RoleViewer, Principal: UserPrincipal(uuid.Nil)},
	}
	got := map[Principal]MemberChange{}
	for _, c := range memberChanges(caller, before, after) {
		got[c.Principal] = c
	}
	assert.Len(t, got, 3, "awa modifiée, kofi retiré, un nouveau ; ni l'appelant, ni ama, ni le compte de service")
	assert.Equal(t, []string{RoleViewer}, got[awa].Before)
	assert.Equal(t, []string{RoleEditor}, got[awa].After)
	assert.Empty(t, got[kofi].After)
	assert.Empty(t, got[UserPrincipal(uuid.Nil)].Before)

	caller2 := memberChanges(awa, before, after)
	for _, c := range caller2 {
		assert.NotEqual(t, awa, c.Principal, "l'appelant n'est pas prévenu de son propre changement")
	}
}
