// Package iam : autorisation de la plateforme (docs/architecture/iam.md).
//
// Une seule question, un seul endroit : Engine.Check(principal, permission,
// ressource). Les handlers ne raisonnent jamais sur des rôles.
//
// Ce package ne dépend d'aucun module métier : les modules dépendent de lui.
package iam

import (
	"sort"
	"strings"
)

// Permission : "<service>.<ressource>.<verbe>". Une permission publiée n'est
// jamais renommée ni supprimée (les rôles personnalisés des clients la
// référencent) ; elle peut être dépréciée.
type Permission string

func (p Permission) String() string { return string(p) }

// Catalogue. Toute permission utilisée par une route doit figurer ici.
const (
	// resourcemanager
	OrganizationsGet          Permission = "resourcemanager.organizations.get"
	OrganizationsUpdate       Permission = "resourcemanager.organizations.update"
	OrganizationsGetIamPolicy Permission = "resourcemanager.organizations.getIamPolicy"
	OrganizationsSetIamPolicy Permission = "resourcemanager.organizations.setIamPolicy"
	ProjectsCreate            Permission = "resourcemanager.projects.create"
	ProjectsGet               Permission = "resourcemanager.projects.get"
	ProjectsList              Permission = "resourcemanager.projects.list"
	ProjectsUpdate            Permission = "resourcemanager.projects.update"
	ProjectsDelete            Permission = "resourcemanager.projects.delete"
	ProjectsUndelete          Permission = "resourcemanager.projects.undelete"
	ProjectsGetIamPolicy      Permission = "resourcemanager.projects.getIamPolicy"
	ProjectsSetIamPolicy      Permission = "resourcemanager.projects.setIamPolicy"

	// workloads
	WorkloadsList           Permission = "workloads.workloads.list"
	WorkloadsGet            Permission = "workloads.workloads.get"
	WorkloadsCreate         Permission = "workloads.workloads.create"
	WorkloadsUpdate         Permission = "workloads.workloads.update"
	WorkloadsDelete         Permission = "workloads.workloads.delete"
	WorkloadsStart          Permission = "workloads.workloads.start"
	WorkloadsStop           Permission = "workloads.workloads.stop"
	WorkloadsRestart        Permission = "workloads.workloads.restart"
	WorkloadsRollback       Permission = "workloads.workloads.rollback"
	WorkloadLogsRead        Permission = "workloads.logs.read"
	WorkloadMetrics         Permission = "workloads.metrics.read"
	WorkloadDomainsList     Permission = "workloads.domains.list"
	WorkloadDomainsUpdate   Permission = "workloads.domains.update"
	WorkloadVariablesList   Permission = "workloads.variables.list"
	WorkloadVariablesUpdate Permission = "workloads.variables.update"
	BuildsList              Permission = "workloads.builds.list"
	BuildsGet               Permission = "workloads.builds.get"
	BuildsCreate            Permission = "workloads.builds.create"
	BuildsCancel            Permission = "workloads.builds.cancel"
	BuildsDelete            Permission = "workloads.builds.delete"
	WorkloadsGitHubManage   Permission = "workloads.github.manage"
	// WorkloadsGitHubRead : voir les comptes GitHub reliés au projet, leurs
	// dépôts et leurs branches (sans rien relier ni déployer). Une lecture :
	// accordée au jeton délégué du Copilot en portée « read ».
	WorkloadsGitHubRead Permission = "workloads.github.read"
	RegistriesList      Permission = "workloads.registries.list"
	RegistriesManage    Permission = "workloads.registries.manage"

	// sites
	SitesList   Permission = "sites.sites.list"
	SitesGet    Permission = "sites.sites.get"
	SitesCreate Permission = "sites.sites.create"
	SitesUpdate Permission = "sites.sites.update"
	SitesDelete Permission = "sites.sites.delete"

	// Bases de données gérées (Postgres).
	DatabasesList   Permission = "databases.databases.list"
	DatabasesGet    Permission = "databases.databases.get"
	DatabasesCreate Permission = "databases.databases.create"
	DatabasesDelete Permission = "databases.databases.delete"
	// DatabasesConnect : brancher une base sur un conteneur (il reçoit l'URI
	// de connexion, mot de passe compris).
	DatabasesConnect Permission = "databases.databases.connect"
	// DatabasesImport : charger un dump dans une base (ses données peuvent
	// être remplacées).
	DatabasesImport Permission = "databases.databases.import"
	// DatabasesReset : vider une base (une copie est gardée 24 h).
	DatabasesReset Permission = "databases.databases.reset"

	// IP de sortie dédiée du projet (ADR 0014) : la voir ; la demander ou la
	// rendre (facturée au mois).
	EgressAddressesGet    Permission = "network.egressAddresses.get"
	EgressAddressesManage Permission = "network.egressAddresses.manage"
	// DatabasesStop : arrêter et relancer une base (son disque est gardé).
	DatabasesStop Permission = "databases.databases.stop"

	// Services additionnels (ADR 0013 : Redis via Valkey…).
	AddonsList   Permission = "addons.instances.list"
	AddonsGet    Permission = "addons.instances.get"
	AddonsCreate Permission = "addons.instances.create"
	// AddonsUpdate : changer de version (le service redémarre ; jamais de
	// retour en arrière).
	AddonsUpdate Permission = "addons.instances.update"
	AddonsDelete Permission = "addons.instances.delete"
	// AddonsRestore : recharger une sauvegarde d'un service (ses données
	// actuelles sont remplacées ; une copie est prise avant).
	AddonsRestore Permission = "addons.instances.restore"
	// AddonsConnect : brancher un service sur un conteneur (il reçoit
	// l'adresse de connexion, mot de passe compris).
	AddonsConnect         Permission = "addons.instances.connect"
	SiteDeploymentsList   Permission = "sites.deployments.list"
	SiteDeploymentsCreate Permission = "sites.deployments.create"
	SiteDeploymentsActive Permission = "sites.deployments.activate"
	SiteDomainsList       Permission = "sites.domains.list"
	SiteDomainsCreate     Permission = "sites.domains.create"
	SiteDomainsVerify     Permission = "sites.domains.verify"
	SiteDomainsDelete     Permission = "sites.domains.delete"

	// iam
	ServiceAccountsList      Permission = "iam.serviceAccounts.list"
	ServiceAccountsGet       Permission = "iam.serviceAccounts.get"
	ServiceAccountsCreate    Permission = "iam.serviceAccounts.create"
	ServiceAccountsUpdate    Permission = "iam.serviceAccounts.update"
	ServiceAccountsDelete    Permission = "iam.serviceAccounts.delete"
	ServiceAccountsActAs     Permission = "iam.serviceAccounts.actAs"
	ServiceAccountKeysList   Permission = "iam.serviceAccountKeys.list"
	ServiceAccountKeysCreate Permission = "iam.serviceAccountKeys.create"
	ServiceAccountKeysDelete Permission = "iam.serviceAccountKeys.delete"
	APIKeysList              Permission = "iam.apiKeys.list"
	APIKeysCreate            Permission = "iam.apiKeys.create"
	APIKeysDelete            Permission = "iam.apiKeys.delete"
	RolesList                Permission = "iam.roles.list"
	RolesCreate              Permission = "iam.roles.create"
	RolesUpdate              Permission = "iam.roles.update"
	RolesDelete              Permission = "iam.roles.delete"
	GroupsList               Permission = "iam.groups.list"
	GroupsGet                Permission = "iam.groups.get"
	GroupsCreate             Permission = "iam.groups.create"
	GroupsUpdate             Permission = "iam.groups.update" // métadonnées et membres
	GroupsDelete             Permission = "iam.groups.delete"
	// Politiques de refus (iam.md §14).
	DenyPoliciesList   Permission = "iam.denyPolicies.list"
	DenyPoliciesUpdate Permission = "iam.denyPolicies.update"

	// compute : VPS revendus (docs/modules/compute.md). La commande, payée par
	// le solde, demande billing.payments.create.
	VPSList      Permission = "compute.vps.list"
	VPSGet       Permission = "compute.vps.get"
	VPSStart     Permission = "compute.vps.start"
	VPSStop      Permission = "compute.vps.stop"
	VPSReboot    Permission = "compute.vps.reboot"
	VPSReinstall Permission = "compute.vps.reinstall"
	VPSRestore   Permission = "compute.vps.restore"
	VPSSnapshot  Permission = "compute.vps.snapshot"
	VPSRescue    Permission = "compute.vps.rescue"
	VPSNetwork   Permission = "compute.vps.network"
	VPSDelete    Permission = "compute.vps.delete"

	// support : tickets de l'organisation (docs/modules/support.md) ; create
	// couvre aussi répondre, joindre un fichier, résoudre et rouvrir.
	SupportTicketsList   Permission = "support.tickets.list"
	SupportTicketsGet    Permission = "support.tickets.get"
	SupportTicketsCreate Permission = "support.tickets.create"

	// billing / logging
	BillingAccountsGet    Permission = "billing.accounts.get"
	BillingAccountsUpdate Permission = "billing.accounts.update"
	BillingPaymentsCreate Permission = "billing.payments.create"
	AuditLogsRead         Permission = "logging.auditLogs.read"

	// platform : réservées au staff betorun, attribuables seulement sur "platform".
	PlatformClustersList Permission = "platform.clusters.list"
	// PlatformClustersManage : déclarer un cluster, déposer son accès,
	// choisir le cluster par défaut, le fermer ou le retirer.
	PlatformClustersManage    Permission = "platform.clusters.manage"
	PlatformClusterKeysList   Permission = "platform.clusterKeys.list"
	PlatformClusterKeysCreate Permission = "platform.clusterKeys.create"
	PlatformClusterKeysRotate Permission = "platform.clusterKeys.rotate"
	PlatformClusterKeysRevoke Permission = "platform.clusterKeys.revoke"
	// PlatformProjectsSetLimits : changer les plafonds de ressources d'un
	// projet (jamais fixés par le client).
	PlatformProjectsSetLimits Permission = "platform.projects.setLimits"
	// PlatformBillingCatalogsGet / Update : grilles de prix de la facturation
	// (jamais fixées dans le code).
	PlatformBillingCatalogsGet    Permission = "platform.billingCatalogs.get"
	PlatformBillingCatalogsUpdate Permission = "platform.billingCatalogs.update"
	// PlatformBillingAccountsGet / Update : comptes de facturation des
	// clients (solde, crédits offerts, ajustements, activation).
	PlatformBillingAccountsGet    Permission = "platform.billingAccounts.get"
	PlatformBillingAccountsUpdate Permission = "platform.billingAccounts.update"
	// PlatformClustersReport : le watcher d'un cluster rend compte de son
	// cluster (enrôlement, configuration, événements des pods).
	PlatformClustersReport Permission = "platform.clusters.report"
	// PlatformMailSuppressionsManage : adresses e-mail mises de côté (rebonds,
	// refus) : les voir et les relâcher.
	PlatformMailSuppressionsManage Permission = "platform.mailSuppressions.manage"
	// PlatformComputeGet / Manage : VPS de tous les clients, relevé du
	// fournisseur et réconciliation (les prix des offres restent sous
	// PlatformBillingCatalogs*).
	PlatformComputeGet    Permission = "platform.compute.get"
	PlatformComputeManage Permission = "platform.compute.manage"
	// PlatformLegalManage : conditions de vente et politique d'usage :
	// brouillons, publication, statistiques d'acceptation.
	PlatformLegalManage Permission = "platform.legal.manage"
	// PlatformStatusManage : incidents et maintenances de la page de statut.
	PlatformStatusManage Permission = "platform.status.manage"
	// PlatformSupportManage : tickets de tous les clients, réponses de
	// l'équipe, réponses types.
	PlatformSupportManage Permission = "platform.support.manage"
)

