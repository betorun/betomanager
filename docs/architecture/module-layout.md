# Rangement d'un module

Chaque module de `internal/modules/<module>` suit le même plan :

| Dossier | Contenu | Peut importer |
|---|---|---|
| racine (`<module>`) | contrôleurs HTTP (`controller.go`, `h*.go`), requêtes et réponses propres à l'API | tout le module |
| `domain/` | types métier, constantes, erreurs, règles pures (sans base ni réseau) | rien du module |
| `service/` | cas d'usage, et les **ports** dont ils ont besoin (interfaces : stockage, Kubernetes, autres modules) | `domain` |
| `repository/` | adaptateurs sortants qui implémentent ces ports : Postgres (GORM), Kubernetes (Jobs, CNPG), API externes | `domain`, `service` (types des ports) |
| `contract/` | ce qu'un **autre** module utilise de celui-ci (interfaces et types échangés) ; créé seulement quand il sert | `domain` |

Règles :

- Jamais de cycle : `service` n'importe pas `repository` ; le câblage
  (`internal/infrastructure/router/*_wiring.go`) relie les deux.
- Une méthode reste dans le paquet de son type.
- Les tests suivent le code qu'ils testent ; un test qui passe par le
  contrôleur reste à la racine.
- Les sous-paquets spécialisés déjà présents (`workflows`, `activities` pour
  Temporal, `keycloak`, `xykpay`, `pdf`, `agent`, `tools`…) restent tels quels.

Modules rangés selon ce plan en novembre 2026 : addons, alerts, clusters,
copilot (racine), databases, diskresize, diskusage, legal, mail, organizations, status ;
auth, billing, builds, domainsales, github, projects, registries, sites et
workloads l'étaient déjà. Racines faites seulement de contrôleurs : iamapi,
identity, realtime.

À faire : `contract/` pour les dépendances entre modules qui passent encore
par le `service` ou le `repository` d'un autre (github → builds, workloads ;
identity → auth, projects ; projects → auth, organizations, sites, workloads ;
workloads → projects, registries ; clusters → workloads ; registries,
sites → builds).
