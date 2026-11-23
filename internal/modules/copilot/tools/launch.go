package tools

import (
	"encoding/json"
	"fmt"
)

// Plan « mettre en ligne » (lot B) : un outil d'écriture composite. Le modèle
// n'en fournit que les paramètres (projet, dépôt, base, service, domaine) ;
// le serveur (copilot/service) construit les étapes à partir des outils
// existants, les montre sur UNE carte et ne les joue qu'après validation.
const (
	ToolProposeLaunchPlan = "propose_launch_plan"

	ToolListGitHubAccounts     = "list_github_accounts"
	ToolListGitHubRepositories = "list_github_repositories"
	ToolListGitHubBranches     = "list_github_branches"
	ToolCreateWorkloadGitHub   = "create_workload_from_github"
	ToolGetBuild               = "get_build"
	ToolCreateDatabase         = "create_database"
	ToolGetDatabase            = "get_database"
	ToolAttachDatabase         = "attach_database"
	ToolCreateAddon            = "create_addon"
	ToolGetAddon               = "get_addon"
	ToolAttachAddon            = "attach_addon"
	ToolAddWorkloadDomain      = "add_workload_domain"
)

// LaunchPlanDeps : outils-routes dont le plan dépend.
var LaunchPlanDeps = []string{
	ToolListGitHubAccounts, ToolListGitHubRepositories, ToolListGitHubBranches,
	ToolCreateWorkloadGitHub, ToolGetBuild, ToolGetWorkload,
	ToolCreateDatabase, ToolGetDatabase, ToolAttachDatabase,
	ToolCreateAddon, ToolGetAddon, ToolAttachAddon, ToolAddWorkloadDomain,
}

// launchPlanSchema : arguments du plan, stricts (champs inconnus refusés à
// tous les niveaux, aussi au décodage côté serveur).
var launchPlanSchema = json.RawMessage(`{"type":"object","additionalProperties":false,
"required":["project_id","name","github"],
"properties":{
 "project_id":{"type":"string","description":"Project identifier (UUID), from list_projects"},
 "name":{"type":"string","minLength":3,"maxLength":30,"description":"Container name: 3 to 30 lowercase letters, digits and hyphens"},
 "github":{"type":"object","additionalProperties":false,"required":["installation_id","repository","branch"],
  "description":"Repository to deploy, as read with list_github_accounts, list_github_repositories and list_github_branches",
  "properties":{
   "installation_id":{"type":"integer","description":"installation_id of a GitHub account linked to the project (list_github_accounts)"},
   "repository":{"type":"string","description":"Repository full_name (owner/name) or its numeric id, from list_github_repositories"},
   "branch":{"type":"string","description":"Branch to deploy, from list_github_branches"},
   "context_dir":{"type":"string","description":"Sub-directory to build (monorepo), only if read in the repository"},
   "dockerfile":{"type":"string","description":"Dockerfile path, only if read in the repository (otherwise detected automatically)"}}},
 "target_port":{"type":"integer","minimum":1,"maximum":65535,"description":"Port the app listens on, ONLY if read in the repository (Dockerfile EXPOSE, start script); leave it out otherwise"},
 "kind":{"type":"string","enum":["web","worker"],"description":"web (default: HTTP service with a URL) or worker (long-running process without a port)"},
 "database":{"type":"object","additionalProperties":false,"required":["name"],
  "description":"Optional managed Postgres database, created first and attached to the container (DATABASE_URL as a secret)",
  "properties":{"name":{"type":"string","description":"Database name: 3 to 30 lowercase letters, digits and hyphens"}}},
 "addon":{"type":"object","additionalProperties":false,"required":["template","name"],
  "description":"Optional service, created first and attached to the container (connection URL as a secret)",
  "properties":{"template":{"type":"string","enum":["redis","mariadb","meilisearch"]},
   "name":{"type":"string","description":"Service name: 3 to 30 lowercase letters, digits and hyphens"}}},
 "domain":{"type":"object","additionalProperties":false,"required":["hostname"],
  "description":"Optional custom domain for a web container",
  "properties":{"hostname":{"type":"string","description":"Domain, e.g. app.example.com"}}}}}`)

// AddLaunchPlan : outil propose_launch_plan. Il n'est ajouté que si tous les
// outils-routes dont il dépend sont au catalogue (sinon : false, sans erreur ;
// le test de la carte des fonctionnalités vérifie qu'il l'est en vrai).
func AddLaunchPlan(c *Catalog) (bool, error) {
	for _, dep := range LaunchPlanDeps {
		if _, ok := c.tools[dep]; !ok {
			return false, nil
		}
	}
	if !json.Valid(launchPlanSchema) {
		return false, fmt.Errorf("copilot: schéma de %s invalide", ToolProposeLaunchPlan)
	}
	return true, c.Add(&Tool{
		Route: Route{Name: ToolProposeLaunchPlan, Write: true, Risk: RiskMedium},
		Description: "Put an app from a GitHub repository online in ONE card: optional Postgres database and/or service " +
			"(created and awaited first), the container built from the repository, its first build awaited, the database " +
			"and service attached, an optional custom domain, then the container awaited until it runs. Read the repository " +
			"with list_github_accounts, list_github_repositories and list_github_branches first: the server checks them again " +
			"and refuses an unknown account, repository or branch. ACTION: calling this tool does NOT run it; it shows the " +
			"person one card to validate, and the steps then run one after the other on the server. One plan per message.",
		Schema: launchPlanSchema,
	})
}
