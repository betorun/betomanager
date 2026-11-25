package keycloak

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Pays de l'inscription : posé en attribut « country », relu normalisé.
func TestCountryAttribute(t *testing.T) {
	assert.Equal(t, map[string][]string{"full_name": {"Awa Diop"}, "country": {"GA"}}, buildAttributes("Awa Diop", "", "GA"))
	assert.Nil(t, buildAttributes("", "", ""))

	p := mapUserProfile(userRepresentation{ID: "kc-1", Email: "Awa@Betorun.test", FirstName: "Awa",
		Attributes: map[string][]string{"country": {" ga "}}})
	assert.Equal(t, "GA", p.Country)
	assert.Equal(t, "awa@betorun.test", p.Email)
	assert.Empty(t, mapUserProfile(userRepresentation{ID: "kc-2"}).Country)
}
