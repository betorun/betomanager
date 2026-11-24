package iam

import (
	"sort"
	"strings"
)

// Role : ensemble nommé de permissions. Les rôles prédéfinis vivent dans le
// code (versionnés avec lui) ; les rôles personnalisés vivront en DB (phase B).
type Role struct {
	Name        string       `json:"name"` // "roles/editor"
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Permissions []Permission `json:"permissions"`
	// PlatformOnly : rôle du staff betorun, attribuable seulement sur "platform".
	PlatformOnly bool `json:"-"`
}

func (r Role) has(p Permission) bool {
	for _, x := range r.Permissions {
		if x == p {
			return true
		}
	}
	return false
}

var (
	viewerPerms = []Permission{
		OrganizationsGet, OrganizationsGetIamPolicy,
		ProjectsGet, ProjectsList, ProjectsGetIamPolicy,
		WorkloadsList, WorkloadsGet, WorkloadLogsRead, WorkloadMetrics, WorkloadVariablesList, WorkloadDomainsList, BuildsList, BuildsGet,
		SitesList, SitesGet, SiteDeploymentsList, SiteDomainsList,
		DatabasesList, DatabasesGet,
		StorageBucketsList, StorageBucketsGet, StorageObjectsList,
		EgressAddressesGet,
		AddonsList, AddonsGet,
		VPSList, VPSGet,
		SupportTicketsList, SupportTicketsGet,
		ServiceAccountsList, ServiceAccountsGet, ServiceAccountKeysList, APIKeysList,
		RolesList, GroupsList, GroupsGet,
		// Savoir pourquoi une action est refusée.
		DenyPoliciesList,
	}

	// developerPerms : parité exacte avec l'ancien rôle DEVELOPER (création et
	// cycle de vie, sans suppression) pour qu'aucune migration n'élargisse un droit.
	developerPerms = concat(viewerPerms, []Permission{
		WorkloadsCreate, WorkloadsUpdate, WorkloadsStart, WorkloadsStop,
		WorkloadsRestart, WorkloadsRollback, WorkloadVariablesUpdate, WorkloadDomainsUpdate, BuildsCreate, BuildsCancel,
		RegistriesList,
		DatabasesConnect, AddonsConnect,
		// Lire et envoyer des fichiers, comme une application le ferait.
		StorageObjectsGet, StorageObjectsCreate,
		SitesCreate, SitesUpdate, SiteDeploymentsCreate, SiteDeploymentsActive,
		SiteDomainsCreate, SiteDomainsVerify,
		VPSStart, VPSStop, VPSReboot, VPSSnapshot, VPSNetwork,
		SupportTicketsCreate,
		ServiceAccountsActAs,
		// Voir les dépôts des comptes GitHub déjà reliés (noms, branches) pour
		// choisir quoi déployer ; relier un compte reste un droit d'éditeur.
		WorkloadsGitHubRead,
	})

	editorPerms = concat(developerPerms, []Permission{
		ProjectsUpdate,
		WorkloadsDelete, SitesDelete, SiteDomainsDelete, BuildsDelete,
		// Une base occupe du disque et sa suppression perd des données : éditeur.
		DatabasesCreate, DatabasesDelete, DatabasesImport, DatabasesReset, DatabasesStop,
		// Un service occupe des ressources et sa suppression perd des données : éditeur.
		AddonsCreate, AddonsUpdate, AddonsDelete, AddonsRestore,
		// Un bucket occupe de l'espace facturé ; une clé ouvre son contenu : éditeur.
		StorageBucketsCreate, StorageBucketsUpdate, StorageBucketsConnect,
		StorageObjectsDelete, StorageObjectsShare,
		// Une IP dédiée est facturée au mois : éditeur.
		EgressAddressesManage,
		// Réinstaller efface le disque du VPS : éditeur.
		VPSReinstall, VPSRestore, VPSRescue, VPSDelete,
		// Relier un compte GitHub ouvre au projet du code privé : pas un droit de développeur.
		WorkloadsGitHubManage,
		// Un jeton de registre ouvre les images privées de l'entreprise : même règle.
		RegistriesManage,
		ServiceAccountsCreate, ServiceAccountsUpdate, ServiceAccountsDelete,
		ServiceAccountKeysCreate, ServiceAccountKeysDelete,
		APIKeysCreate, APIKeysDelete,
	})

	ownerPerms = concat(editorPerms, []Permission{
		OrganizationsUpdate, OrganizationsSetIamPolicy,
		ProjectsCreate, ProjectsDelete, ProjectsUndelete, ProjectsSetIamPolicy,
		// Supprimer un bucket efface son contenu (ADR 0015) : propriétaire.
		StorageBucketsDelete,
		// Ouvrir un bucket à tous expose son contenu : propriétaire.
		StorageBucketsSetPublic,
		RolesCreate, RolesUpdate, RolesDelete,
		GroupsCreate, GroupsUpdate, GroupsDelete,
		DenyPoliciesUpdate,
		BillingAccountsGet, BillingAccountsUpdate, BillingPaymentsCreate,
		AuditLogsRead,
	})

	platformAdminPerms = []Permission{
		PlatformClustersList, PlatformClustersManage, PlatformClusterKeysList, PlatformClusterKeysCreate, PlatformClusterKeysRotate,
		PlatformClusterKeysRevoke, PlatformProjectsSetLimits, PlatformBillingCatalogsGet, PlatformBillingCatalogsUpdate,
		PlatformBillingAccountsGet, PlatformBillingAccountsUpdate,
		PlatformMailSuppressionsManage,
		PlatformComputeGet, PlatformComputeManage,
		PlatformSupportManage,
		PlatformLegalManage, PlatformStatusManage,
		PlatformOperatorUse, PlatformSupportGet, PlatformStatusGet,
	}
)

