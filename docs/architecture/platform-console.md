# Console plateforme : back-office séparé

Le back-office de l'équipe betorun (clusters, clés des watchers, limites des
projets, facturation des clients, grilles et plans, adresses e-mail mises de
côté) n'est **pas** hébergé avec l'API et la console des clients. Même code,
deux déploiements.

## API : un mode par process (`APP_MODE`)

| Mode | Sert | Boucles de fond |
|---|---|---|
| `server` (défaut) | toutes les routes **sauf** `/admin/...` (404) | purge des organisations suspendues |
| `platform` | `/admin/...`, `/identity/...` (connexion, session, double authentification), `/iam/...` (`testPermissions`) | aucune |
| `worker` | aucune route (santé) | Temporal, facturation, e-mails… |

- Le tri se fait sur le groupe de premier niveau (`ServedIn`,
  `internal/infrastructure/router/wrapper.go`). Les routes des watchers
  (`platform.clusters.report`) restent dans `server` : les clusters rendent
  compte à l'API des clients.
- Le Copilot n'est pas monté en `platform` (ses outils viennent des routes des clients).
- Garde-fous (tests `platform_mode_test.go`) : toute route qui exige une
  permission `platform.*` est sous `/admin` ; `server` ne monte aucune route
  `/admin` ; `platform` ne monte rien d'autre que ses trois préfixes.
- Les droits ne changent pas : une route `/admin` exige toujours sa
  permission `platform.*`, et les gestes sensibles la double authentification.

## Console : interrupteur de build (`NEXT_PUBLIC_CONSOLE`)

Dans `kweb` (`lib/console.ts`, `proxy.ts`) :

- `client` (défaut) : aucune page `/admin` (404), pas de rubrique Admin ;
- `platform` : seulement `/admin/...`, la connexion et `/settings` (profil,
  sécurité, sessions ; pas les réglages d'organisation) ; toute autre page
  renvoie vers `/admin/clusters`. Ni projets, ni Copilot, ni recherche, ni
  bandeau de solde ; `X-Robots-Tag: noindex`.

`NEXT_PUBLIC_*` est figé au build : une image par console.

```
docker build --build-arg NEXT_PUBLIC_CONSOLE=platform \
  --build-arg NEXT_PUBLIC_API_BASE_URL=https://<api plateforme>/kmanager/v1 \
  --build-arg NEXT_PUBLIC_SITES_BASE_DOMAIN=sites.<domaine> -t kweb-platform .
```

## En local (aujourd'hui)

Le back-office tourne sur le poste, à côté de l'environnement de développement :

1. `make dev-up` puis l'API des clients comme d'habitude (`kubemanager`, port 8080 : sans `/admin`) ;
2. `make dev-platform` : `kubemanager-platform` (`APP_MODE=platform`, `127.0.0.1:8081`, sans migrations, origines `localhost:3001`) ;
3. dans `kweb` : `npm run dev:platform` → <http://localhost:3001> (console plateforme sur l'API 8081) ; `npm run dev` reste la console des clients (port 3000).

Un compte staff (`roles/platform.admin`, liste `iam.platform_admins` de la configuration) se connecte sur le port 3001.

## Déploiement (plus tard)

Deux processus de plus, même image que l'API et que la console :

- `api-platform` : image de l'API, `APP_MODE=platform`,
  `KUBEMANAGER_DATABASE_SKIP_MIGRATIONS=true` (l'API `server` reste seule à
  migrer), même base, Redis et Keycloak ;
- `console-platform` : image `kweb` construite avec `NEXT_PUBLIC_CONSOLE=platform`.

Chacun derrière son propre domaine (ex. `admin.<domaine>` et
`admin-api.<domaine>`), qui ne doit pas être exposé comme l'API des clients :
liste d'IP de l'équipe, VPN ou accès par tunnel. Keycloak : ajouter l'URL de
la console plateforme aux redirections autorisées du client web, et son
origine à `server.allowed_origins` de l'API plateforme.
