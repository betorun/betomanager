package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/copilot/tools"
)

// Couverture du Copilot (lot A1) : chaque route publique de l'API a une
// décision. Soit elle est un outil (marquée ForCopilot dans son câblage, ou
// ci-dessous), soit le Copilot la connaît comme « guide » (il explique et
// envoie vers l'écran de la console, sans l'exécuter : action sensible,
// suppression, envoi de fichier, secret montré une fois, compte de la
// personne), soit elle est hors Copilot (mécanique de connexion, jetons
// techniques). TestCopilotCoverage refuse une route publique sans décision et
// une décision qui ne vise plus aucune route : une fonctionnalité nouvelle ne
// peut pas échapper au Copilot, et il ne peut pas en connaître une disparue.

// CopilotGuide : route connue du Copilot, faite par la personne dans la console.
type CopilotGuide struct {
	// Screen : écran de la console (chemin, voir consoleScreens).
	Screen string
	// Note : précision pour la personne (facultative).
	Note string
}

// copilotDecision : sort d'une route publique pour le Copilot.
type copilotDecision struct {
	tool  *CopilotTool
	guide *CopilotGuide
	out   string
}

func asTool(t CopilotTool) copilotDecision { return copilotDecision{tool: &t} }
func asGuide(screen string, note ...string) copilotDecision {
	return copilotDecision{guide: &CopilotGuide{Screen: screen, Note: strings.Join(note, " ")}}
}
func outOfCopilot(why string) copilotDecision { return copilotDecision{out: why} }

// asStrong : carte sensible (suppression, action sensible, argent, accès) :
// jouée avec la session de la personne, qui confirme avec sa double
// authentification ; target : outil qui lit le nom de la ressource (nom à
// retaper si confirm).
func asStrong(name, target string, confirm bool, hint ...string) copilotDecision {
	return asTool(CopilotTool{Name: name, Write: true, Risk: tools.RiskHigh, Strong: true,
		TargetTool: target, ConfirmName: confirm, Hint: strings.Join(hint, " ")})
}

// consoleScreens : écrans de la console des clients vers lesquels le Copilot
// peut envoyer (kweb, app/(dashboard)). Un écran inconnu fait échouer le test.
var consoleScreens = map[string]consoleScreen{
	"/dashboard":        {"Overview of the current project", "Vue d'ensemble"},
	"/projects":         {"Projects: settings, alert webhooks, deletion", "Projets : réglages, webhooks d'alertes, suppression"},
	"/projects/new":     {"Create a project (choose its region)", "Créer un projet (choix de la région)"},
	"/workloads":        {"Containers: web services, workers, crons", "Conteneurs : services web, workers, crons"},
	"/workloads/new":    {"New container, from an image or a GitHub repository", "Nouveau conteneur, depuis une image ou un dépôt GitHub"},
	"/builds":           {"Builds: images built from Git or an archive", "Builds : images construites depuis Git ou une archive"},
	"/sites":            {"Static sites: upload, deployments, custom domains", "Sites statiques : envoi, déploiements, domaines"},
	"/services":         {"Services: managed Postgres databases and add-on services (Redis, MariaDB…)", "Services : bases Postgres gérées et services (Redis, MariaDB…)"},
	"/network":          {"Network: who can reach whom, internal addresses, dedicated egress IP", "Réseau : qui joint qui, adresses internes, IP de sortie dédiée"},
	"/registries":       {"Private registries: access to external image registries", "Registres privés : accès aux registres d'images externes"},
	"/vps":              {"VPS servers", "Serveurs VPS"},
	"/vps/new":          {"Order a VPS", "Commander un VPS"},
	"/domains":          {"Domain names: buy and attach", "Noms de domaine : achat et branchement"},
	"/access":           {"Access: project members and their roles", "Accès : membres du projet et leurs rôles"},
	"/groups":           {"Groups of the organization", "Groupes de l'organisation"},
	"/roles":            {"Custom roles", "Rôles personnalisés"},
	"/deny-policies":    {"Deny policies", "Politiques de refus"},
	"/service-accounts": {"Service accounts, their API keys, keys and CI trust (OIDC)", "Comptes de service, clés API, clés et confiance CI (OIDC)"},
	"/api-keys":         {"Personal API keys", "Clés API personnelles"},
	"/audit":            {"Audit log", "Journal d'audit"},
	"/team-access":      {"betorun team access: requests, open access, revocation", "Accès de l'équipe betorun : demandes, accès ouverts, révocation"},
	"/billing":          {"Billing: balance, top-up, plan, budget, invoices, billing details", "Facturation : solde, recharge, plan, budget, factures, informations"},
	"/support":          {"Support tickets", "Tickets de support"},
	"/settings":         {"Settings: account security (password, two-factor, passkeys, sessions, linked accounts), e-mail preferences, organization security and company sign-in (SSO)", "Paramètres : sécurité du compte (mot de passe, double authentification, passkeys, sessions, comptes liés), préférences d'e-mail, sécurité de l'organisation et connexion d'entreprise (SSO)"},
	"/status":           {"Platform status and incidents", "État de la plateforme et incidents"},
	"/docs":             {"API documentation", "Documentation de l'API"},
}

// consoleScreen : titre d'un écran (anglais, français).
type consoleScreen struct{ Title, TitleFR string }