var predefinedRoles = []Role{
	{Name: RoleOwner, Title: "Propriétaire",
		Description: "Accès complet, y compris les accès, la facturation et la suppression.",
		Permissions: ownerPerms},
	{Name: RoleEditor, Title: "Éditeur",
		Description: "Crée, modifie et supprime les ressources ; ne gère ni les accès ni la facturation.",
		Permissions: editorPerms},
	{Name: RoleDeveloper, Title: "Développeur",
		Description: "Déploie et opère les conteneurs et les sites, sans rien supprimer.",
		Permissions: developerPerms},
	{Name: RoleViewer, Title: "Lecteur",
		Description: "Consulte les ressources, les logs et les métriques.",
		Permissions: viewerPerms},

	{Name: "roles/workloads.developer", Title: "Développeur conteneurs",
		Description: "Déploie et opère les conteneurs du projet.",
		Permissions: []Permission{ProjectsGet, WorkloadsList, WorkloadsGet, WorkloadsCreate, WorkloadsUpdate,
			WorkloadsStart, WorkloadsStop, WorkloadsRestart, WorkloadsRollback, WorkloadLogsRead, WorkloadMetrics,
			WorkloadVariablesList, WorkloadVariablesUpdate, WorkloadDomainsList, WorkloadDomainsUpdate, BuildsList, BuildsGet, BuildsCreate, BuildsCancel, RegistriesList, ServiceAccountsActAs,
			WorkloadsGitHubRead}},
	{Name: "roles/workloads.viewer", Title: "Lecteur conteneurs",
		Description: "Consulte les conteneurs, leurs logs et leurs métriques.",
		Permissions: []Permission{ProjectsGet, WorkloadsList, WorkloadsGet, WorkloadLogsRead, WorkloadMetrics, WorkloadVariablesList, WorkloadDomainsList, BuildsList, BuildsGet}},
	{Name: "roles/sites.admin", Title: "Administrateur sites",
		Description: "Gère entièrement les sites statiques et leurs domaines.",
		Permissions: []Permission{ProjectsGet, SitesList, SitesGet, SitesCreate, SitesUpdate, SitesDelete,
			SiteDeploymentsList, SiteDeploymentsCreate, SiteDeploymentsActive,
			SiteDomainsList, SiteDomainsCreate, SiteDomainsVerify, SiteDomainsDelete}},
	{Name: "roles/sites.deployer", Title: "Déployeur sites",
		Description: "Publie de nouvelles versions des sites (idéal pour la CI).",
		Permissions: []Permission{SitesList, SitesGet, SiteDeploymentsList, SiteDeploymentsCreate, SiteDeploymentsActive}},
	{Name: "roles/storage.admin", Title: "Administrateur stockage",
		Description: "Gère entièrement les buckets de stockage, leurs clés, leur accès public et leurs fichiers.",
		Permissions: []Permission{ProjectsGet, StorageBucketsList, StorageBucketsGet, StorageBucketsCreate, StorageBucketsUpdate,
			StorageBucketsConnect, StorageBucketsDelete, StorageBucketsSetPublic,
			StorageObjectsList, StorageObjectsGet, StorageObjectsCreate, StorageObjectsDelete, StorageObjectsShare}},
	{Name: "roles/storage.objectUser", Title: "Utilisateur fichiers stockage",
		Description: "Parcourt, télécharge, envoie, supprime et partage les fichiers des buckets, sans gérer les buckets ni les clés.",
		Permissions: []Permission{ProjectsGet, StorageBucketsList, StorageBucketsGet,
			StorageObjectsList, StorageObjectsGet, StorageObjectsCreate, StorageObjectsDelete, StorageObjectsShare}},
	{Name: "roles/iam.serviceAccountUser", Title: "Utilisateur de compte de service",
		Description: "Peut exécuter des ressources en tant que le compte de service.",
		Permissions: []Permission{ServiceAccountsList, ServiceAccountsGet, ServiceAccountsActAs}},
	{Name: "roles/iam.serviceAccountAdmin", Title: "Administrateur comptes de service",
		Description: "Crée et gère les comptes de service et leurs clés.",
		Permissions: []Permission{ServiceAccountsList, ServiceAccountsGet, ServiceAccountsCreate, ServiceAccountsUpdate,
			ServiceAccountsDelete, ServiceAccountKeysList, ServiceAccountKeysCreate, ServiceAccountKeysDelete,
			APIKeysList, APIKeysCreate, APIKeysDelete}},
	{Name: "roles/iam.securityAdmin", Title: "Administrateur sécurité",
		Description: "Gère les accès de l'organisation et de ses projets, et lit le journal d'audit.",
		Permissions: []Permission{OrganizationsGet, OrganizationsGetIamPolicy, OrganizationsSetIamPolicy,
			ProjectsGet, ProjectsList, ProjectsGetIamPolicy, ProjectsSetIamPolicy,
			RolesList, RolesCreate, RolesUpdate, RolesDelete,
			GroupsList, GroupsGet, GroupsCreate, GroupsUpdate, GroupsDelete,
			ServiceAccountsList, ServiceAccountsGet, ServiceAccountKeysList, APIKeysList, APIKeysDelete,
			DenyPoliciesList, DenyPoliciesUpdate,
			AuditLogsRead}},
	{Name: "roles/iam.groupAdmin", Title: "Administrateur des groupes",
		Description: "Crée les groupes de l'organisation et gère leurs membres (dans la limite de ses propres droits).",
		Permissions: []Permission{OrganizationsGet, GroupsList, GroupsGet, GroupsCreate, GroupsUpdate, GroupsDelete}},
	{Name: "roles/billing.admin", Title: "Administrateur facturation",
		Description: "Gère la facturation et les paiements.",
		Permissions: []Permission{OrganizationsGet, BillingAccountsGet, BillingAccountsUpdate, BillingPaymentsCreate}},
	{Name: "roles/logging.viewer", Title: "Lecteur journal d'audit",
		Description: "Lit le journal d'audit.",
		Permissions: []Permission{AuditLogsRead}},

	{Name: RolePlatformAdmin, Title: "Administrateur plateforme",
		Description: "Staff betorun : opère la flotte de clusters.",
		Permissions: platformAdminPerms, PlatformOnly: true},
	{Name: RolePlatformSupport, Title: "Support plateforme",
		Description: "Staff betorun : consulte la flotte et traite les tickets de support.",
		Permissions: []Permission{PlatformClustersList, PlatformClusterKeysList, PlatformSupportManage, PlatformSupportGet, PlatformStatusGet, PlatformOperatorUse}, PlatformOnly: true},
	// Attribué par la plateforme au compte de service système d'un cluster,
	// jamais à une personne (iam.md §7.4).
	{Name: RoleClusterAgent, Title: "Agent de cluster",
		Description: "Compte système du watcher d'un cluster : rend compte de ce seul cluster.",
		Permissions: []Permission{PlatformClustersReport}, PlatformOnly: true},
}

// RoleClusterAgent : rôle du compte de service système d'un cluster.
const RoleClusterAgent = "roles/platform.clusterAgent"

var rolesByName = func() map[string]Role {
	m := make(map[string]Role, len(predefinedRoles))
	for _, r := range predefinedRoles {
		m[r.Name] = r
	}
	return m
}()

// LookupRole : rôle prédéfini par nom.
func LookupRole(name string) (Role, bool) {
	r, ok := rolesByName[name]
	return r, ok
}

// PredefinedRoles : rôles attribuables par les clients (hors staff), triés.
func PredefinedRoles() []Role {
	out := make([]Role, 0, len(predefinedRoles))
	for _, r := range predefinedRoles {
		if !r.PlatformOnly {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsPredefinedRoleName : "roles/x" sans "/" supplémentaire. Les rôles
// personnalisés auront la forme "organizations/{o}/roles/{r}".
func IsPredefinedRoleName(name string) bool {
	return strings.HasPrefix(name, RolePrefix) && !strings.Contains(strings.TrimPrefix(name, RolePrefix), "/")
}

func concat(a, b []Permission) []Permission {
	out := make([]Permission, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}
