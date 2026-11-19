# Glossaire

> Les termes qu'on utilise en réunion et en PR. Si tu butes sur un mot, il est sans doute ici.

## Métier

**Project** — entité racine multi-tenant. 1 project = 1 namespace K8s (`km-<project-name>`) + 1 groupe Keycloak (`project-<uuid>`) + N workloads + N membres (via `project_members`).

**Workload** — une application containerisée déployée via Helm. Une `release` Helm = un Deployment + un Service + (optionnel) Ingress, PVC, HPA, PDB, NetworkPolicy. Label K8s pivot : `km-workload-id=<uuid>`.

**Cluster** — un K8s/K3s distant que kubemanager pilote. Chaque cluster exécute un `watcher` (binaire séparé dans `betorun/watcher`) qui pousse les pod events vers kubemanager via API key.

**Watcher** — le binaire installé sur chaque cluster géré. Rôle : Enroll au démarrage, GetConfig périodique, OnPodEvent par événement K8s. Leader election via Lease K8s (un seul actif par cluster).

**Member** / **ProjectMember** — un User rattaché à un Project avec un Role (owner/admin/developer/viewer) et un Status (ACTIVE/INVITED/APPROVED).

**API Key** — credential alternatif au JWT Bearer. Deux `kind` possibles : `user` (scopes API) pour les devs et les CI ; `cluster` pour les watchers.

## Auth

**JWT Bearer** — token OIDC émis par Keycloak, validé côté kubemanager via JWKS. `userID` extrait du claim `sub`.

**ROPC** (Resource Owner Password Credentials) — grant OAuth2 `password` — réservé au dev/CI, désactivé en prod sur les clients publics.

**RBAC projet** — `CheckProjectPermission(userID, projectID, permissionSlug)` — point d'entrée unique pour les permissions projet.

**Scope** — droit attaché à une API key (ex: `project:read`, `workload:write`, `cluster:report`).

**Kind** (API key) — `user` ou `cluster`. Le middleware `RequireAPIKeyKind("cluster")` protège les endpoints watcher.

## Temporal

**Workflow** — orchestrateur long-running et résumable. Chez nous : `CreateProjectWorkflow`, `DeleteProjectWorkflow`, `DeployWorkloadWorkflow`, `DeleteWorkloadWorkflow`.

**Activity** — unité de travail idempotente appelable par un workflow. Chez nous : `InstallChart`, `UninstallChart`, `CreateNamespace`, `SoftDeleteWorkload`, …

**Saga** — pattern où une séquence d'activités déclare aussi ses compensations (rollback) en cas d'échec partiel. Exemple : si `CreateNamespace` échoue après `CreateProjectInDB`, on déclenche `DeleteProjectDBActivity` pour compenser.

**Idempotency key** — clé stockée dans Redis (`idem:<activity>:<input-hash>`) pour qu'un retry Temporal ne re-effectue pas un effet déjà appliqué (ex: un `helm upgrade` qui a déjà réussi).

**Task queue** — nom logique sur lequel les workers s'abonnent pour consommer les workflows/activities. Chez nous : `kubemanager-queue`.

## Helm / K8s

**Release** — instance installée d'un chart Helm. Nommée d'après le workload.

**chart standard-app** — le template unique qu'on utilise pour toutes les apps utilisateurs (Deployment, Service, Ingress, PVC, NetworkPolicy). Lives in [internal/infrastructure/helm/charts/standard-app/](../../internal/infrastructure/helm/charts/standard-app/).

**PSA restricted** — Pod Security Admission profile `restricted`. Contraintes : `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`. Notre chart est compliant par défaut.

**HPA** — HorizontalPodAutoscaler. Scale réplicas sur CPU/RAM.

**PDB** — PodDisruptionBudget. Garantit un minimum de pods up pendant les drain/upgrade K8s.

**NetworkPolicy** — règle L3/L4 "deny by default + allowlist". Chez nous : ingress = namespace ingress-nginx, egress = DNS + HTTPS.

**PVC / PV** — PersistentVolumeClaim / PersistentVolume. Storage persistant. Défaut : `local-path` (K3s local-path-provisioner).

**ResourceQuota** — limite CPU/RAM/storage par namespace, posée à la création du projet.

## Observabilité

**Golden signals** ou **RED** — Rate, Errors, Duration. Les 3 métriques minimales de tout service HTTP.

**Trace ID** — identifiant unique par requête, propagé via OpenTelemetry. Utilisé pour corréler logs ↔ traces.

**Span** — unité de travail dans une trace (1 requête = N spans imbriqués).

**PromQL** — langage de query Prometheus. Exemple : `histogram_quantile(0.95, rate(gin_request_duration_seconds_bucket[5m]))`.

**LogQL** — langage de query Loki (similaire à PromQL mais sur les logs).

**ServiceMonitor** — CRD prometheus-operator qui dit à Prometheus "scrape tel Service sur tel path". Alternative moderne au bloc `scrape_configs`.

**PrometheusRule** — CRD prometheus-operator qui définit alertes + recording rules.

## Infra

**DI** (Dependency Injection) — construction des singletons (DB, Redis, Temporal client, etc.) dans [dependencies.go](../../internal/infrastructure/router/dependencies.go), injection via struct `App`.

**Tonic** — lib de routing/validation pour Gin ([loopfz/gadgeto/tonic](https://github.com/loopfz/gadgeto)). Nos controllers prennent `(c *gin.Context, in *DTO) (*Response, error)`, tonic parse + sérialise.

**Fizz** — wrapper OpenAPI autour de tonic. Génère le spec OpenAPI à partir des signatures Go. Ouvre `/docs` (Scalar) et `/swagger/` pour explorer.

**GORM** — ORM Go. Utilisé avec le plugin OTEL pour instrumenter chaque query.

**golang-migrate** — outil de migration SQL versionnée. Les fichiers sont dans [migrations/](../../migrations/) (`000001_init.up.sql`, `.down.sql`).

**Asynq** — ❌ non utilisé chez nous (c'est le pattern gophercart). Kubemanager utilise **Temporal** pour l'asynchrone.

## Sigles

- **CaaS** — Container-as-a-Service (Heroku, Scaleway Container, Fly.io, nous).
- **OIDC** — OpenID Connect (Keycloak).
- **HMAC** — clé de signature symétrique (HS256 JWT Centrifugo).
- **SLO** / **SLA** — Service Level Objective / Agreement.
- **RPO** / **RTO** — Recovery Point/Time Objective (DR plan).
- **P50/P95/P99** — percentiles de latence.
- **ADR** — Architecture Decision Record (nos docs dans `docs/adr/`).

## Prochaine étape

Tu es armé. Choisis un [how-to](../README.md#tu-cherches-comment-faire-x-) pour ton premier ticket, ou passe à [architecture](../architecture/overview.md) pour la vue d'ensemble.
