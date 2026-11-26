# KubeManager — Documentation

> Plateforme CaaS (Container-as-a-Service) multi-tenant en Go. Déploie des applications containerisées sur clusters K3s/K8s distants via une API unifiée.

## Tu es nouveau ? Commence ici

1. [Setup local (10 min)](onboarding/01-setup-local.md) — faire tourner la stack sur ta machine
2. [Première contribution (30 min)](onboarding/02-first-contribution.md) — tests, PR, review
3. [Tour du code (45 min)](onboarding/03-tour-du-code.md) — où vit quoi et pourquoi
4. [Glossaire](onboarding/04-glossaire.md) — les mots qu'on utilise tous les jours

## Tu cherches comment faire X ?

- [Ajouter un endpoint HTTP](how-to/ajouter-un-endpoint.md)
- [Ajouter un workflow Temporal](how-to/ajouter-un-workflow-temporal.md)
- [Ajouter une migration SQL](how-to/ajouter-une-migration-sql.md)
- [Ajouter une métrique Prometheus](how-to/ajouter-une-metric.md)
- [Déboguer un déploiement bloqué](how-to/deboguer-un-deploy-bloque.md)
- [Rotation d'une clé cluster](how-to/rotation-cluster-key.md)

## Tu veux comprendre l'architecture ?

- [Vue d'ensemble](architecture/overview.md) — 3 diagrammes C4
- [Cycle de vie d'une requête](architecture/request-lifecycle.md) — que se passe-t-il quand `POST /workloads` ?
- [Sagas Temporal](architecture/temporal-sagas.md) — compensation, idempotence, retries
- [Sécurité](architecture/security.md) — JWT, API keys, RBAC, NetworkPolicy
- [IAM](architecture/iam.md) — organisations, policies, comptes de service, clés, permissions plateforme
- [Observabilité](architecture/observability.md) — logs, metrics, traces

## Tu travailles sur un module ?

Chaque page décrit un module avec la même structure : rôle, endpoints, DB, activities, invariants, pitfalls, tests, fichiers clés.

- [auth](modules/auth.md) — users, sessions, API keys, RBAC
- [identity](modules/identity.md) — Keycloak (register, login, OAuth social)
- [projects](modules/projects.md) — namespace K8s + saga provisioning
- [workloads](modules/workloads.md) — conteneurs web, worker et cron : cycle de vie, variables, domaines
- [clusters](modules/clusters.md) — watcher protocol + enrollment
- [realtime](modules/realtime.md) — Centrifugo (WebSocket events)
- [sites](modules/sites.md) — sites statiques (upload .zip, S3, binaire static-edge, domaines perso)
- [storages3](modules/storages3.md) — stockage objet S3 : buckets, clés par usage, quota, lecture publique, liens signés, explorateur
- [databases](modules/databases.md) — Postgres gérées (CloudNativePG) : branchement, import, contenu, remise à zéro
- [network](modules/network.md) — réseau privé d'un projet : membres, liens, règles (modèle indépendant de Kubernetes)
- [vpc](modules/vpc.md) — réseau privé (VPC) du projet étendu à ses VPS (ADR 0019) : rattachement, inscription, mises en œuvre interchangeables
- [addons](modules/addons.md) — services additionnels (Redis via Valkey) : catalogue, branchement, mise à jour, « Me prévenir »
- [diskusage](modules/diskusage.md) — usage réel des disques : mesure horaire, alertes 80/100 %, arrêt à 120 %
- [insights](modules/insights.md) — « L'assistant a remarqué » : constats d'un projet (échecs, builds, budget, disques, domaines, bases, services), cache 60 s par droits
- [diskresize](modules/diskresize.md) — agrandir un disque par copie (conteneurs et services), reprenable après un échec
- [builds](modules/builds.md) — construction d'images (BuildKit), registre de la plateforme, ménage et quota
- [registries](modules/registries.md) — identifiants des registres d'images privés
- [github](modules/github.md) — GitHub App, dépôts, déploiement à chaque push
- [organizations](modules/organizations.md) — organisations, groupes, membres
- [iamapi](modules/iamapi.md) — routes IAM : policies, rôles personnalisés, comptes de service, clés, audit
- [alerts](modules/alerts.md) — alertes e-mail (conteneur en échec, build échoué)
- [copilot](modules/copilot.md) — assistant (outils dérivés des routes, LLM, SSE)

## Tu es ops / oncall ?

- [Runbook](interne/runbook.md) — SLAs, playbooks incidents, DR plan 30 min
- [Secrets management](interne/secrets-management.md) — Vault, ExternalSecrets, SOPS
- [Rétention Prometheus/Loki](interne/observability-retention.md) — 30 j chaud, archivage
- [Helm hardening](interne/helm-hardening.md) — PSA restricted, NetworkPolicy
- [Dev stack locale](interne/dev-stack.md) — docker-compose complet

## Décisions d'architecture (ADRs)

Les "pourquoi on a choisi ça plutôt qu'autre chose". Lire dans l'ordre pour comprendre la philosophie.

- [ADR-0001 — Temporal plutôt qu'Asynq](adr/0001-temporal-over-asynq.md)
- [ADR-0002 — Helm SDK v3 intégré plutôt que shell](adr/0002-helm-sdk-v3.md)
- [ADR-0003 — Centrifugo fail-open](adr/0003-centrifugo-fail-open.md)
- [ADR-0004 — Cluster watcher comme binaire séparé](adr/0004-cluster-watcher-separate.md)
- [ADR-0005 — Keycloak comme source unique auth](adr/0005-keycloak-source-of-truth.md)
- [ADR-0006 — Rate limit Redis-only](adr/0006-ratelimit-redis-only.md)
- [ADR-0007 — Keycloak derrière le backend](adr/0007-keycloak-behind-backend.md)
- [ADR-0008 — Couche d'accès aux modèles de langage](adr/0008-llm-provider-layer.md)
- [ADR-0009 — Alertes e-mail minimales](adr/0009-alertes-email.md)
- [ADR-0010 — Ménage et quota du registre de la plateforme](adr/0010-registre-menage-quota.md)
- [ADR-0011 — Accès aux registres d'images externes](adr/0011-registres-externes.md)

## Conventions doc

- Chaque page répond à **une** question (son titre).
- Les références code utilisent `[file.go:42](path#L42)` — cliquable depuis GitHub/IDE.
- Les diagrammes sont en **Mermaid** inline (rendu natif GitHub, pas de PNG).
- Les chunks `.md` de `docs/interne/` sont ingérés par le devportal (`go run ./cmd/dev-docs`).

## Retrouver de l'aide

- Canal Slack `#kubemanager-dev` — question pendant tes 2 premières semaines : no stupid question.
- Oncall — voir [runbook](interne/runbook.md) pour la rotation et les SLAs.