// PermissionInfo : métadonnées d'une permission (doc, UI, rôles personnalisés).
type PermissionInfo struct {
	Title string
	// CustomRoles : utilisable dans un rôle personnalisé client.
	CustomRoles bool
	// PlatformOnly : ne peut être accordée que sur la ressource "platform".
	PlatformOnly bool
	// Sensitive : action à fort impact (accès, clés, suppression, paiement) ;
	// une personne doit s'être authentifiée récemment avec double
	// authentification (step-up, iam.md §9).
	Sensitive bool
}

var catalog = map[Permission]PermissionInfo{
	OrganizationsGet:          {Title: "Voir une organisation", CustomRoles: true},
	OrganizationsUpdate:       {Title: "Modifier une organisation", CustomRoles: true, Sensitive: true},
	OrganizationsGetIamPolicy: {Title: "Voir les accès d'une organisation", CustomRoles: true},
	OrganizationsSetIamPolicy: {Title: "Modifier les accès d'une organisation", CustomRoles: true, Sensitive: true},
	ProjectsCreate:            {Title: "Créer un projet", CustomRoles: true},
	ProjectsGet:               {Title: "Voir un projet", CustomRoles: true},
	ProjectsList:              {Title: "Lister les projets", CustomRoles: true},
	ProjectsUpdate:            {Title: "Modifier un projet", CustomRoles: true},
	ProjectsDelete:            {Title: "Supprimer un projet", CustomRoles: true, Sensitive: true},
	ProjectsUndelete:          {Title: "Restaurer un projet dont la suppression est programmée", CustomRoles: true},
	ProjectsGetIamPolicy:      {Title: "Voir les accès d'un projet", CustomRoles: true},
	ProjectsSetIamPolicy:      {Title: "Modifier les accès d'un projet", CustomRoles: true, Sensitive: true},

	WorkloadsList:           {Title: "Lister les conteneurs", CustomRoles: true},
	WorkloadsGet:            {Title: "Voir un conteneur", CustomRoles: true},
	WorkloadsCreate:         {Title: "Déployer un conteneur", CustomRoles: true},
	WorkloadsUpdate:         {Title: "Modifier un conteneur", CustomRoles: true},
	WorkloadsDelete:         {Title: "Supprimer un conteneur", CustomRoles: true},
	WorkloadsStart:          {Title: "Démarrer un conteneur", CustomRoles: true},
	WorkloadsStop:           {Title: "Arrêter un conteneur", CustomRoles: true},
	WorkloadsRestart:        {Title: "Redémarrer un conteneur", CustomRoles: true},
	WorkloadsRollback:       {Title: "Revenir à une version d'un conteneur", CustomRoles: true},
	WorkloadLogsRead:        {Title: "Lire les logs d'un conteneur", CustomRoles: true},
	WorkloadDomainsList:     {Title: "Voir les domaines d'un conteneur", CustomRoles: true},
	WorkloadDomainsUpdate:   {Title: "Modifier les domaines d'un conteneur", CustomRoles: true},
	WorkloadVariablesList:   {Title: "Voir les variables d'un conteneur (jamais la valeur des secrets)", CustomRoles: true},
	WorkloadVariablesUpdate: {Title: "Modifier les variables et les secrets d'un conteneur", CustomRoles: true},
	BuildsList:              {Title: "Lister les builds", CustomRoles: true},
	BuildsGet:               {Title: "Voir un build et son journal", CustomRoles: true},
	BuildsCreate:            {Title: "Construire une image", CustomRoles: true},
	BuildsCancel:            {Title: "Annuler un build", CustomRoles: true},
	BuildsDelete:            {Title: "Supprimer une image construite", CustomRoles: true},
	WorkloadsGitHubManage:   {Title: "Gérer la connexion GitHub et les dépôts liés aux conteneurs", CustomRoles: true},
	WorkloadsGitHubRead:     {Title: "Voir les comptes GitHub reliés au projet, leurs dépôts et leurs branches", CustomRoles: true},
	RegistriesList:          {Title: "Voir les registres d'images externes du projet", CustomRoles: true},
	RegistriesManage:        {Title: "Gérer les accès aux registres d'images externes", CustomRoles: true},
	WorkloadMetrics:         {Title: "Voir les métriques d'un conteneur", CustomRoles: true},

	SitesList:             {Title: "Lister les sites", CustomRoles: true},
	SitesGet:              {Title: "Voir un site", CustomRoles: true},
	SitesCreate:           {Title: "Créer un site", CustomRoles: true},
	DatabasesList:         {Title: "Lister les bases de données", CustomRoles: true},
	DatabasesGet:          {Title: "Voir une base de données", CustomRoles: true},
	DatabasesCreate:       {Title: "Créer une base de données", CustomRoles: true},
	DatabasesDelete:       {Title: "Supprimer une base de données (ses données sont perdues)", CustomRoles: true, Sensitive: true},
	DatabasesConnect:      {Title: "Brancher une base de données sur un conteneur", CustomRoles: true},
	DatabasesImport:       {Title: "Importer un dump dans une base de données (ses données peuvent être remplacées)", CustomRoles: true, Sensitive: true},
	DatabasesReset:        {Title: "Vider une base de données (copie de sécurité gardée 24 h)", CustomRoles: true, Sensitive: true},
	EgressAddressesGet:    {Title: "Voir l'IP de sortie dédiée du projet", CustomRoles: true},
	EgressAddressesManage: {Title: "Demander ou rendre l'IP de sortie dédiée du projet (facturée)", CustomRoles: true},
	DatabasesStop:         {Title: "Arrêter et relancer une base de données (ses données sont gardées)", CustomRoles: true},
	AddonsList:            {Title: "Lister les services d'un projet", CustomRoles: true},
	AddonsGet:             {Title: "Voir un service", CustomRoles: true},
	AddonsCreate:          {Title: "Ajouter un service (Redis, MariaDB, Meilisearch…)", CustomRoles: true},
	AddonsUpdate:          {Title: "Mettre à jour la version d'un service", CustomRoles: true},
	AddonsDelete:          {Title: "Supprimer un service (ses données sont perdues)", CustomRoles: true, Sensitive: true},
	AddonsRestore:         {Title: "Restaurer la sauvegarde d'un service (ses données actuelles sont remplacées)", CustomRoles: true, Sensitive: true},
	AddonsConnect:         {Title: "Brancher un service sur un conteneur", CustomRoles: true},
	SitesUpdate:           {Title: "Modifier un site", CustomRoles: true},
	SitesDelete:           {Title: "Supprimer un site", CustomRoles: true},
	SiteDeploymentsList:   {Title: "Voir les déploiements d'un site", CustomRoles: true},
	SiteDeploymentsCreate: {Title: "Déployer un site", CustomRoles: true},
	SiteDeploymentsActive: {Title: "Activer une version d'un site", CustomRoles: true},
	SiteDomainsList:       {Title: "Voir les domaines d'un site", CustomRoles: true},
	SiteDomainsCreate:     {Title: "Ajouter un domaine", CustomRoles: true},
	SiteDomainsVerify:     {Title: "Vérifier un domaine", CustomRoles: true},
	SiteDomainsDelete:     {Title: "Retirer un domaine", CustomRoles: true},

	ServiceAccountsList:      {Title: "Lister les comptes de service", CustomRoles: true},
	ServiceAccountsGet:       {Title: "Voir un compte de service", CustomRoles: true},
	ServiceAccountsCreate:    {Title: "Créer un compte de service", CustomRoles: true},
	ServiceAccountsUpdate:    {Title: "Modifier un compte de service", CustomRoles: true},
	ServiceAccountsDelete:    {Title: "Supprimer un compte de service", CustomRoles: true, Sensitive: true},
	ServiceAccountsActAs:     {Title: "Agir en tant que compte de service", CustomRoles: true},
	ServiceAccountKeysList:   {Title: "Lister les clés d'un compte de service", CustomRoles: true},
	ServiceAccountKeysCreate: {Title: "Créer une clé de compte de service", CustomRoles: true, Sensitive: true},
	ServiceAccountKeysDelete: {Title: "Supprimer une clé de compte de service", CustomRoles: true},
	APIKeysList:              {Title: "Lister les clés API", CustomRoles: true},
	APIKeysCreate:            {Title: "Créer une clé API", CustomRoles: true, Sensitive: true},
	APIKeysDelete:            {Title: "Révoquer une clé API", CustomRoles: true},
	RolesList:                {Title: "Lister les rôles", CustomRoles: true},
	RolesCreate:              {Title: "Créer un rôle personnalisé", CustomRoles: true, Sensitive: true},
	RolesUpdate:              {Title: "Modifier un rôle personnalisé", CustomRoles: true, Sensitive: true},
	RolesDelete:              {Title: "Supprimer un rôle personnalisé", CustomRoles: true, Sensitive: true},
	GroupsList:               {Title: "Lister les groupes", CustomRoles: true},
	GroupsGet:                {Title: "Voir un groupe et ses membres", CustomRoles: true},
	GroupsCreate:             {Title: "Créer un groupe", CustomRoles: true},
	GroupsUpdate:             {Title: "Modifier un groupe et ses membres", CustomRoles: true, Sensitive: true},
	GroupsDelete:             {Title: "Supprimer un groupe", CustomRoles: true, Sensitive: true},
	DenyPoliciesList:         {Title: "Voir les politiques de refus", CustomRoles: true},
	DenyPoliciesUpdate:       {Title: "Créer, modifier et supprimer les politiques de refus", CustomRoles: true, Sensitive: true},

	BillingAccountsGet:    {Title: "Voir la facturation", CustomRoles: true},
	BillingAccountsUpdate: {Title: "Modifier la facturation", CustomRoles: true, Sensitive: true},
	BillingPaymentsCreate: {Title: "Effectuer un paiement", CustomRoles: true, Sensitive: true},
	AuditLogsRead:         {Title: "Lire le journal d'audit", CustomRoles: true},

	VPSList:      {Title: "Lister les VPS", CustomRoles: true},
	VPSGet:       {Title: "Voir un VPS", CustomRoles: true},
	VPSStart:     {Title: "Démarrer un VPS", CustomRoles: true},
	VPSStop:      {Title: "Arrêter un VPS", CustomRoles: true},
	VPSReboot:    {Title: "Redémarrer un VPS", CustomRoles: true},
	VPSReinstall: {Title: "Réinstaller un VPS (efface son disque)", CustomRoles: true, Sensitive: true},
	VPSRestore:   {Title: "Restaurer un VPS depuis une sauvegarde ou son snapshot", CustomRoles: true, Sensitive: true},
	VPSSnapshot:  {Title: "Créer ou supprimer le snapshot d'un VPS", CustomRoles: true},
	VPSRescue:    {Title: "Mode secours d'un VPS et son mot de passe", CustomRoles: true, Sensitive: true},
	VPSNetwork:   {Title: "Régler le DNS inverse d'un VPS", CustomRoles: true},
	VPSDelete:    {Title: "Résilier un VPS", CustomRoles: true, Sensitive: true},

	SupportTicketsList:   {Title: "Lister les tickets de support", CustomRoles: true},
	SupportTicketsGet:    {Title: "Voir un ticket de support", CustomRoles: true},
	SupportTicketsCreate: {Title: "Ouvrir un ticket de support et y répondre", CustomRoles: true},

	PlatformClustersList:           {Title: "Lister les clusters de la plateforme", PlatformOnly: true},
	PlatformClustersManage:         {Title: "Gérer les clusters de la plateforme : déclaration, accès, cluster par défaut, fermeture", PlatformOnly: true, Sensitive: true},
	PlatformClusterKeysList:        {Title: "Lister les clés de cluster", PlatformOnly: true},
	PlatformClusterKeysCreate:      {Title: "Créer une clé de cluster", PlatformOnly: true, Sensitive: true},
	PlatformClusterKeysRotate:      {Title: "Faire tourner une clé de cluster", PlatformOnly: true, Sensitive: true},
	PlatformClusterKeysRevoke:      {Title: "Révoquer une clé de cluster", PlatformOnly: true, Sensitive: true},
	PlatformClustersReport:         {Title: "Rendre compte de son cluster (watcher)", PlatformOnly: true},
	PlatformProjectsSetLimits:      {Title: "Changer les limites de ressources d'un projet", PlatformOnly: true, Sensitive: true},
	PlatformBillingCatalogsGet:     {Title: "Voir les grilles de prix", PlatformOnly: true},
	PlatformBillingCatalogsUpdate:  {Title: "Modifier les grilles de prix", PlatformOnly: true, Sensitive: true},
	PlatformBillingAccountsGet:     {Title: "Voir les comptes de facturation des clients", PlatformOnly: true},
	PlatformBillingAccountsUpdate:  {Title: "Offrir un crédit, corriger un solde, activer la facturation", PlatformOnly: true, Sensitive: true},
	PlatformMailSuppressionsManage: {Title: "Voir et relâcher les adresses e-mail mises de côté", PlatformOnly: true},
	PlatformComputeGet:             {Title: "Voir les VPS de tous les clients et la réconciliation", PlatformOnly: true},
	PlatformComputeManage:          {Title: "Gérer les VPS des clients : abus, port 25, résiliation, rattachement, réconciliation", PlatformOnly: true, Sensitive: true},
	PlatformLegalManage:            {Title: "Rédiger et publier les conditions de vente et la politique d'usage", PlatformOnly: true},
	PlatformStatusManage:           {Title: "Annoncer les incidents et les maintenances sur la page de statut", PlatformOnly: true},
	PlatformSupportManage:          {Title: "Traiter les tickets de support de tous les clients", PlatformOnly: true},
}

// Known : la permission figure au catalogue.
func Known(p Permission) bool {
	_, ok := catalog[p]
	return ok
}

// Info : métadonnées d'une permission connue.
func Info(p Permission) (PermissionInfo, bool) {
	i, ok := catalog[p]
	return i, ok
}

// AllPermissions : catalogue trié (API /iam/v1/permissions, doc, UI).
func AllPermissions() []Permission {
	out := make([]Permission, 0, len(catalog))
	for p := range catalog {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ReadOnly : la permission ne fait que lire (verbe get, list, read ou
// getIamPolicy). Sert à borner un jeton délégué en lecture (sts.ScopeRead).
func (p Permission) ReadOnly() bool {
	s := string(p)
	verb := s[strings.LastIndexByte(s, '.')+1:]
	switch verb {
	case "get", "list", "read", "getIamPolicy":
		return true
	}
	return false
}
