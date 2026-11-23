# Module legal

Conditions générales de vente des serveurs VPS (`terms_vps`) et politique
d'usage acceptable (`aup`) : documents **versionnés**, rédigés et publiés par
l'équipe depuis le back-office, acceptés par les organisations clientes.

- Une version est un **brouillon** (modifiable, jamais public) ou **publiée**
  (immuable : le service refuse, et un déclencheur Postgres aussi).
- La version **en vigueur** d'un document est sa dernière version publiée.
- Un seul brouillon à la fois par document ; son numéro = dernier + 1.
- Une organisation accepte une version précise (qui, quand, adresse IP).
  Publier une nouvelle version oblige à l'accepter de nouveau.
- Un document sans version publiée ne bloque rien.

Code : `internal/modules/legal` (`domain`, `service`, `repository`,
`contract`), câblage `internal/infrastructure/router/legal_wiring.go`,
migration `000066_legal` (tables `legal_documents`, `legal_acceptances`, et la
version 1 de chaque document, publiée).

> Les textes de la version 1 sont un **projet à faire relire par un juriste
> avant ouverture au public** (bandeau en tête de chaque texte). Marqueurs à
> remplir : « [à compléter : droit applicable et juridiction] » (CGV) et
> « [adresse de contact abus à compléter] » (politique d'usage). Pour les
> corriger : nouveau brouillon depuis le back-office, puis publication. Aucun
> fournisseur d'infrastructure n'est nommé (marque blanche).

## API

Préfixe `/kmanager/v1`. Erreurs au format habituel `{code, message, error}`.

### Public (sans authentification)

`GET /legal/:kind?version=&lang=` → version en vigueur, ou une version publiée
plus ancienne (`version`). Langue : `lang=fr|en`, sinon celle de la requête
(cookie `btr_locale`, `Accept-Language`, français par défaut).

```json
{ "kind": "terms_vps", "version": 2, "lang": "fr", "title": "…", "body": "Markdown…",
  "published_at": "2026-11-23T10:00:00Z", "current": true }
```

Erreurs : 404 `NOT_FOUND` (document inconnu, version absente ou brouillon),
400 `INVALID_INPUT` (`lang` ou `version` invalide).

### Organisation

| Méthode | Chemin | Permission | Rôle |
|---|---|---|---|
| GET | `/organizations/:id/legal?lang=` | `organizations.get` | où en est l'organisation |
| POST | `/organizations/:id/legal/accept` | `billing.payments.create` | accepter la version en vigueur |

`GET` →

```json
{ "documents": [ { "kind": "terms_vps", "current_version": 2, "title": "…",
  "published_at": "…", "accepted_version": 1, "accepted_at": "…",
  "accepted_by": "user:…", "needs_acceptance": true } ] }
```

`accepted_version` vaut `null` si l'organisation n'a jamais accepté ce
document. Seuls les documents publiés apparaissent.

`POST` corps `{ "kind": "terms_vps", "version": 2 }` → `{ kind, version,
accepted_at, accepted_by }`. Idempotent (même version : l'acceptation
d'origine). La permission de paiement est sensible : la console demande une
confirmation d'identité récente (step-up), comme pour une commande.
Erreurs : 409 `LEGAL_VERSION_NOT_CURRENT` (pas la version en vigueur), 404
document inconnu, 400 corps invalide.

### Équipe (`/admin/legal`, interne, `platform.legal.manage`)

| Méthode | Chemin | Rôle |
|---|---|---|
| GET | `/admin/legal` | chaque document : `current_version`, `draft_version` (0 : aucun), `current_acceptances` |
| GET | `/admin/legal/:kind/versions` | versions, la plus récente d'abord, sans le texte, avec `acceptances` (nombre d'organisations) et `current` |
| POST | `/admin/legal/:kind/versions` | nouveau brouillon ; corps `{title_fr, title_en, body_fr, body_en}`, ou vide : copie de la version en vigueur |
| GET | `/admin/legal/:kind/versions/:version` | version complète (+ `current`, `acceptances`) |
| PUT | `/admin/legal/:kind/versions/:version` | réécrire un brouillon (les quatre champs, obligatoires) |
| DELETE | `/admin/legal/:kind/versions/:version` | abandonner un brouillon |
| POST | `/admin/legal/:kind/versions/:version/publish` | publier : la version entre en vigueur |

Version complète : `kind, version, title_fr, title_en, body_fr, body_en,
status (draft|published), created_by, created_at, updated_by, updated_at,
published_by, published_at, current, acceptances`.

Erreurs : 409 `LEGAL_DRAFT_EXISTS` (un brouillon existe déjà), 409
`LEGAL_VERSION_PUBLISHED` (modifier, supprimer ou republier une version
publiée), 400 `INVALID_INPUT` (texte manquant dans une langue, trop long —
titre 200, texte 200 000 caractères —, rien à copier), 404.

## Pour les autres modules : `contract.TermsGate`

```go
// internal/modules/legal/contract
type TermsGate interface {
	RequireAccepted(ctx context.Context, org uuid.UUID, kinds ...string) error
}
var ErrTermsNotAccepted // HTTP 409, code TERMS_NOT_ACCEPTED
const KindTermsVPS, KindAUP
```

`*legal/service.Service` (`App.legalService()`) la satisfait. Usage prévu
(compute, commande d'un VPS, branchement à faire) :

```go
if err := s.Terms.RequireAccepted(ctx, org, contract.KindTermsVPS, contract.KindAUP); err != nil {
	return err
}
```

À réception de `TERMS_NOT_ACCEPTED`, la console lit
`GET /organizations/:id/legal`, affiche les textes (`GET /legal/:kind`) et
fait accepter ceux dont `needs_acceptance` est vrai.
