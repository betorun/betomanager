package router

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Carte des fonctionnalités construite depuis la VRAIE spécification :
// questions de débutant (français, sans accents, pluriels), et fonctionnalités
// qui n'existent pas, pour lesquelles la réponse doit être « NO MATCH ».
func TestCopilotFeatureMap(t *testing.T) {
	spec, err := publicSpec(documentedAPI(t).Generator().API(), i18n.EN)
	require.NoError(t, err)
	cat, err := tools.Build(CopilotRoutes(AllGroups), spec)
	require.NoError(t, err)
	require.NoError(t, tools.AddDiagnose(cat))
	added, err := tools.AddLaunchPlan(cat)
	require.NoError(t, err)
	require.True(t, added, "propose_launch_plan : un outil dont il dépend manque au catalogue")
	fr, guides := CopilotFeatureInput(AllGroups)
	require.NoError(t, tools.AddFeatureMap(cat, spec, fr, guides, CopilotScreens()))

	find := func(q string) string {
		args, _ := json.Marshal(map[string]string{"query": q})
		r := cat.Call(context.Background(), tools.Invoker{}, tools.ToolFindFeatures, args)
		require.False(t, r.IsError, r.Content)
		return r.Content
	}

	out := find("créer une base de données")
	assert.Contains(t, out, "action card: create_database")

	out = find("supprimer ma base")
	assert.Contains(t, out, "action card: delete_database (sensitive")
	assert.Contains(t, out, "retype the resource name")

	// Reste à la console : recharge par mobile money (téléphone, prestataire).
	out = find("recharger le solde mobile money")
	assert.Contains(t, out, "done by the person in the console: /billing")
	assert.Contains(t, out, "needs two-factor authentication")

	out = find("reseau ip de sortie")
	assert.Contains(t, out, "request_egress_ip")
	assert.Contains(t, out, "/network")

	out = find("factures")
	assert.Contains(t, out, "list_billing_documents")

	out = find("prix")
	assert.Contains(t, out, "get_price_catalog")

	out = find("deployer depuis github")
	assert.Contains(t, out, "create_workload_from_github")

	out = find("mettre en ligne mon application")
	assert.Contains(t, out, "action card: propose_launch_plan")

	out = find("depots github")
	assert.Contains(t, out, "read tool: list_github_repositories")

	// Les requêtes que le plan construit passent le contrôle du vrai catalogue.
	for tool, args := range map[string]string{
		tools.ToolCreateWorkloadGitHub: `{"id":"p","name":"web-app","installation_id":1,"repo_id":2,"branch":"main","kind":"web","target_port":3000,"context_dir":"app","dockerfile":"Dockerfile"}`,
		tools.ToolCreateDatabase:       `{"id":"p","name":"web-db"}`,
		tools.ToolAttachDatabase:       `{"id":"p","databaseID":"d","workload_id":"w"}`,
		tools.ToolCreateAddon:          `{"id":"p","template":"redis","name":"cache"}`,
		tools.ToolAttachAddon:          `{"id":"p","addonID":"a","workload_id":"w"}`,
		tools.ToolAddWorkloadDomain:    `{"id":"w","hostname":"app.example.com"}`,
	} {
		_, err := cat.Prepare(tool, json.RawMessage(args))
		require.NoError(t, err, tool)
	}

	out = find("mot de passe")
	assert.Contains(t, out, "/settings")

	for _, q := range []string{"serverless lambda", "GPU", "kubernetes helm chart", "machine learning notebook"} {
		assert.True(t, strings.HasPrefix(find(q), "NO MATCH"), q)
	}

	out = find("")
	assert.Contains(t, out, "Areas of betorun")
	assert.Contains(t, out, "/network:")

	// Noyau : chaque outil du noyau existe (un nom faux le ferait disparaître
	// sans bruit), et il reste petit.
	core := cat.Core()
	require.Len(t, core, len(tools.CoreTools))
	require.LessOrEqual(t, len(core), 30)
	// Ce que find_features nomme existe dans le catalogue.
	for _, n := range tools.NamedTools(find("base de donnees conteneur domaine vps facture")) {
		_, ok := cat.Lookup(n)
		require.True(t, ok, n)
	}

	def, ok := cat.Lookup(tools.ToolFindFeatures)
	require.True(t, ok)
	assert.Contains(t, def.Description, "never invent")
}

// Boutique de domaines fermée (domains.store_enabled faux) : le Copilot ne
// connaît ni la recherche ni l'achat ; ouverte, il les propose.
func TestCopilotHiddenDomainStore(t *testing.T) {
	spec, err := publicSpec(documentedAPI(t).Generator().API(), i18n.EN)
	require.NoError(t, err)
	fr, guides := CopilotFeatureInput(AllGroups)
	app := &App{Config: &configs.GlobalConfig{}}
	routes, g, screens := withoutHiddenFeatures(CopilotRoutes(AllGroups), guides, CopilotScreens(), app.hiddenCopilotPaths())
	cat, err := tools.Build(routes, spec)
	require.NoError(t, err)
	require.NoError(t, tools.AddDiagnose(cat))
	require.NoError(t, tools.AddFeatureMap(cat, spec, fr, g, screens))
	for _, name := range []string{"buy_domain", "search_domain", "suggest_domains", "list_organization_domains"} {
		_, ok := cat.Lookup(name)
		assert.False(t, ok, name)
	}
	args, _ := json.Marshal(map[string]string{"query": "acheter un nom de domaine"})
	out := cat.Call(context.Background(), tools.Invoker{}, tools.ToolFindFeatures, args).Content
	assert.NotContains(t, out, "buy_domain")
	assert.NotContains(t, out, "/domains:")
	// Brancher son propre domaine reste possible.
	assert.Contains(t, out, "add_workload_domain")

	app.Config.Domains.StoreEnabled = true
	assert.Empty(t, app.hiddenCopilotPaths())
}
