# ADR 0006 — Rate limit backé par Redis uniquement

- **Status** : accepté
- **Date** : 2026-04-15

## Contexte

La première implémentation du rate limit utilisait `golang.org/x/time/rate` — token bucket **in-memory** par IP. Simple, rapide, mais :

- **Non partagé** entre replicas : avec 3 pods kubemanager derrière un LB, un client peut faire 3× le quota.
- **Perd les compteurs** au restart.
- **Peut dériver** en consommation mémoire (éviction stale entries nécessaire).

En passant en production multi-replica, ce choix devient un bug critique.

## Décision

**Rate limit backé par Redis**, **obligatoire** quand `RateLimitConfig.Enabled=true`. Fixed-window via script Lua atomique (`INCR` + `PEXPIRE NX`).

Implémentation : [internal/middleware/rateLimit.go](../../internal/middleware/rateLimit.go).

## Conséquences

### Positives

- **Cohérent multi-replica** : 3 pods partagent le même compteur Redis → un client ne peut pas tricher en load-balancing.
- **Persistant** : un restart kubemanager ne reset pas les fenêtres — un attaquant ne peut pas "DoS then restart" pour reset le rate limit.
- **Script Lua atomique** : `INCR + EXPIRE NX` en une seule opération → pas de race window.
- **Fail-closed** : si Redis est injoignable, on retourne 503. Préserve la garantie de limit.
- **Pas d'éviction locale** à gérer — Redis expire auto la clé après la fenêtre (ex: 1 min).

### Négatives

- **Dépendance forte à Redis** : Redis down = pas de trafic. Compensé par Redis HA (Sentinel ou Cluster).
- **Latence Redis ajoutée à chaque requête** : ~1-5 ms pour le script Lua. Négligeable mais pas gratuit.
- **Cardinalité Redis** : une clé par `(ip, bucket)` → volume négligeable grâce au TTL auto.
- **IP masquée derrière proxy/LB** : `c.ClientIP()` dépend de `X-Forwarded-For`. Si mal configuré en prod, tout le trafic apparaît comme venant de l'IP du LB → rate limit global, pas par client.

## Alternatives considérées

### In-memory + sync périodique vers Redis

Rejeté. Complexité accrue pour un gain marginal (latence in-memory vs Redis = ~1 ms). Le vrai problème c'est la cohérence multi-replica, pas la latence.

### Token bucket Redis (au lieu de fixed-window)

Plus smooth que le fixed-window (évite les "bursts au reset de fenêtre"). Mais script Lua plus complexe, et on n'a pas observé de problème de burst en pratique.

Peut être revu si on a des use cases où le burst devient gênant.

### Sliding window log

Précis, mais cher en mémoire Redis (1 entrée par requête). Pour un quota "100 req/min par IP", ça stocke 100 timestamps par IP → trop lourd à l'échelle.

### Rate limit au niveau ingress (nginx, Envoy)

Valable en complément mais pas en remplacement — l'ingress ne voit pas le contexte applicatif (user authentifié, scope API key…). On garde le rate limit applicatif pour appliquer des limites plus fines.

### Fail-open si Redis down

**Rejeté** — la compromission du rate limit pendant une panne Redis ouvrirait une fenêtre à des attaques volumétriques. Préférable de refuser le trafic (503) et d'alerter oncall.

## Configuration

Dans `values.yaml` du chart :

```yaml
config:
  rateLimit:
    enabled: true
    limit: 300         # max req par fenêtre
    window: "1m"       # durée de la fenêtre
```

Valeurs par env :

| Env | Limit | Window |
|-----|-------|--------|
| dev | 1000 | 1 min |
| staging | 600 | 1 min |
| prod (par IP) | 300 | 1 min |
| prod (par API key) | 1000 | 1 min (à implémenter — voir TODO) |

## TODO / évolutions

- **Scope par API key** (pas juste par IP) : permet à un service CI avec IP partagée de ne pas être rate-limité à cause du trafic d'un voisin.
- **Scope par user authentifié** : même logique, une fois post-Auth.
- **Burst toléré** : passer en token bucket pour accepter 2× la limite en pic court.
- **Alerte dédiée** : `KubeManagerRateLimitBackendDown` si Redis tourne mais le script Lua fail de manière persistante.

## Contraintes opérationnelles

- Redis HA en prod obligatoire (Sentinel ou Cluster).
- Le bucket ID `bucketStart := time.Now().Truncate(window).Unix()` dépend de l'horloge locale — NTP sync est critique (sinon les pods calculent des buckets différents).
- Surveiller la cardinalité Redis via `redis-cli --bigkeys` — si on voit des TTLs > window, c'est un bug du script Lua.

## Références

- Implémentation : [middleware/rateLimit.go](../../internal/middleware/rateLimit.go)
- Alerte : `KubeManagerRateLimitStorm` dans [chart/kubemanager-backend/templates/prometheusrule.yaml](../../chart/kubemanager-backend/templates/prometheusrule.yaml)
- Sanity check multi-replica : faire `scripts/loadtest.sh` avec 2 replicas kubemanager, vérifier que les 429 sont équivalents à 1 replica.
