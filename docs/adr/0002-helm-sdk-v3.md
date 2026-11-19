# ADR 0002 — Helm SDK v3 intégré plutôt que shell

- **Status** : accepté
- **Date** : 2025-12-02

## Contexte

Pour déployer des workloads utilisateurs sur les clusters gérés, on a besoin de piloter Helm. Deux familles de solutions :

- **Shell-out** : forker `helm install`, `helm upgrade`, `helm uninstall` depuis notre process.
- **SDK intégré** : importer `helm.sh/helm/v3/pkg/action` et appeler les fonctions en Go.

Choisir mal influe directement sur la fiabilité, la performance et l'observabilité de la plateforme.

## Décision

Utiliser le **Helm SDK v3** en Go, intégré dans les activities Temporal (`internal/modules/workloads/activities/heml.go`).

## Conséquences

### Positives

- **Pas de process fork** — pas de risque de zombie, pas de dépendance sur un binaire Helm installé correctement dans le container (pile de versions à gérer).
- **Error handling natif Go** — on récupère des erreurs typées (`*driver.StorageDriverError`, `release.ErrNotFound`, …) plutôt que de parser du stdout/stderr.
- **Context-aware** — on peut passer un `context.Context` avec timeout et cancel ; le shell-out ignore l'annulation proprement.
- **Performance** : pas de startup cost du binaire Helm à chaque appel (peanuts mais × 1000 ops/jour ça s'additionne).
- **OTEL tracing natif** — on peut wrapper les appels avec des spans, chose impossible en shell-out.
- **Tests plus faciles** — on peut mocker `action.Configuration` en tests unitaires (même si Helm lui-même est peu mockable, c'est un cran mieux que du shell).

### Négatives

- **Taille binaire** : Helm SDK + toutes ses deps (k8s.io/client-go, apimachinery, etc.) ajoute ~50 Mo au binaire kubemanager.
- **Couplage de version** : on est lié à une version du SDK Helm. Upgrader Helm v3 → v4 nécessite un port.
- **API moins stable** que le CLI — les maintainers de Helm changent parfois la signature des `action.NewXxx()`.
- **Complexité** : `action.Configuration` a plusieurs champs obligatoires (`RESTClientGetter`, `Releases`, `Log`) qu'il faut câbler correctement. Exemple dans [activities/heml.go:311](../../internal/modules/workloads/activities/heml.go#L311).

## Alternatives considérées

### Shell-out `helm install ...`

Rejeté :
- Bootstrap du binaire dans l'image Docker (version à pinner, layer à updater).
- Parsing fragile du stdout pour extraire le nom de release, le status…
- Pas de cancel propre — un SIGTERM sur le process Helm pendant un install laisse un état indéterminé.

### Appel direct à l'API Kubernetes (pas de Helm)

Rejeté pour l'instant. On aurait plus de contrôle mais on perd :
- Le versioning des releases (Helm storage driver).
- Les hooks Helm (pre-install, post-upgrade).
- La compatibilité avec les charts communautaires si un jour on en embarque.

Sera peut-être revu quand on contrôle 100% des templates et qu'on n'importe plus de charts tiers.

### Flux (GitOps)

Intéressant pour un SaaS hautement régulé, mais pour notre modèle "CaaS on-demand" c'est lourd : chaque deploy ferait un commit + sync Flux → latence de plusieurs secondes inacceptable pour un `POST /workloads`.

## Impact sur l'architecture

- Les activities Helm (`InstallChart`, `UninstallChart`) sont encapsulées par `idempotency.ExecuteActivity()` (TTL 10 min) — garantit l'idempotence sur retry Temporal.
- Le chart `standard-app` est embedé dans le binaire via `go embed`… **non**, en réalité il est chargé depuis le filesystem `/app/internal/infrastructure/helm/charts/standard-app/`. À migrer vers `go:embed` pour rendre le binaire autosuffisant (TODO à planifier).
- On ne supporte **qu'un seul chart** (`standard-app`). Pour supporter plusieurs charts (ex: `stateful-app` pour DB), étendre le DTO avec un sélecteur + le map côté activity.

## Contraintes opérationnelles

- Les credentials K8s du cluster cible (kubeconfig) sont résolus via `k8sprovider.ResolveKubeconfig(clusterID)`. Pour l'instant hardcodé au cluster local — à câbler proprement quand on passe multi-cluster en prod.
- L'image kubemanager doit contenir les CRDs standards (rien de spécial) — le SDK Helm gère sa propre storage driver.

## Références

- Helm SDK docs : <https://helm.sh/docs/topics/advanced/>
- Notre implémentation : [activities/heml.go](../../internal/modules/workloads/activities/heml.go)
- Chart `standard-app` : [internal/infrastructure/helm/charts/standard-app/](../../internal/infrastructure/helm/charts/standard-app/)
