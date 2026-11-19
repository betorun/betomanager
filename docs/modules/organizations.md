# Module `organizations`

> Racine de la hiérarchie de ressources IAM (`platform` → `organizations/<id>` → `projects/<id>`). Chaque utilisateur a une organisation personnelle, créée avec son premier projet ; elle porte les groupes, les rôles personnalisés et la politique de sécurité.

## Modèle

Table `organizations` (migration 000003) :

| Colonne | Rôle |
|---|---|
| `id` | UUID |
| `display_name` | Nom affiché (`Mon organisation` par défaut à la création par le code) |
| `kind` | `PERSONAL` (une par utilisateur) ou `TEAM` (partagée, défaut de la colonne) |
| `owner_user_id` | Utilisateur à l'origine d'une organisation `PERSONAL` ; **pas un droit** (les droits sont dans `iam_bindings`) |
| `created_at`, `updated_at`, `deleted_at` | Suppression logique |

- Index unique partiel `uq_organizations_personal_owner` sur `owner_user_id` quand `kind = 'PERSONAL'` : au plus une organisation personnelle par utilisateur.
- `projects.organization_id` (NOT NULL) rattache chaque projet à une organisation.
- Tables liées : `iam_groups` et `iam_group_members` (000010), `iam_custom_roles` (000010, parent `organizations/<id>` ou `projects/<id>`), `iam_org_policies` (000011).
- **Pas de `billing_account_id`** : ni la table ni le code n'en ont. La facturation n'existe que sous forme de permissions (`billing.accounts.get`, `billing.accounts.update`, `billing.payments.create`) et du rôle `roles/billing.admin`.

## Création de l'organisation personnelle

`organizations.EnsurePersonal(ctx, db, userID, displayName)` est le seul point de création dans le code :

- renvoie l'organisation `PERSONAL` existante de l'utilisateur, sinon la crée, dans une transaction, avec un binding `roles/owner` pour `user:<id>` sur `organizations/<id>` (acteur système des organisations) ;
- idempotente et sûre en concurrence : en cas de conflit sur l'index unique, elle relit l'organisation créée par l'autre requête.

Appelant unique : `projects/repository.CreateProject`, quand le projet n'a pas d'organisation. À l'inscription, le bootstrap d'identité (`identity/service.ensureProject`) crée le premier projet, ce qui crée l'organisation personnelle dans la même transaction que le projet et son binding owner. La migration 000003 a créé rétroactivement une organisation `PERSONAL` par utilisateur existant (nom = nom complet ou email) et une organisation `TEAM` sans droit pour chaque projet sans owner.

## Endpoints HTTP

Le paquet `organizations` n'a pas de contrôleur ; les routes de l'organisation sont servies par `iamapi` ([route.go](../../internal/infrastructure/router/route.go), `addIAMRoutes`). Préfixe `/kmanager/v1`.

| Méthode | Chemin | Permission | Description |
|---|---|---|---|
| GET | `/organizations/:id/securityPolicy` | `resourcemanager.organizations.get` | Politique de sécurité (`require_mfa`) |
| PUT | `/organizations/:id/securityPolicy` | `resourcemanager.organizations.update` | Modifier (confirmation récente du second facteur requise) |
| GET | `/organizations/:id/groups` | `iam.groups.list` | Lister les groupes |
| POST | `/organizations/:id/groups` | `iam.groups.create` | Créer un groupe (`group_id`, `display_name`, `description`) |
| GET | `/organizations/:id/groups/:groupID` | `iam.groups.get` | Détails |
| PATCH | `/organizations/:id/groups/:groupID` | `iam.groups.update` | Modifier nom et description |
| DELETE | `/organizations/:id/groups/:groupID` | `iam.groups.delete` | Supprimer (ses rôles partout compris) |
| GET | `/organizations/:id/groups/:groupID/members` | `iam.groups.get` | Membres |
| POST | `/organizations/:id/groups/:groupID/members` | `iam.groups.update` | Ajouter `user:<email\|uuid>` ou `serviceAccount:<uuid>` |
| DELETE | `/organizations/:id/groups/:groupID/members?member=…` | `iam.groups.update` | Retirer un membre |

Les accès à l'organisation passent par l'API générique de policy (`GET/POST /iam/policy?resource=organizations/<id>`, permissions `resourcemanager.organizations.getIamPolicy` / `setIamPolicy`) ; `GET /iam/ancestry` donne la chaîne projet → organisation → platform.

## Groupes et membres

- `group_id` : 3 à 63 caractères, minuscules, chiffres et tirets, commence par une lettre ; unique dans l'organisation (hors groupes supprimés).
- Membres : personnes et comptes de service ; **pas de groupes imbriqués** (`ErrNestedGroup`). Un membre ajouté par email est enregistré par son UUID ; l'ajout est idempotent.
- **Anti-escalade** : ajouter un membre exige que l'appelant détienne lui-même, sur chaque ressource, toutes les permissions des rôles accordés au groupe (`ErrGroupEscalation`). Retirer un membre est libre.
- Un groupe ne peut recevoir de rôles que dans son organisation (`ErrGroupOutOfScope`).
- Rôles qui couvrent les groupes : `roles/owner`, `roles/iam.securityAdmin`, `roles/iam.groupAdmin` ; les rôles de base (viewer compris) voient l'organisation, listent les groupes et lisent leurs membres.

## Pièges

- `owner_user_id` n'accorde rien : retirer le binding owner retire le droit, même sur l'organisation personnelle.
- Aucune route ne crée, renomme ou supprime une organisation, ni ne crée d'organisation `TEAM` : seules la migration et `EnsurePersonal` en créent.
- Utiliser les permissions `resourcemanager.organizations.*` et `iam.groups.*`, pas les anciens noms du style `project:view`.

## Fichiers clés

- [organizations.go](../../internal/modules/organizations/organizations.go) — modèle, `EnsurePersonal`.
- [projects/repository/repository.go](../../internal/modules/projects/repository/repository.go) — création du projet et de l'organisation.
- [iamapi/teams.go](../../internal/modules/iamapi/teams.go), [iamapi/security.go](../../internal/modules/iamapi/security.go) — groupes, politique de sécurité.
- [iam/groups.go](../../internal/iam/groups.go) — règles des groupes, anti-escalade.
- [000003_iam.up.sql](../../migrations/000003_iam.up.sql), [000010_groups_custom_roles.up.sql](../../migrations/000010_groups_custom_roles.up.sql), [000011_org_policies.up.sql](../../migrations/000011_org_policies.up.sql) — schéma.