// CopilotScreens : écrans de la console, pour la carte des fonctionnalités.
func CopilotScreens() []tools.Screen {
	out := make([]tools.Screen, 0, len(consoleScreens))
	for path, s := range consoleScreens {
		out = append(out, tools.Screen{Path: path, Title: s.Title, TitleFR: s.TitleFR})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// CopilotFeatureInput : guides et descriptions françaises des routes publiques.
func CopilotFeatureInput(groups []*RouteGroup) (map[string]string, []tools.FeatureRoute) {
	fr := map[string]string{}
	walkRoutes(groups, func(full string, r *Route, internal bool) {
		if !internal {
			fr[r.Method+" "+full] = r.Description
		}
	})
	var guides []tools.FeatureRoute
	for _, g := range CopilotGuideRoutes(groups) {
		guides = append(guides, tools.FeatureRoute{Method: g.Method, Path: g.Path, DescriptionFR: g.Description,
			Screen: g.Screen, Note: g.Note, Sensitive: g.Sensitive})
	}
	return fr, guides
}

const (
	outAuth     = "parcours de connexion ou de compte, mené par la console"
	outTechnics = "mécanique technique (jetons, découverte), sans intérêt pour la personne"
)

// copilotDecisions : décisions des routes non marquées dans leur câblage,
// par « MÉTHODE chemin » (chemin sans /kmanager/v1).
var copilotDecisions = map[string]copilotDecision{
	// ── Facturation et prix ────────────────────────────────────────────
	"GET /billing/catalog": asTool(CopilotTool{Name: "get_price_catalog",
		Hint: "The ONLY source for prices: container sizes, databases, disk, builds, egress IP, free tier. Never quote a price that is not in this result."}),
	"GET /billing/plans":                                       asTool(CopilotTool{Name: "list_billing_plans"}),
	"GET /organizations/:id/billing/estimate":                  asTool(CopilotTool{Name: "get_billing_estimate"}),
	"GET /organizations/:id/billing/plan":                      asTool(CopilotTool{Name: "get_billing_plan"}),
	"GET /organizations/:id/billing/profile":                   asTool(CopilotTool{Name: "get_billing_profile"}),
	"GET /organizations/:id/billing/wallet":                    asTool(CopilotTool{Name: "get_wallet"}),
	"GET /organizations/:id/billing/transactions":              asTool(CopilotTool{Name: "list_billing_transactions"}),
	"GET /organizations/:id/billing/documents":                 asTool(CopilotTool{Name: "list_billing_documents"}),
	"GET /organizations/:id/billing/documents/:documentID/pdf": asGuide("/billing", "le PDF se télécharge depuis la liste des factures"),
	"GET /organizations/:id/billing/topups":                    asTool(CopilotTool{Name: "list_topups"}),
	"GET /organizations/:id/billing/topups/:topupID":           asTool(CopilotTool{Name: "get_topup"}),
	"POST /organizations/:id/billing/topups":                   asGuide("/billing"),
	"PUT /organizations/:id/billing/plan":                      asStrong("change_billing_plan", "", false, "Give the plan price from list_billing_plans first."),
	"PUT /organizations/:id/billing/profile":                   asStrong("update_billing_profile", "", false),
	"GET /projects/:id/billing/budget":                         asTool(CopilotTool{Name: "get_project_budget"}),
	"PUT /projects/:id/billing/budget":                         asStrong("set_project_budget", "get_project", false),
	"GET /organizations/:id/legal":                             asTool(CopilotTool{Name: "get_organization_legal"}),
	"POST /organizations/:id/legal/accept":                     asGuide("/billing", "accepter les conditions se fait au moment de payer"),
	"GET /legal/:kind":                                         asTool(CopilotTool{Name: "get_legal_document"}),

	// ── Domaines revendus ──────────────────────────────────────────────
	"GET /organizations/:id/domains":                     asTool(CopilotTool{Name: "list_organization_domains"}),
	"GET /organizations/:id/domains/search":              asTool(CopilotTool{Name: "search_domain"}),
	"GET /organizations/:id/domains/suggestions":         asTool(CopilotTool{Name: "suggest_domains"}),
	"POST /organizations/:id/domains":                    asStrong("buy_domain", "", false, "Paid from the balance: give the yearly price from search_domain first."),
	"POST /organizations/:id/domains/:orderID/attach":    asStrong("attach_domain", "", false),
	"PUT /organizations/:id/domains/:orderID/auto-renew": asStrong("set_domain_auto_renew", "", false),

	// ── VPS ────────────────────────────────────────────────────────────
	"GET /organizations/:id/compute/offers":                           asTool(CopilotTool{Name: "list_vps_offers"}),
	"GET /organizations/:id/compute/vps":                              asTool(CopilotTool{Name: "list_vps"}),
	"GET /organizations/:id/compute/vps/:vpsID":                       asTool(CopilotTool{Name: "get_vps"}),
	"GET /organizations/:id/compute/vps/:vpsID/backups":               asTool(CopilotTool{Name: "list_vps_backups"}),
	"POST /organizations/:id/compute/vps/:vpsID/start":                asTool(CopilotTool{Name: "start_vps", Write: true, Risk: tools.RiskLow, Undo: "stop_vps"}),
	"POST /organizations/:id/compute/vps/:vpsID/stop":                 asTool(CopilotTool{Name: "stop_vps", Write: true, Risk: tools.RiskMedium, Undo: "start_vps"}),
	"POST /organizations/:id/compute/vps/:vpsID/reboot":               asTool(CopilotTool{Name: "reboot_vps", Write: true, Risk: tools.RiskMedium}),
	"POST /organizations/:id/compute/vps/:vpsID/snapshot":             asTool(CopilotTool{Name: "create_vps_snapshot", Write: true, Risk: tools.RiskLow}),
	"PUT /organizations/:id/compute/vps/:vpsID/reverse":               asTool(CopilotTool{Name: "set_vps_reverse_dns", Write: true, Risk: tools.RiskLow}),
	"DELETE /organizations/:id/compute/vps/:vpsID/snapshot":           asStrong("delete_vps_snapshot", "get_vps", false),
	"POST /organizations/:id/compute/vps":                             asGuide("/vps/new", "commande payée par le solde"),
	"POST /organizations/:id/compute/vps/:vpsID/backups/restore":      asStrong("restore_vps_backup", "get_vps", true),
	"POST /organizations/:id/compute/vps/:vpsID/backups/unmount":      asStrong("unmount_vps_backup", "get_vps", false),
	"POST /organizations/:id/compute/vps/:vpsID/reinstall":            asGuide("/vps", "efface le disque"),
	"POST /organizations/:id/compute/vps/:vpsID/rescue":               asStrong("enter_vps_rescue", "get_vps", false),
	"POST /organizations/:id/compute/vps/:vpsID/rescue/exit":          asStrong("exit_vps_rescue", "get_vps", false),
	"POST /organizations/:id/compute/vps/:vpsID/rescue/password":      asGuide("/vps", "mot de passe montré une seule fois"),
	"POST /organizations/:id/compute/vps/:vpsID/snapshot/revert":      asStrong("revert_vps_snapshot", "get_vps", true, "The disk goes back to the snapshot: later changes are lost."),
	"POST /organizations/:id/compute/vps/:vpsID/snapshot/subscribe":   asStrong("subscribe_vps_snapshot", "get_vps", false, "Billed: one month taken from the balance, renewed with the VPS."),
	"POST /organizations/:id/compute/vps/:vpsID/snapshot/unsubscribe": asStrong("unsubscribe_vps_snapshot", "get_vps", false, "The snapshot is deleted; the current month is not refunded."),
	"POST /organizations/:id/compute/vps/:vpsID/terminate":            asStrong("terminate_vps", "get_vps", true, "Cut off at once, no longer renewed, the current month is not refunded."),

	// ── Projets ────────────────────────────────────────────────────────
	"GET /projects/:id":                                 asTool(CopilotTool{Name: "get_project"}),
	"PATCH /projects/:id":                               asTool(CopilotTool{Name: "update_project", Write: true, Risk: tools.RiskLow}),
	"POST /projects":                                    asGuide("/projects/new"),
	"DELETE /projects/:id":                              asStrong("delete_project", "get_project", true, "Stops everything in the project at once; 12 hours later everything is erased for good (containers, databases, services, sites, disks), unless the project is restored before."),
	"POST /projects/:id/restore":                        asTool(CopilotTool{Name: "restore_project", Write: true, Risk: tools.RiskLow}),
	"GET /projects/:id/disk-usage":                      asTool(CopilotTool{Name: "get_project_disk_usage"}),
	"GET /projects/:id/alert-webhooks":                  asTool(CopilotTool{Name: "list_alert_webhooks"}),
	"POST /projects/:id/alert-webhooks":                 asGuide("/projects", "le secret de signature n'est montré qu'une fois"),
	"POST /projects/:id/alert-webhooks/telegram/chats":  asGuide("/projects", "le jeton du bot se colle dans la console, jamais dans la conversation"),
	"POST /projects/:id/alert-webhooks/:webhookID/test": asTool(CopilotTool{Name: "test_alert_webhook", Write: true, Risk: tools.RiskLow}),
	"DELETE /projects/:id/alert-webhooks/:webhookID":    asStrong("delete_alert_webhook", "", false),

	// ── Conteneurs ─────────────────────────────────────────────────────
	"PATCH /workloads/:id": asTool(CopilotTool{Name: "update_workload", Write: true, Risk: tools.RiskMedium,
		Omit: []string{"secret_data", "env_vars", "secrets"},
		Hint: "Change image, size or settings. Environment variables go through update_workload_variables; secrets are never set by you."}),
	"POST /workloads/:id/duplicate":           asTool(CopilotTool{Name: "duplicate_workload", Write: true, Risk: tools.RiskLow}),
	"GET /workloads/:id/disk":                 asTool(CopilotTool{Name: "get_workload_disk_options"}),
	"DELETE /workloads/:id":                   asStrong("delete_workload", "get_workload", true, "The container, its domains and its disk data are deleted."),
	"DELETE /workloads/:id/domains/:domainID": asStrong("remove_workload_domain", "get_workload", false),

	// ── GitHub ─────────────────────────────────────────────────────────
	"PUT /workloads/:id/github":         asTool(CopilotTool{Name: "link_workload_github", Write: true, Risk: tools.RiskLow}),
	"PATCH /workloads/:id/github":       asTool(CopilotTool{Name: "update_workload_github", Write: true, Risk: tools.RiskLow}),
	"POST /workloads/:id/github/deploy": asTool(CopilotTool{Name: "deploy_workload_from_github", Write: true, Risk: tools.RiskLow}),
	"DELETE /workloads/:id/github":      asStrong("unlink_workload_github", "get_workload", false),
	"POST /projects/:id/github/workloads": asTool(CopilotTool{Name: "create_workload_from_github", Write: true, Risk: tools.RiskLow,
		Hint: "One action: container, GitHub link and first build. Needs a GitHub account linked to the project (see the console)."}),
	// Lecture des comptes reliés, dépôts et branches : permission
	// workloads.github.read (une lecture, accordée au jeton délégué en portée
	// « read ») ; base du parcours « mettre en ligne » (propose_launch_plan).
	"GET /projects/:id/github/installations": asTool(CopilotTool{Name: "list_github_accounts",
		Hint: "GitHub accounts linked to the project. None: the person links one on the new container screen (/workloads/new); never invent an account."}),
	"GET /projects/:id/github/installations/:installationID/repositories": asTool(CopilotTool{Name: "list_github_repositories",
		Max: map[string]int{"per_page": 50}, Hint: "Use q to search by name; id and full_name identify a repository."}),
	"GET /projects/:id/github/installations/:installationID/repositories/:repoID/branches": asTool(CopilotTool{Name: "list_github_branches"}),
	"POST /projects/:id/github/installations/url":                                          asGuide("/workloads/new", "relier un compte GitHub se fait sur la page de GitHub"),
	"DELETE /projects/:id/github/installations/:installationID":                            asStrong("unlink_github_account", "", false),
	"GET /integrations/github/status":                                                      outOfCopilot(outTechnics),
	"POST /integrations/github/callback":                                                   outOfCopilot(outAuth),

	// ── Builds ─────────────────────────────────────────────────────────
	"POST /projects/:id/builds/upload":           asGuide("/builds", "envoi d'une archive depuis l'ordinateur"),
	"DELETE /projects/:id/builds/:buildID/image": asStrong("delete_build_image", "", false),

	// ── Sites statiques ────────────────────────────────────────────────
	"POST /projects/:id/sites":                                            asTool(CopilotTool{Name: "create_site", Write: true, Risk: tools.RiskLow}),
	"PATCH /projects/:id/sites/:siteID":                                   asTool(CopilotTool{Name: "update_site", Write: true, Risk: tools.RiskLow}),
	"POST /projects/:id/sites/:siteID/deployments":                        asGuide("/sites", "envoi d'une archive .zip depuis l'ordinateur"),
	"POST /projects/:id/sites/:siteID/deployments/:deploymentID/activate": asTool(CopilotTool{Name: "activate_site_deployment", Write: true, Risk: tools.RiskMedium}),
	"POST /projects/:id/sites/:siteID/domains":                            asTool(CopilotTool{Name: "add_site_domain", Write: true, Risk: tools.RiskLow}),
	"POST /projects/:id/sites/:siteID/domains/:domainID/verify":           asTool(CopilotTool{Name: "verify_site_domain", Write: true, Risk: tools.RiskLow}),
	"DELETE /projects/:id/sites/:siteID":                                  asStrong("delete_site", "get_site", true, "Its files and domains are deleted."),
	"DELETE /projects/:id/sites/:siteID/domains/:domainID":                asStrong("remove_site_domain", "get_site", false),

	// ── Services (addons) ──────────────────────────────────────────────
	"GET /projects/:id/addons/templates":         asTool(CopilotTool{Name: "list_addon_templates"}),
	"GET /projects/:id/addons/:addonID":          asTool(CopilotTool{Name: "get_addon"}),
	"GET /projects/:id/addons/:addonID/backups":  asTool(CopilotTool{Name: "list_addon_backups"}),
	"GET /projects/:id/addons/:addonID/disk":     asTool(CopilotTool{Name: "get_addon_disk_options"}),
	"POST /projects/:id/addons/:addonID/backups": asTool(CopilotTool{Name: "backup_addon", Write: true, Risk: tools.RiskLow}),
	"POST /projects/:id/addons/:addonID/disk":    asTool(CopilotTool{Name: "resize_addon_disk", Write: true, Risk: tools.RiskMedium}),
	"POST /projects/:id/addons/:addonID/upgrade": asTool(CopilotTool{Name: "upgrade_addon", Write: true, Risk: tools.RiskMedium,
		Hint: "The service restarts and can never go back to the previous version."}),
	"POST /projects/:id/addons/:addonID/backups/:backupID/restore": asStrong("restore_addon_backup", "get_addon", true, "Current data is replaced (a copy is taken first)."),
	"DELETE /projects/:id/addons/:addonID":                         asStrong("delete_addon", "get_addon", true, "The service and its disk are deleted; attached containers keep their variable."),
	"GET /addons/waitlist":                                         asGuide("/services"),
	"PUT /addons/waitlist/:template":                               asGuide("/services"),
	"DELETE /addons/waitlist/:template":                            asGuide("/services"),

	// ── Bases de données ───────────────────────────────────────────────
	"GET /projects/:id/databases/:databaseID/imports":           asTool(CopilotTool{Name: "list_database_imports"}),
	"GET /projects/:id/databases/:databaseID/imports/:importID": asTool(CopilotTool{Name: "get_database_import"}),
	"POST /projects/:id/databases/:databaseID/import":           asGuide("/services", "envoi d'un fichier pg_dump depuis l'ordinateur"),
	"POST /projects/:id/databases/:databaseID/import/inspect":   asGuide("/services", "envoi d'un fichier pg_dump depuis l'ordinateur"),
	"POST /projects/:id/databases/:databaseID/reset":            asStrong("reset_database", "get_database", true, "Empties the database (a copy is kept 24 h)."),
	"POST /projects/:id/databases/:databaseID/reset/undo":       asStrong("undo_database_reset", "get_database", false),
	"DELETE /projects/:id/databases/:databaseID":                asStrong("delete_database", "get_database", true, "Its data is lost."),
	// Stockage objet (ADR 0015) : le secret d'une clé ne passe jamais par le Copilot.
	"POST /projects/:id/storage/buckets/:bucketID/keys":          asGuide("/services", "clé d'accès dont le secret n'est montré qu'une fois"),
	"DELETE /projects/:id/storage/buckets/:bucketID/keys/:keyID": asStrong("revoke_bucket_key", "get_bucket", false, "Applications using this key lose access; an attached container has its S3_* variables removed and is redeployed."),
	"DELETE /projects/:id/storage/buckets/:bucketID":             asStrong("delete_bucket", "get_bucket", true, "Its content is lost."),
	// Fichiers : le Copilot liste ; le contenu ne passe pas par lui.
	"GET /projects/:id/storage/buckets/:bucketID/objects/download":    asGuide("/services", "téléchargement d'un fichier, depuis l'explorateur du bucket"),
	"POST /projects/:id/storage/buckets/:bucketID/objects":            asGuide("/services", "envoi d'un fichier, depuis l'explorateur du bucket"),
	"POST /projects/:id/storage/buckets/:bucketID/objects/link":       asGuide("/services", "lien de partage d'un fichier, depuis l'explorateur du bucket"),
	"POST /projects/:id/storage/buckets/:bucketID/objects/delete":     asStrong("delete_bucket_files", "list_bucket_files", true, "The files are lost; a name ending with / deletes the whole folder."),
	"POST /projects/:id/storage/buckets/:bucketID/keys/:keyID/rotate": asGuide("/services", "rotation d'une clé (le nouveau secret n'est montré qu'une fois)"),
	"PUT /projects/:id/storage/buckets/:bucketID/public":              asGuide("/services", "lecture publique d'un bucket, action sensible (double authentification)"),

	// ── Réseau ─────────────────────────────────────────────────────────
	"POST /projects/:id/network/egress": asTool(CopilotTool{Name: "request_egress_ip", Write: true, Risk: tools.RiskMedium,
		Hint: "Billed every month: give the price from get_price_catalog (egress_ip_monthly) before proposing it."}),
	"DELETE /projects/:id/network/egress": asStrong("release_egress_ip", "get_project", false, "The partner allow-listing this IP will refuse the traffic; the IP may go to someone else."),

	// ── Registres externes ─────────────────────────────────────────────
	"GET /projects/:id/registries":             asTool(CopilotTool{Name: "list_registries"}),
	"POST /projects/:id/registries/:host/test": asTool(CopilotTool{Name: "test_registry_access", Write: true, Risk: tools.RiskLow}),
	"PUT /projects/:id/registries/:host":       asGuide("/registries", "le jeton du registre se saisit dans la console, jamais dans la conversation"),
	"DELETE /projects/:id/registries/:host":    asStrong("remove_registry_access", "", false),

	// ── Accès, rôles, groupes, politiques ──────────────────────────────
	"GET /rbac/:projectID/members":                       asTool(CopilotTool{Name: "list_project_members"}),
	"DELETE /rbac/:projectID/members/:userID":            asStrong("remove_project_member", "", false),
	"POST /rbac/assign-role":                             asStrong("assign_project_role", "", false, "Grants access: say exactly who gets which role on which project."),
	"GET /rbac/roles":                                    asGuide("/access"),
	"GET /iam/policy":                                    asTool(CopilotTool{Name: "get_iam_policy", Hint: "Who has which role on a resource (project or organization)."}),
	"POST /iam/policy":                                   asGuide("/access"),
	"GET /iam/auditLogs":                                 asTool(CopilotTool{Name: "list_audit_logs"}),
	"GET /iam/customRoles":                               asTool(CopilotTool{Name: "list_custom_roles"}),
	"GET /iam/customRole":                                asTool(CopilotTool{Name: "get_custom_role"}),
	"POST /iam/customRoles":                              asGuide("/roles"),
	"PATCH /iam/customRole":                              asGuide("/roles"),
	"DELETE /iam/customRole":                             asGuide("/roles"),
	"GET /iam/roles":                                     asGuide("/roles"),
	"GET /iam/permissions":                               asGuide("/roles"),
	"GET /iam/ancestry":                                  outOfCopilot(outTechnics),
	"GET /iam/session":                                   outOfCopilot(outTechnics),
	"POST /iam/testPermissions":                          outOfCopilot(outTechnics),
	"GET /organizations/:id/groups":                      asTool(CopilotTool{Name: "list_groups"}),
	"GET /organizations/:id/groups/:groupID":             asTool(CopilotTool{Name: "get_group"}),
	"GET /organizations/:id/groups/:groupID/members":     asTool(CopilotTool{Name: "list_group_members"}),
	"POST /organizations/:id/groups":                     asTool(CopilotTool{Name: "create_group", Write: true, Risk: tools.RiskLow}),
	"PATCH /organizations/:id/groups/:groupID":           asStrong("update_group", "get_group", false),
	"DELETE /organizations/:id/groups/:groupID":          asStrong("delete_group", "get_group", true, "Its roles everywhere are removed."),
	"POST /organizations/:id/groups/:groupID/members":    asStrong("add_group_member", "get_group", false, "Grants the group's roles: say exactly who."),
	"DELETE /organizations/:id/groups/:groupID/members":  asStrong("remove_group_member", "get_group", false),
	"GET /organizations/:id/denyPolicies":                asTool(CopilotTool{Name: "list_organization_deny_policies"}),
	"GET /organizations/:id/denyPolicies/:policyID":      asTool(CopilotTool{Name: "get_organization_deny_policy"}),
	"POST /organizations/:id/denyPolicies":               asGuide("/deny-policies"),
	"PUT /organizations/:id/denyPolicies/:policyID":      asGuide("/deny-policies"),
	"DELETE /organizations/:id/denyPolicies/:policyID":   asGuide("/deny-policies"),
	"GET /projects/:id/denyPolicies":                     asTool(CopilotTool{Name: "list_project_deny_policies"}),
	"GET /projects/:id/denyPolicies/:policyID":           asTool(CopilotTool{Name: "get_project_deny_policy"}),
	"POST /projects/:id/denyPolicies":                    asGuide("/deny-policies"),
	"PUT /projects/:id/denyPolicies/:policyID":           asGuide("/deny-policies"),
	"DELETE /projects/:id/denyPolicies/:policyID":        asGuide("/deny-policies"),
	"GET /organizations/:id/securityPolicy":              asTool(CopilotTool{Name: "get_security_policy"}),
	"PUT /organizations/:id/securityPolicy":              asGuide("/settings"),
	"GET /organizations/:id/sso":                         asTool(CopilotTool{Name: "get_sso_settings"}),
	"PUT /organizations/:id/sso":                         asGuide("/settings"),
	"DELETE /organizations/:id/sso":                      asGuide("/settings"),
	"POST /organizations/:id/sso/domains":                asGuide("/settings"),
	"POST /organizations/:id/sso/domains/:domain/verify": asGuide("/settings"),
	"DELETE /organizations/:id/sso/domains/:domain":      asGuide("/settings"),

	// ── Comptes de service et clés ─────────────────────────────────────
	"GET /projects/:id/serviceAccounts":                              asTool(CopilotTool{Name: "list_service_accounts"}),
	"GET /projects/:id/serviceAccounts/:saID":                        asTool(CopilotTool{Name: "get_service_account"}),
	"GET /projects/:id/serviceAccounts/:saID/apiKeys":                asTool(CopilotTool{Name: "list_service_account_api_keys"}),
	"GET /projects/:id/serviceAccounts/:saID/keys":                   asTool(CopilotTool{Name: "list_service_account_keys"}),
	"GET /projects/:id/serviceAccounts/:saID/federation":             asTool(CopilotTool{Name: "list_service_account_federation"}),
	"POST /projects/:id/serviceAccounts":                             asTool(CopilotTool{Name: "create_service_account", Write: true, Risk: tools.RiskLow}),
	"POST /projects/:id/serviceAccounts/:saID/apiKeys":               asGuide("/service-accounts", "le secret n'est montré qu'une fois"),
	"POST /projects/:id/serviceAccounts/:saID/keys":                  asGuide("/service-accounts", "la clé privée n'est remise qu'une fois"),
	"POST /projects/:id/serviceAccounts/:saID/federation":            asGuide("/service-accounts"),
	"DELETE /projects/:id/serviceAccounts/:saID":                     asStrong("delete_service_account", "get_service_account", true, "Its keys and roles are deleted."),
	"DELETE /projects/:id/serviceAccounts/:saID/apiKeys/:keyID":      asStrong("revoke_service_account_api_key", "get_service_account", false),
	"DELETE /projects/:id/serviceAccounts/:saID/keys/:keyID":         asStrong("disable_service_account_key", "get_service_account", false),
	"DELETE /projects/:id/serviceAccounts/:saID/federation/:trustID": asStrong("delete_service_account_federation", "get_service_account", false),
	"GET /users/api-keys":                                            asGuide("/api-keys"),
	"GET /users/api-keys/:id":                                        asGuide("/api-keys"),
	"GET /users/api-keys/audit":                                      asGuide("/api-keys"),
	"POST /users/api-keys":                                           asGuide("/api-keys", "le secret n'est montré qu'une fois"),
	"DELETE /users/api-keys/:id":                                     asGuide("/api-keys"),

	// ── Accès d'assistance (ADR 0017) ─────────────────────────────────
	// Lire et retirer : oui ; ouvrir l'organisation à l'équipe : dans la
	// console seulement (double authentification, jamais par un agent).
	"GET /organizations/:id/assistance": asTool(CopilotTool{Name: "list_assistance_access",
		Hint: "betorun team members who asked for or hold temporary access to the organization, with the reason and the end date."}),
	"POST /organizations/:id/assistance/:grantID/approve":  asGuide("/team-access", "double authentification"),
	"POST /organizations/:id/assistance/tickets/:ticketID": asGuide("/team-access", "double authentification"),
	"POST /organizations/:id/assistance/:grantID/deny":     asTool(CopilotTool{Name: "deny_assistance_request", Write: true, Risk: tools.RiskLow}),
	"POST /organizations/:id/assistance/:grantID/revoke":   asTool(CopilotTool{Name: "revoke_assistance_access", Write: true, Risk: tools.RiskLow}),

	// ── Support ────────────────────────────────────────────────────────
	"GET /organizations/:id/support/tickets":           asTool(CopilotTool{Name: "list_support_tickets"}),
	"GET /organizations/:id/support/tickets/:ticketID": asTool(CopilotTool{Name: "get_support_ticket"}),
	"POST /organizations/:id/support/tickets": asTool(CopilotTool{Name: "create_support_ticket", Write: true, Risk: tools.RiskLow,
		Hint: "When the person is stuck on something you cannot do or explain from the tools, offer to open a ticket for them."}),
	"POST /organizations/:id/support/tickets/:ticketID/messages":                 asTool(CopilotTool{Name: "reply_support_ticket", Write: true, Risk: tools.RiskLow}),
	"PATCH /organizations/:id/support/tickets/:ticketID/resolve":                 asTool(CopilotTool{Name: "resolve_support_ticket", Write: true, Risk: tools.RiskLow, Undo: "reopen_support_ticket"}),
	"PATCH /organizations/:id/support/tickets/:ticketID/reopen":                  asTool(CopilotTool{Name: "reopen_support_ticket", Write: true, Risk: tools.RiskLow}),
	"POST /organizations/:id/support/tickets/:ticketID/attachments":              asGuide("/support", "envoi d'un fichier depuis l'ordinateur"),
	"GET /organizations/:id/support/tickets/:ticketID/attachments/:attachmentID": asGuide("/support"),

	// ── Plateforme ─────────────────────────────────────────────────────
	"GET /status": asTool(CopilotTool{Name: "get_platform_status",
		Hint: "Check it first when something fails for no visible reason: an ongoing incident explains it."}),

	// ── Compte de la personne (la console, jamais un agent) ────────────
	"GET /identity/sessions":                        asGuide("/settings"),
	"DELETE /identity/sessions/:id":                 asGuide("/settings"),
	"POST /identity/sessions/revoke-others":         asGuide("/settings"),
	"GET /identity/passkeys":                        asGuide("/settings"),
	"PATCH /identity/passkeys/:id":                  asGuide("/settings"),
	"DELETE /identity/passkeys/:id":                 asGuide("/settings"),
	"POST /identity/passkeys":                       asGuide("/settings"),
	"POST /identity/passkeys/options":               outOfCopilot(outAuth),
	"GET /identity/mfa":                             asGuide("/settings"),
	"POST /identity/mfa/totp":                       asGuide("/settings"),
	"POST /identity/mfa/totp/confirm":               outOfCopilot(outAuth),
	"DELETE /identity/mfa/totp":                     asGuide("/settings"),
	"POST /identity/mfa/recovery-codes":             asGuide("/settings", "codes montrés une seule fois"),
	"GET /identity/identities":                      asGuide("/settings"),
	"DELETE /identity/identities/:provider":         asGuide("/settings"),
	"GET /identity/me/preferences":                  asGuide("/settings"),
	"PATCH /identity/me/preferences":                asGuide("/settings"),
	"POST /identity/change-password":                asGuide("/settings"),
	"POST /identity/forgot-password":                outOfCopilot(outAuth),
	"POST /identity/reset-password":                 outOfCopilot(outAuth),
	"GET /identity/bootstrap/status":                outOfCopilot(outAuth),
	"POST /identity/bootstrap":                      outOfCopilot(outAuth),
	"GET /identity/cli/authorize-url":               outOfCopilot(outAuth),
	"POST /identity/cli/authorize":                  outOfCopilot(outAuth),
	"POST /identity/cli/token":                      outOfCopilot(outAuth),
	"GET /identity/device":                          outOfCopilot(outAuth),
	"POST /identity/device/approve":                 outOfCopilot(outAuth),
	"POST /identity/device/code":                    outOfCopilot(outAuth),
	"POST /identity/device/token":                   outOfCopilot(outAuth),
	"POST /identity/login":                          outOfCopilot(outAuth),
	"POST /identity/login/mfa":                      outOfCopilot(outAuth),
	"POST /identity/login/mfa/passkey":              outOfCopilot(outAuth),
	"POST /identity/login/mfa/passkey/options":      outOfCopilot(outAuth),
	"POST /identity/login/passkey":                  outOfCopilot(outAuth),
	"POST /identity/login/passkey/options":          outOfCopilot(outAuth),
	"POST /identity/login/sso":                      outOfCopilot(outAuth),
	"POST /identity/login/sso/callback":             outOfCopilot(outAuth),
	"POST /identity/logout":                         outOfCopilot(outAuth),
	"POST /identity/refresh":                        outOfCopilot(outAuth),
	"POST /identity/register":                       outOfCopilot(outAuth),
	"POST /identity/resend-verification":            outOfCopilot(outAuth),
	"POST /identity/verify-email":                   outOfCopilot(outAuth),
	"POST /identity/session/verify":                 outOfCopilot(outAuth),
	"POST /identity/session/verify/passkey":         outOfCopilot(outAuth),
	"POST /identity/session/verify/passkey/options": outOfCopilot(outAuth),
	"GET /identity/social/:provider/authorize":      outOfCopilot(outAuth),
	"POST /identity/social/:provider/callback":      outOfCopilot(outAuth),
	"GET /identity/social/providers":                outOfCopilot(outAuth),
	"GET /identity/validate":                        outOfCopilot(outTechnics),
	"GET /iam/.well-known/jwks.json":                outOfCopilot(outTechnics),
	"GET /iam/.well-known/openid-configuration":     outOfCopilot(outTechnics),
	"POST /iam/token":                               outOfCopilot(outTechnics),
	"GET /realtime/subscribe":                       outOfCopilot(outTechnics),
	"GET /realtime/token":                           outOfCopilot(outTechnics),
	"POST /alerts/unsubscribe":                      outOfCopilot(outAuth),
	"POST /alerts/email/confirm":                    outOfCopilot(outAuth),
	"POST /alerts/email/remove":                     outOfCopilot(outAuth),
}

// decisionKey : « MÉTHODE chemin » sans le préfixe de l'API.
func decisionKey(method, fullPath string) string {
	return method + " " + strings.TrimPrefix(fullPath, PathAPIRoot)
}

// walkRoutes : routes de tous les groupes, chemin complet et caractère interne.
func walkRoutes(groups []*RouteGroup, fn func(full string, r *Route, internal bool)) {
	var walk func(g *RouteGroup, abs string, internal bool)
	walk = func(g *RouteGroup, abs string, internal bool) {
		internal = internal || g.internal
		for _, r := range g.Routes {
			fn(buildFullPath(abs, r.Path), r, internal)
		}
		for _, c := range g.Groups {
			walk(c, abs+c.Path, internal)
		}
	}
	for _, g := range groups {
		walk(g, g.Path, false)
	}
}

// applyCopilotDecisions : outils et guides de la table posés sur leurs
// routes (une route déjà marquée dans son câblage garde son outil).
func applyCopilotDecisions(groups []*RouteGroup) {
	walkRoutes(groups, func(full string, r *Route, internal bool) {
		if internal || r.Copilot != nil {
			return
		}
		d, ok := copilotDecisions[decisionKey(r.Method, full)]
		if !ok {
			return
		}
		switch {
		case d.tool != nil:
			t := *d.tool
			r.Copilot = &t
		case d.guide != nil:
			g := *d.guide
			r.CopilotGuide = &g
		}
	})
}

func sensitive(a Access) bool {
	info, _ := iam.Info(a.permission)
	return info.Sensitive
}

// GuideRoute : route connue du Copilot comme guide (carte des fonctionnalités, lot A2).
type GuideRoute struct {
	Method, Path, Description, Screen, Note string
	Sensitive                               bool
}

// CopilotGuideRoutes : routes « guide », triées.
func CopilotGuideRoutes(groups []*RouteGroup) []GuideRoute {
	var out []GuideRoute
	walkRoutes(groups, func(full string, r *Route, internal bool) {
		if internal || r.CopilotGuide == nil {
			return
		}
		out = append(out, GuideRoute{Method: r.Method, Path: full, Description: r.Description,
			Screen: r.CopilotGuide.Screen, Note: r.CopilotGuide.Note, Sensitive: sensitive(r.Access)})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path+out[i].Method < out[j].Path+out[j].Method })
	return out
}

// copilotCoverageProblems : routes publiques sans décision, décisions sans
// route, écrans inconnus, outils sur une route qui refuse le jeton délégué.
func copilotCoverageProblems(groups []*RouteGroup) []string {
	var problems []string
	seen := map[string]bool{}
	walkRoutes(groups, func(full string, r *Route, internal bool) {
		if internal {
			return
		}
		key := decisionKey(r.Method, full)
		d, inTable := copilotDecisions[key]
		if inTable {
			seen[key] = true
		}
		switch {
		case r.Copilot != nil && inTable && d.tool == nil:
			problems = append(problems, key+" : outil dans le câblage mais autre décision dans la table")
		case r.Copilot == nil && r.CopilotGuide == nil && !inTable:
			problems = append(problems, key+" : aucune décision pour le Copilot (outil, guide ou hors Copilot)")
		}
		// Lecture : le jeton délégué de la conversation n'a que la portée
		// « read » ; une permission qui n'est pas une lecture échouerait toujours.
		if r.Copilot != nil && !r.Copilot.Write && r.Access.permission != "" && !r.Access.permission.ReadOnly() {
			problems = append(problems, fmt.Sprintf("%s : outil de lecture sur une permission qui n'en est pas une (%s)", key, r.Access.permission))
		}
		if r.Copilot != nil && r.Access.kind == accessAuthenticated && !r.Access.delegable {
			problems = append(problems, key+" : outil sur une route qui refuse le jeton délégué (Authenticated)")
		}
		if r.CopilotGuide != nil && !knownScreen(r.CopilotGuide.Screen) {
			problems = append(problems, fmt.Sprintf("%s : écran inconnu %q", key, r.CopilotGuide.Screen))
		}
	})
	for key := range copilotDecisions {
		if !seen[key] {
			problems = append(problems, key+" : décision sans route (route renommée ou retirée)")
		}
	}
	sort.Strings(problems)
	return problems
}

func knownScreen(path string) bool {
	_, ok := consoleScreens[path]
	return ok
}

// hiddenCopilotPaths : préfixes de routes (et écrans) masqués aux clients
// par un interrupteur : le Copilot n'en parle pas, comme la console.
func (a *App) hiddenCopilotPaths() []string {
	var out []string
	if !a.Config.Domains.StoreEnabled {
		// Boutique de domaines fermée (console : page en 404, hors du menu).
		out = append(out, PathAPIRoot+"/organizations/:id/domains", "/domains")
	}
	return out
}

// withoutHiddenFeatures : outils, guides et écrans hors des préfixes masqués.
func withoutHiddenFeatures(routes []tools.Route, guides []tools.FeatureRoute, screens []tools.Screen, hidden []string) ([]tools.Route, []tools.FeatureRoute, []tools.Screen) {
	if len(hidden) == 0 {
		return routes, guides, screens
	}
	masked := func(path string) bool {
		for _, h := range hidden {
			if path == h || strings.HasPrefix(path, h+"/") {
				return true
			}
		}
		return false
	}
	var r []tools.Route
	for _, x := range routes {
		if !masked(x.Path) {
			r = append(r, x)
		}
	}
	var g []tools.FeatureRoute
	for _, x := range guides {
		if !masked(x.Path) && !masked(x.Screen) {
			g = append(g, x)
		}
	}
	var sc []tools.Screen
	for _, x := range screens {
		if !masked(x.Path) {
			sc = append(sc, x)
		}
	}
	return r, g, sc
}
