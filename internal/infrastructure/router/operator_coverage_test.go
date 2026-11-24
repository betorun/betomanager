package router

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// TestOperatorCoverage : chaque route /admin a une décision pour l'Opérateur,
// et chaque outil est une lecture qu'un jeton délégué en lecture peut faire.
func TestOperatorCoverage(t *testing.T) {
	documentedAPI(t)
	problems := operatorCoverageProblems(AllGroups)
	require.Empty(t, problems, "\n"+strings.Join(problems, "\n"))

	routes, perms := OperatorRoutes(AllGroups)
	cards, out := 0, 0
	for _, d := range operatorDecisions {
		switch {
		case d.card:
			cards++
		case d.out != "":
			out++
		}
	}
	t.Logf("routes /admin : %d outils, %d cartes (lot 3), %d hors Opérateur", len(routes), cards, out)
	for _, r := range routes {
		require.True(t, perms[r.Name].ReadOnly(), r.Name)
		require.True(t, iam.Known(perms[r.Name]), r.Name)
	}
}

// Le catalogue se construit depuis la spécification complète (routes
// internes comprises) : schémas d'arguments, paramètres de chemin.
func TestOperatorCatalogBuilds(t *testing.T) {
	f := documentedAPI(t)
	spec, err := json.Marshal(f.Generator().API())
	require.NoError(t, err)
	routes, _ := OperatorRoutes(AllGroups)
	cat, err := tools.Build(routes, spec)
	require.NoError(t, err)
	defs := cat.Definitions()
	require.Len(t, defs, len(routes))
	var ticket json.RawMessage
	for _, d := range defs {
		require.NotEmpty(t, d.Description, d.Name)
		if d.Name == "get_ticket" {
			ticket = d.Parameters
		}
	}
	require.Contains(t, string(ticket), "ticketID")
}
