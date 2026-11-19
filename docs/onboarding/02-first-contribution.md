# Première contribution (30 min)

> Objectif : faire tourner les tests, ouvrir ta première Pull Request, comprendre le workflow de review.

Prérequis : [setup local](01-setup-local.md) terminé.

## 1. Faire tourner les tests

```sh
go test ./internal/...
```

Tu dois voir `ok` sur tous les packages qui ont des tests, `[no test files]` sur les autres. Temps total : ~1 min.

Tests par catégorie :

| Catégorie | Commande |
|-----------|----------|
| Unitaires (tout) | `go test ./internal/...` |
| Middleware | `go test ./internal/middleware/...` |
| Un module | `go test ./internal/modules/workloads/...` |
| Avec coverage | `go test -cover ./internal/...` |
| En parallèle + verbose | `go test -v -race ./internal/...` |

## 2. Lancer `go vet` et `go build`

```sh
go build ./...     # doit compiler sans erreur
go vet ./...       # doit être silencieux
```

Ces deux commandes sont exécutées par la CI sur chaque PR. Localement en pre-commit.

## 3. Lint du chart Helm

```sh
helm lint chart/kubemanager-backend
```

Doit renvoyer `1 chart(s) linted, 0 chart(s) failed`.

## 4. Smoke e2e

```sh
bash scripts/e2e.sh
```

Enchaîne create project → deploy workload → stop → start → delete. Prend ~3 min. Si ça passe, ta stack est en bon état.

## 5. Ouvrir ta première PR

### Choisir un ticket "good-first-issue"

Labels GitHub : `good-first-issue` ou `docs`. Demande sur `#kubemanager-dev` si rien te tente.

### Workflow git

```sh
git checkout -b feat/<court-slug>
# ... modifs ...
git add -p                      # stage par hunk, relis ce que tu commit
git commit -m "feat(auth): description courte"
git push origin feat/<court-slug>
```

### Convention de commit

On suit [Conventional Commits](https://www.conventionalcommits.org/) :

| Préfixe | Quand |
|---------|-------|
| `feat(scope):` | nouvelle fonctionnalité |
| `fix(scope):` | bug |
| `refactor(scope):` | changement sans impact fonctionnel |
| `docs(scope):` | doc uniquement |
| `test(scope):` | tests uniquement |
| `chore(scope):` | build, deps, outillage |

Le `scope` = module/paquet concerné (`auth`, `workloads`, `chart`, `docs`, …).

### Check-list avant d'ouvrir la PR

- [ ] `go test ./internal/... && go vet ./... && go build ./...` passent
- [ ] `helm lint chart/kubemanager-backend` si tu as touché au chart
- [ ] Nouveaux endpoints : un test de service ajouté
- [ ] Nouveaux workflows : un test `temporalsdk/testsuite` ajouté
- [ ] Doc à jour si tu changes un invariant ou ajoutes une page
- [ ] Commit signé (`git commit -s`)

### Template PR

Titre : `feat(auth): support login by email alias`

Description :
```markdown
## Contexte
Lien vers le ticket.

## Changements
- Ce que tu as modifié, en bullets courts.

## Tests
Comment tu as vérifié (commandes, screenshots si UI, logs).

## Impact
- Breaking change ? Migration requise ? Feature flag ?
```

## 6. Review

- Un reviewer au minimum. Pour les changements sensibles (auth, saga, chart) : deux reviewers.
- Les changements sur `migrations/` nécessitent la relecture d'un senior.
- Les commentaires `nit:` = optionnel. Les autres = à traiter.
- Les discussions résolues : celui qui les a ouvertes les marque `Resolved`.

## 7. Merge

Squash merge par défaut. Le titre du squash devient l'entrée visible dans `git log main`.

Après merge :
- La CI construit et push l'image Docker (`ghcr.io/thekrauss/kubemanager:<sha>`).
- Sur main, un bot crée un tag RC quotidien.
- Pour prod : tag manuel + GitHub Release (voir [runbook](../interne/runbook.md)).

## Prochaine étape

→ [Tour du code](03-tour-du-code.md)
