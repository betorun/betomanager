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

	routes, perms, _ := OperatorRoutes(AllGroups)
	cards, out := 0, 0
	for _, d := range operatorDecisions {
		switch {
		case d.tool != nil && d.tool.Write:
			cards++
		case d.out != "":
			out++
		}
	}
	t.Logf("routes /admin : %d outils et cartes, dont %d cartes, %d hors Opérateur", len(routes), cards, out)
	for _, r := range routes {
		require.True(t, iam.Known(perms[r.Name]), r.Name)
		// Une lecture : permission de lecture (jeton délégué en lecture) ;
		// une carte : jamais jouée avec ce jeton.
		require.Equal(t, !r.Write, perms[r.Name].ReadOnly(), r.Name)
	}
}

// Le catalogue se construit depuis la spécification complète (routes
// internes comprises) : schémas d'arguments, paramètres de chemin.
func TestOperatorCatalogBuilds(t *testing.T) {
	f := documentedAPI(t)
	spec, err := json.Marshal(f.Generator().API())
	require.NoError(t, err)
	routes, _, _ := OperatorRoutes(AllGroups)
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

	// Une carte sensible pour l'argent, l'accès, un message au client.
	for _, name := range []string{"grant_credit", "reply_to_customer", "terminate_vps", "suspend_vps"} {
		tl, ok := cat.Lookup(name)
		require.True(t, ok, name)
		require.True(t, tl.Write && tl.Strong, name)
	}
	_, err = cat.Prepare("add_ticket_note", json.RawMessage(`{"ticketID":"t-1","body":"x"}`))
	require.NoError(t, err)
}
