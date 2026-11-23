package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func healthOf(state string, alloc, req int64, reasons ...string) *Health {
	return &Health{State: state, CPUAllocMilli: alloc, CPUReqMilli: req, Reasons: reasons}
}

func TestPlace(t *testing.T) {
	cs := []Candidate{
		{ClusterID: "def", Region: "eu-west-gra", State: StateActive, IsDefault: true, Reachable: true, Health: healthOf(HealthOK, 6000, 5000)},
		{ClusterID: "gra-2", Region: "eu-west-gra", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 6000, 1000)},
		{ClusterID: "gra-full", Region: "eu-west-gra", State: StateActive, Reachable: true, Health: healthOf(HealthDegraded, 6000, 100, ReasonMemoryPressure)},
		{ClusterID: "rbx-down", Region: "eu-west-rbx", State: StateActive, Reachable: true, Health: healthOf(HealthDown, 0, 0, ReasonAPIUnreachable)},
		{ClusterID: "sbg-closed", Region: "eu-west-sbg", State: StateClosed, Reachable: true, Health: healthOf(HealthOK, 6000, 0)},
		{ClusterID: "dkr-noaccess", Region: "af-west-dkr", State: StateActive, Health: healthOf(HealthUnknown, 0, 0, ReasonNoAccess)},
	}
	got, err := Place(cs, "")
	assert.NoError(t, err)
	assert.Equal(t, "def", got, "sans région : le défaut s'il peut recevoir")

	got, err = Place(cs, "eu-west-gra")
	assert.NoError(t, err)
	assert.Equal(t, "gra-2", got, "le plus de place libre, jamais un cluster sous pression avant les autres")

	for _, region := range []string{"eu-west-rbx", "eu-west-sbg", "af-west-dkr", "us-east"} {
		_, err = Place(cs, region)
		assert.ErrorIs(t, err, ErrNoClusterInRegion, region)
	}

	// Défaut en panne, sans région : le meilleur des autres.
	cs[0].Health = healthOf(HealthDown, 0, 0, ReasonAPIUnreachable)
	got, _ = Place(cs, "")
	assert.Equal(t, "gra-2", got)

	// Installation à un cluster jamais relevé : le défaut.
	got, _ = Place([]Candidate{{ClusterID: "seul", State: StateActive, IsDefault: true, Reachable: true}}, "")
	assert.Equal(t, "seul", got)
	got, _ = Place(nil, "")
	assert.Equal(t, "", got, "registre vide : cluster de la configuration")
}

func TestPlace_TieBreaks(t *testing.T) {
	cs := []Candidate{
		{ClusterID: "b", Region: "r", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 1000, 500), Projects: 3},
		{ClusterID: "a", Region: "r", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 1000, 500), Projects: 3},
		{ClusterID: "c", Region: "r", State: StateActive, Reachable: true, Health: healthOf(HealthOK, 1000, 500), Projects: 1},
	}
	got, _ := Place(cs, "r")
	assert.Equal(t, "c", got, "à place égale : le moins de projets")
	cs[2].Projects = 3
	got, _ = Place(cs, "r")
	assert.Equal(t, "a", got, "puis l'identifiant")
}

func TestRegions(t *testing.T) {
	rs := Regions([]Candidate{
		{ClusterID: "x", Region: "eu-west-rbx", State: StateActive, Reachable: true, Health: healthOf(HealthDown, 0, 0)},
		{ClusterID: "d", Region: "eu-west-gra", State: StateActive, IsDefault: true, Reachable: true, Health: healthOf(HealthOK, 1, 0)},
		{ClusterID: "s", Region: "eu-west-sbg", State: StateClosed, Reachable: true},
		{ClusterID: "n", State: StateActive, Reachable: true},
	})
	assert.Equal(t, []Region{{Code: "eu-west-gra", Available: true, Default: true}, {Code: "eu-west-rbx"}}, rs)
	assert.True(t, ValidSetting("traefik") && ValidSetting("lb.example.com") && ValidSetting(""))
	assert.False(t, ValidSetting("Traefik Class"))
}
