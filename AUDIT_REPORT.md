# Audit nocturne — betorunapi (2026-11-20)

Branche `audit/nuit-2026-11-20`, partie de `dev` (`d5c6813`). Rien n'a été poussé ni fusionné.
Go 1.27.4, module `github.com/thekrauss/kubemanager`.

## 1. Résumé

1. 27 commits, un par correction : 17 corrections (`fix`), 1 ajout (`Spawner`), 2 renforcements de tests, 7 nettoyages (style, code mort, lint). Les 5 derniers ont été faits le 2026-11-20 au matin, à la demande, après la nuit.
2. Concurrence : panique « send on closed channel » dans le pool de workers quand `Submit` croise `Shutdown` (corrigé, test `-race`).
3. Concurrence : l'accumulateur d'usage pouvait lancer des centaines de flushs simultanés sous charge (334 mesurés) ; il n'en lance plus qu'un.
4. Arrêt propre : les tâches de fond (synchro IAM, rotation STS, purges, boucles du worker) tournaient sur `context.Background()` et n'étaient jamais arrêtées ; elles ont maintenant un cycle de vie (annulation, attente bornée, `recover`).
5. Ressources : Redis, la base SQL et le cache ristretto (jusqu'à 1 Gio) n'étaient jamais fermés. L'appel `redis.Close()` existant visait un client global de beto-shared jamais initialisé.
6. Ressources : envoi SMTP sourd à l'annulation, goroutine bloquée à vie dans l'upload multipart de la CLI (gênant pour le serveur MCP), table `lastUsed` des clés API sans purge.
7. Sécurité et robustesse : la garde de facturation `CanRun` s'ouvrait si la base renvoyait une erreur ; elle reste maintenant fermée. `publicJWK` paniquait sur une clé hors P-256 et lisait les champs X/Y dépréciés.
8. Idiomes : `errors.Is` remplace `==` sur cinq sentinelles. Retours d'erreur toujours nuls supprimés, code mort retiré, gofmt appliqué.
9. Outils : golangci-lint passe de 222 constats à 0, et staticcheck de 21 à 0, grâce à `.golangci.yml`, versionné et commenté. govulncheck : aucun appel vulnérable.
10. goleak est actif sur les 15 paquets qui lancent des goroutines, tous propres. `go test -race -count=1 ./...` : 79 paquets OK, 0 échec ; build, vet, gofmt, staticcheck, golangci-lint et govulncheck au vert.

## 2. Constats

Gravité : **haute** (panique, garde de sécurité, fuite en production), **moyenne** (fuite bornée, arrêt sale), **basse** (idiome, lisibilité).

| fichier:ligne | catégorie | gravité | statut | commit |
|---|---|---|---|---|
| internal/core/workerpool/pool.go:93 | concurrence (send on closed channel, data race) | haute | corrigé | `ba9c53d` |
| internal/modules/auth/service/usage_accumulator.go:83 | concurrence (flushs non bornés) | haute | corrigé | `901ddff` |
| internal/infrastructure/router/lifecycle.go:15 | goroutines sans arrêt (synchro IAM, STS, purges, worker) | haute | corrigé | `b45df3d` |
| internal/modules/billing/service/phase4.go:179 | sécurité (garde ouverte sur erreur SQL) | haute | corrigé | `cc28faf`, `22c55d1` |
| internal/infrastructure/router/app.go:274 | ressources (Redis, sql.DB, ristretto jamais fermés) | haute | corrigé | `2fb3701` |
| internal/infrastructure/mail/mail.go:96 | contexte ignoré (SMTP) | moyenne | corrigé | `467f490` |
| internal/cli/api/client.go:171 | fuite de goroutine (io.Pipe) | moyenne | corrigé | `b4b20ea` |
| internal/iam/serviceaccounts.go:455 | cache sans purge (`lastUsed`) | moyenne | corrigé | `ff22bba` |
| internal/iam/sts/keys.go:323 | API dépréciée + panique possible | moyenne | corrigé | `2a22389` |
| internal/infrastructure/router/server.go:198 | API (retours d'erreur toujours nuls) | basse | corrigé | `d81e5fe` |
| internal/core/cache/redis.go:60 (+ cache.go, idempotence/worker.go:67, router/bind.go:32, auth/repository/members_iam.go:161) | idiome (`==` sur erreurs) | basse | corrigé | `e10daff` |
| internal/iam/policy.go:509 | ressources (`rows.Close` manuel) | basse | corrigé | `4d93213` |
| cmd/dev-docs/main.go:727 | erreur ignorée sur écriture (`Close`) | basse | corrigé | `5ad046f` |
| internal/iam/sts/federation_e2e_test.go, sts_e2e_test.go | tests (statut HTTP lu puis ignoré) | basse | corrigé | `af367f2` |
| 6 fichiers (configs, idempotence, temporal, security, auth/repository, clusters) | gofmt | basse | corrigé | `ee5ad66` |
| internal/core/i18n/extract/extract.go:151, projects/repository/repository.go:15, builds/service/service.go:333, … | staticcheck (ST1015, S1039, SA1012, ST1019, SA4006) | basse | corrigé | `f607b0d` |
| auth/service/apikey.go, identity/service/login.go + service.go, workloads/service/deploy.go | code mort (U1000) | basse | corrigé | `951f636` |
| internal/modules/workloads/service/deploy.go:337 | ineffassign | basse | corrigé | `e2fd9a2` |
| errcheck (189 constats) | erreurs ignorées | basse | configuré + revu | `856c206` |
| cli/builds.go, cli/databases.go, keycloak/directory.go, github test | staticcheck QF/ST1023 | basse | corrigé | `78dcd52` |
| internal/modules/billing/domain/plans.go:173, projects/service/limits.go:87 | S1016 | basse | conservé exprès (voir §3) | `78dcd52` |
| 15 paquets (`leak_test.go`) | tests (goleak) | — | ajouté | `00f6aea` |
| internal/modules/diskresize/resize.go (`phase`) | perte de données possible (étape exécutée sans phase enregistrée, puis reprise destructrice) | haute | corrigé | `32781ed` |
| internal/core/background/background.go | goroutines des modules non suivies à l'arrêt | moyenne | `Spawner` ajouté | `5c48a26` |
| internal/modules/alerts/alerts.go:100 | goroutine non suivie à l'arrêt | basse | corrigé | `cd5f1f5` |
| internal/modules/addons/backup.go:237 | goroutine non suivie ; sauvegarde « en cours » pendant 7 jours après un arrêt | moyenne | corrigé | `31e7d51` |
| internal/modules/diskresize/resize.go:222 | goroutine non suivie à l'arrêt | basse | corrigé (suivie, pas annulée) | `0206d34` |
| identity/service/security_notice.go:28 | e-mail de sécurité non suivi à l'arrêt | basse | ouvert (voir §4) | — |
| iam/serviceaccounts.go:443, middleware/apikey_auth.go:52, security/middleware.go:176 | écritures `last_used_at` non suivies (≤ 5 s) | basse | laissé tel quel | — |
| internal/core/cache/redis.go:37 | contextcheck | — | faux positif (garde `ctx == nil`) | — |
| internal/iam/sts/federation_e2e_test.go:210 | bodyclose | — | faux positif (la requête doit échouer) | — |
| boucles avec `time.After` (router, temporal…) | fuite de timer supposée | — | sans objet depuis Go 1.23 | — |

## 3. Décisions à valider

1. **`CanRun` (facturation) reste fermée sur une erreur de lecture** (`cc28faf`). Avant, une panne de base laissait démarrer des conteneurs ; maintenant, elle bloque les démarrages tant qu'elle dure. Seul le cas « projet inconnu » (`ErrProjectUnknown`) laisse passer, comme avant.
2. **Ordre de l'arrêt** (`b45df3d`, `2fb3701`) : serveurs HTTP/gRPC, puis tâches de fond (attente bornée par `Server.ShutdownTimeout`, 10 s par défaut), accumulateur, pool, puis Redis, cache et base. Les trois goroutines non suivies du §4 peuvent maintenant voir une base fermée si elles tournent encore. Avant, le processus se terminait de toute façon en les tuant : le résultat est le même, mais il peut y avoir quelques lignes d'erreur en plus dans les logs.
3. **`.golangci.yml`** (`856c206`) : errcheck ignore :
   - `fmt.Fprint*` ;
   - les fermetures de lecture et de socket ;
   - `os.Remove` ;
   - les fichiers `_test.go`.

   J'ai vérifié à la main que les chemins d'écriture contrôlent `Close` (cli/config, cli/iam, archives, et dev-docs corrigé). La règle est écrite dans le fichier. staticcheck : QF1001 (loi de De Morgan) et QF1008 (sélecteur de champ embarqué) sont désactivés, car ces réécritures sont jugées moins lisibles.
4. **S1016 conservé** : `Plan.Admin()` et `SetLimits` copient champ par champ, exprès. Une conversion de type exposerait sans bruit tout champ ajouté au type source. Les deux directives (`//lint:ignore` pour staticcheck, `//nolint` pour golangci) sont justifiées en commentaire.
5. **Mail** : la sémantique de l'échéance est inchangée (celle du contexte si elle existe, sinon `cfg.Timeout`). J'ai seulement ajouté le respect de l'annulation (`context.AfterFunc`).
6. **`RegisterSchema` et `RegisterRoutes` ne rendent plus d'erreur** : elles rendaient toujours nil. Ce sont des signatures internes, et les appelants ont été adaptés.
7. **`publicJWK` rend une erreur** hors P-256. Une ligne de clé corrompue fait donc échouer `Reload` (l'ancien jeu de clés reste en place) au lieu de faire paniquer le processus.
8. **Nouvelle dépendance de test : goleak v1.3.0**, autorisée par la consigne. `go mod tidy` a aussi reclassé `cbor` en dépendance directe.
9. **Constat laissé tel quel** : les boucles avec `time.After` ne fuient plus depuis Go 1.23 (le GC ramasse le timer). Je ne les ai pas réécrites.
10. **Cas nilerr et errorlint jugés intentionnels**, laissés tels quels :
    - anti-énumération dans emailflow ;
    - webhooks ignorés volontairement ;
    - `p.Parse` dans iam ;
    - double wrap `%w: %v` (social.go:193, authflow.go:131, diskresize/resize.go:305) ;
    - comparaison de sentinelle dans errors.go:330.
11. **Nouveau constructeur `NewRistrettoAuthCacheWithSize`** pour les tests : le cache de production (1e7 compteurs, 1 Gio) met environ 1 s à se fermer sous `-race`.

## 4. Problèmes non résolus

Aucune correction n'a été annulée : aucune n'a demandé trois tentatives.

Les trois goroutines non suivies de la première version de ce rapport (alertes, sauvegarde manuelle, redimensionnement) sont maintenant traitées. Je les ai reprises le matin, à la demande. Choix retenus :
- **alertes** : l'envoi est suivi par l'arrêt mais pas annulé ; il se termine dans le délai d'arrêt.
- **sauvegarde manuelle** : annulée à l'arrêt, elle enregistre « interrompue » pendant que la base est encore ouverte. Après un arrêt brutal, la liste affiche « interrompue » au lieu de « en cours » pour toute copie démarrée depuis plus d'une heure (`backupStale`).
- **redimensionnement de disque** : suivi mais jamais annulé, car couper une copie ou un échange de disque serait pire. Si l'arrêt dure plus que le délai, la reprise existante prend le relais. En le relisant, j'ai trouvé et corrigé un défaut plus grave, présent avant l'audit : une étape pouvait s'exécuter sans que sa phase soit enregistrée, et la reprise supprimait alors le disque temporaire qui portait les données (`32781ed`).

Restent ouverts :
- **E-mail de sécurité** (`identity/service/security_notice.go:28`) : il part encore dans une goroutine non suivie, si bien qu'un arrêt peut le perdre. À brancher sur le `Spawner` comme les alertes.
- **Écritures `last_used_at`** (trois endroits, 5 s au plus) : non suivies. Perdre une mise à jour à l'arrêt est sans conséquence.
- **noctx (81 constats, presque tous dans des tests)** et **contextcheck (10, des faux positifs pour la plupart)** : non traités. Ces linters ne font pas partie de la configuration par défaut.
- **govulncheck** : GO-2026-5932 (`golang.org/x/crypto/openpgp`) se trouve dans un module requis, mais n'est appelé nulle part. À surveiller lors de la prochaine montée de x/crypto.
- **Mesure de référence** : `go test -race` tournait encore quand j'ai commencé à modifier le pool de workers. La référence reste valable : elle a été prise sur un worktree séparé.

## 5. Recommandations d'architecture

1. **`background.Spawner` partout.** L'interface existe et sert déjà aux alertes, aux addons et à diskresize. Tout nouveau travail en arrière-plan devrait passer par elle ; on pourrait l'imposer par une règle de lint (forbidigo sur `go func` hors `internal/core`).
2. **Un registre de fermeture.** `shutdownInfra` ferme les ressources à la main, dans un ordre implicite. Une liste `closers` alimentée à l'initialisation (dans l'ordre inverse) éviterait les oublis du type Redis, base et cache.
3. **Plus de client global partagé.** Le paquet `beto-shared/pkg/redis` expose un `Client` global : c'est ce qui rendait le `Close` silencieusement inutile. Le dépôt n'utilise plus ce paquet ; il faudrait l'interdire (depguard) pour ne pas y revenir.
4. **Cache d'authentification surdimensionné.** 1e7 compteurs et 1 Gio de coût maximal pour des claims et des validations de clés. À rendre configurable et à dimensionner selon la charge réelle.
5. **CI** : `go test -race -count=1 ./...`, `golangci-lint run` (la configuration est versionnée), `staticcheck`, `govulncheck` et `gofmt -l`. goleak est déjà branché dans les paquets concernés.
6. **Linters à activer progressivement** : errorlint, nilerr et contextcheck, avec des exclusions ciblées pour les cas intentionnels listés au §3.

## 6. Commandes de vérification

```sh
cd ~/betorun/betorunapi
git switch audit/nuit-2026-11-20
git log --oneline dev..HEAD          # 27 commits + rapport

# Machine à ~4 Go libres : limiter le parallélisme
export GOMAXPROCS=3 GOFLAGS=-p=3 GOMEMLIMIT=3GiB

go build ./... && go vet ./...
go test -race -count=1 ./...
test -z "$(gofmt -l ./internal ./cmd)"
staticcheck ./...
golangci-lint run ./...              # lit .golangci.yml
govulncheck ./...

# Tests ajoutés par l'audit
go test -race -count=1 -run 'TestSubmitDuringShutdown|TestAccumulator_Backpressure|TestShutdownInfra|TestConsumeInvalidations|TestPublicJWK|TestShouldTouch|TestUpload_NoWriterLeak' \
  ./internal/core/workerpool ./internal/modules/auth/service ./internal/infrastructure/router \
  ./internal/iam/sts ./internal/iam ./internal/cli/api
go test -race -count=1 ./internal/modules/billing/service ./internal/infrastructure/mail
go test -race -count=1 -run 'TestSpawner|TestNotifier_Background|TestBackup_(Shutdown|Stale)|TestResize_(Unrecorded|SpawnerTracks)' \
  ./internal/infrastructure/router ./internal/modules/alerts ./internal/modules/addons ./internal/modules/diskresize
```
