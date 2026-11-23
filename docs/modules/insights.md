# Module `insights`

> « L'assistant a remarqué » : ce qui mérite l'attention dans un projet, tiré de l'état réel (conteneurs en échec, builds ratés, budget, disques, domaines non vérifiés, bases arrêtées, services en panne). Affiché par la console ; lu par le Copilot (`get_project_insights`, outil du noyau).

## Route

| Méthode | Chemin | Permission | Description |
|---------|--------|-----------|-------------|
| GET | `/kmanager/v1/projects/:id/insights` | `resourcemanager.projects.get` (+ droits par source, voir plus bas) | Constats du projet, 10 au plus |

Réponse (contrat de la console, ne pas changer) :

```json
{ "generated_at": "2026-11-23T12:00:00Z",
  "insights": [ {
    "key": "workload_crashloop:<id>",
    "kind": "workload_crashloop",
    "severity": "danger",
    "resource": { "type": "workload", "id": "<id>", "name": "web" },
    "params": { "status": "CRASHLOOP", "restarts": "9" },
    "since": "2026-11-23T11:55:00Z" } ] }
```

`params` est toujours un objet (valeurs en texte), `since` est absent s'il n'est pas connu, `insights` est toujours un tableau.

## Règles

| Sorte | Source | Condition | Gravité | Params |
|-------|--------|-----------|---------|--------|
| `workload_crashloop` | conteneurs (statut enregistré) | `CRASHLOOP` | danger | `status`, `restarts` (lu en direct, 3 s au plus ; omis si illisible) |
| `workload_oom` | idem | `OOM_KILLED` | danger | `status` |
| `workload_image_pull` | idem | `IMAGE_PULL_FAILED` | danger | `status` |
| `workload_failed` | idem | `FAILED`, `CONFIG_ERROR` (danger) ; `EVICTED`, `RUN_FAILED` (warn) | | `status` |
| `build_failed` | builds récents | dernier build terminé d'un conteneur en échec (`FAILED`) plus récent que son dernier succès | warn | `workload_id`, `failure` |
| `budget_high` | budget du projet | dépense ≥ 80 % du budget (≥ 100 % : danger) | warn/danger | `percent` |
| `budget_stopped` | idem | conteneurs arrêtés par le budget ce mois-ci (remplace `budget_high`) | danger | `percent` |
| `disk_high` | usage des disques (`diskusage`) | ≥ 85 % (≥ 100 % : danger) | warn/danger | `percent` |
| `domain_unverified` | domaines des conteneurs et des sites | non vérifié, ajouté depuis plus d'1 h | warn | `hostname`, `workload_id` ou `site_id` |
| `database_stopped` | bases Postgres | `STOPPED` (danger si un conteneur y est branché, sinon info) | | `status`, `attached` |
| `addon_unhealthy` | services (statut du porteur) | `CRASHLOOP`, `OOM_KILLED`, `IMAGE_PULL_FAILED`, `FAILED`, `CONFIG_ERROR`, `EVICTED`, `DEGRADED` | danger | `status` |

Les conteneurs porteurs d'un service (`managed_by`) ne font pas de constat de conteneur : `addon_unhealthy` les couvre.

Tri : danger, puis warn, puis info ; à gravité égale le plus récent (`since`) d'abord, sans date ensuite, puis par clé ; 10 au plus (`domain.Rank`).

**Aucune donnée inventée** : une source en erreur est omise et journalisée (`insights: source omitted`, avec la source) ; un module désactivé (builds, facturation, sites…) est une source absente.

## Droits et cache

La route exige la lecture du projet. Chaque source exige en plus sa permission, testée sur le projet (`iam.Engine.TestPermissions`) : `workloads.workloads.list` (conteneurs), `workloads.builds.list` (builds), `billing.accounts.get` (budget : sans elle, aucun constat de budget), `workloads.workloads.list` + `workloads.domains.list` (domaines des conteneurs), `sites.sites.list` + `sites.domains.list` (domaines des sites), `databases.databases.list`, `addons.instances.list`. Les disques suivent la permission de la route (même règle que `GET /projects/:id/disk-usage`). Aucune permission nouvelle.

Cache Redis 60 s, clé `betorun:insights:<projet>:<empreinte des droits>` : l'empreinte est le masque des sources accordées. Deux personnes du même projet aux droits différents n'ont donc jamais le même résultat ; deux personnes aux mêmes droits partagent le calcul (le contenu ne dépend que du projet et de ces droits). Redis injoignable : calcul refait, sans cache. Choix écarté : un cache par projet seul exigerait que tout le contenu suive la permission de la route, donc de renoncer au budget (facturation) ou de l'exiger de tous.

Limite : les droits sont lus au niveau du projet ; une politique de refus posée sur une ressource précise (un conteneur) n'en retire pas le constat.

## Rangement

- [controller.go](../../internal/modules/insights/controller.go) — route, droits par source.
- [domain/insight.go](../../internal/modules/insights/domain/insight.go) — contrat, sortes, seuils, tri.
- [service/](../../internal/modules/insights/service/) — règles (`service.go`), ports des sources (`ports.go`).
- [repository/cache.go](../../internal/modules/insights/repository/cache.go) — cache Redis.
- [insights_wiring.go](../../internal/infrastructure/router/insights_wiring.go) — adaptateurs : conteneurs (dépôt, état en direct pour les redémarrages), builds, budget (facturation), disques, domaines, sites, bases (dépôt + branchements), services, nom du projet.
