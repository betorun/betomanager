# KubeManager — Runbook oncall

> Document opérationnel. Mise à jour à chaque incident majeur. Propriétaire : équipe plateforme.

## Severity & SLA

| Sev | Définition | First response | Resolution SLA |
|-----|------------|----------------|----------------|
| **P1** | API indisponible OU perte de données en cours OU cluster K8s cible compromis | 15 min | 2 h |
| **P2** | Dégradation majeure : déploiements bloqués, realtime down, watcher offline cross-cluster | 30 min | 6 h |
| **P3** | Lenteur, dégradations par tenant, endpoint spécifique KO | 2 h (business hours) | 24 h |
| **P4** | Cosmétique, doc, backlog | n/a | quand possible |

## Checklist oncall (premier réflexe — 5 min)

1. **Quelle alerte ?** (Grafana / Pager). Dashboard principal : `grafana.internal/d/kubemanager-overview`.
2. **Scope** : 1 cluster ? 1 tenant ? global ?
3. **Health endpoint** :
   ```sh
   curl -sSf https://api.kubemanager/api/v1/health
   ```
   Sinon → P1, escalader + passer à "API totalement down" ci-dessous.
4. **Logs backend** :
   ```sh
   kubectl -n kubemanager logs -l app.kubernetes.io/component=server --tail=200 -f
   ```
5. **Taux d'erreur** : panel Grafana `API 5xx rate by endpoint` — identifier l'endpoint.
6. **Temporal** : UI sur `https://temporal-ui.kubemanager` → workflows en `Running`/`Failed`.
7. **Déclarer l'incident** (canal `#incidents-kubemanager`, thread dédié) avec sev + résumé.

## Playbooks incidents courants

### A. API totalement down (503/timeouts tous endpoints)

**Symptômes** : probes liveness failing, `kubectl get pods` en `CrashLoopBackOff`.

1. Logs pod : `kubectl -n kubemanager logs <pod> --previous`.
2. Causes fréquentes :
   - **Migration DB bloquée** : logs → `failed to run migrations`. Check Postgres up + `SELECT * FROM schema_migrations;` → si dirty = `1`, corriger manuellement.
   - **Redis down** : le rate limit fail-closed renvoie 503. Vérifier `redis-cli -h <host> ping`.
   - **Keycloak JWKS unreachable** : `jwksURL` unresolvable. Fallback : `kubectl -n kubemanager rollout restart deploy/kubemanager-server` après remise up Keycloak.
3. **Rollback** si un déploiement récent :
   ```sh
   helm -n kubemanager rollback kubemanager <revision>
   ```
4. Post-incident : annotate Grafana, créer ticket root-cause.

### B. Déploiements workloads coincés (PENDING éternel)

**Symptômes** : `POST /workloads` 202 mais `GET /workloads/:id/status` reste `STARTING` > 10 min.

1. **Temporal UI** : chercher workflow `deploy-workload-<id>`. Status `Running` ?
2. Activity stuck ?
   - `InstallChart` qui fail → logs worker `kubectl -n kubemanager logs -l app.kubernetes.io/component=worker`.
   - `helm upgrade` timeout → cluster cible saturé ? `kubectl -n km-<project> describe pods`.
3. **Retry** : reset le workflow depuis Temporal UI (bouton "Reset" → cause `manual retry after <cause>`).
4. **Abandon propre** :
   ```sh
   # Force DELETE via endpoint admin (si implémenté), sinon :
   helm -n km-<project> uninstall <release>
   # + marquer DELETED en DB
   psql -c "UPDATE workloads SET status='DELETED', deleted_at=NOW() WHERE id='<uuid>';"
   ```

### C. Realtime (Centrifugo) down — WebSockets KO

**Symptômes** : frontend reçoit pas les events déploiement. Backend logs : `centrifugo publish failed`.

- **Impact** : **pas de perte de données** (Publisher est fail-open). UX dégradée uniquement.
- Check : `curl -sf http://centrifugo:8000/health`.
- Rolling restart Centrifugo : `kubectl -n realtime rollout restart deploy/centrifugo`.
- Vérifier les 2 secrets HMAC côté kubemanager ET Centrifugo matchent (`CENTRIFUGO_TOKEN_HMAC_SECRET` / `CENTRIFUGO_TOKEN_HMAC_SECRET_KEY`).

### D. Watcher offline (staleness alert)

**Symptômes** : `GET /admin/clusters` → `last_seen_at` > 5 min, alerte Prometheus.

1. **Côté cluster géré** : `kubectl -n kubemanager-watcher logs deploy/watcher -f`.
   - Leader lost ? (lease K8s expiré) → rolling restart.
   - API key invalide (401 vers kubemanager) → rotation nécessaire.
2. **Côté kubemanager** :
   ```sh
   curl -H "Authorization: Bearer $ADMIN" https://api.kubemanager/kmanager/v1/admin/clusters | jq '.[] | select(.cluster_id=="<cluster>")'
   ```
3. **Rotation de la cluster key** :
   ```sh
   curl -X POST -H "Authorization: Bearer $ADMIN" \
     https://api.kubemanager/kmanager/v1/admin/cluster-keys/<key-id>/rotate \
     -d '{"grace_days":7}' | jq -r .api_key
   # Injecte la nouvelle clé dans les values du chart watcher, `helm upgrade`.
   ```

### E. Rate limit storm (429 massifs)

**Symptômes** : Grafana `rate_limit_exceeded` explose, utilisateurs reçoivent des 429.

1. Identifier l'IP/tenant responsable : metrics `rl:<ip>` dans Redis.
2. Attaque ? Fronter via WAF/CDN. Bannir l'IP au niveau L7 ingress.
3. Besoin légitime ? Upper la limite par env via values :
   ```yaml
   config:
     rateLimit:
       enabled: true
       limit: 600  # de 300 → 600 par défaut
   ```
4. `helm upgrade` + rolling.

### F. Postgres slow / lock contention

**Symptômes** : P95 latence API > 1s, timeouts DB dans les logs.

1. **Long-running queries** :
   ```sql
   SELECT pid, age(clock_timestamp(), query_start), query
   FROM pg_stat_activity
   WHERE state != 'idle' AND query_start < NOW() - INTERVAL '30 seconds'
   ORDER BY query_start ASC;
   ```
2. **Locks** :
   ```sql
   SELECT * FROM pg_locks WHERE NOT granted;
   ```
3. **Kill** si nécessaire : `SELECT pg_terminate_backend(<pid>);`.
4. **Hot row api_keys.usage_count** : vérifier que `BatchIncrementAPIKeyUsage` est bien actif (fix P4).
5. **Index manquant** : `EXPLAIN ANALYZE` sur la query lente, ajouter migration.

## Backup & Restore

### Backup Postgres (prod)

- **Logical dump** quotidien via CronJob K8s (`backups/kubemanager-dump-<date>.sql.gz`).
  ```yaml
  # kube-system/postgres-backup CronJob — rétention 30 jours S3.
  schedule: "0 2 * * *"
  ```
- **PITR** (Point In Time Recovery) : WAL archiving vers S3 — RPO **≤ 5 min**.
- Vérification mensuelle : restore sandbox + `SELECT count(*) FROM projects;`.

### Restore (procédure DR)

1. Provisionner un nouveau Postgres vide (même version).
2. `gunzip < backup.sql.gz | psql`.
3. Replay WAL si PITR actif : `pg_restore` + `recovery_target_time`.
4. Re-créer les ressources K8s (ExternalSecrets pull depuis Vault).
5. `helm upgrade kubemanager chart/kubemanager-backend` — migrations ré-apply (idempotent).

## DR plan — RTO 30 min

Cas : cluster de management (kubemanager backend) totalement perdu.

| Étape | Qui | Temps cible |
|-------|-----|-------------|
| 1. Déclarer P1 + lever standby region | oncall | 5 min |
| 2. Provisionner cluster K8s de secours (Terraform déjà prêt) | oncall + infra | 10 min |
| 3. Restore Postgres depuis backup S3 + WAL | oncall | 8 min |
| 4. Apply ExternalSecrets + helm install kubemanager | oncall | 5 min |
| 5. DNS switch api.kubemanager vers nouvelle IP | oncall | 2 min |

**Total : ≤ 30 min.**

**Points critiques** :
- Les watchers côté clusters gérés redétectent l'URL via DNS sans intervention.
- Temporal : workflows in-flight doivent être reprenable — Temporal reconnecte automatiquement (si historique préservé, c'est-à-dire si son Postgres est HA aussi).
- Sessions JWT actives continuent de valider (JWT stateless via JWKS Keycloak).

## Escalade

1. **L1 oncall** — Slack `#incidents-kubemanager`, PagerDuty primary.
2. **L2** (> 30 min sans résolution) — PagerDuty secondary, tech lead.
3. **L3 / CTO** — Sev P1 > 1h OU data loss OU compromission sécurité.

## Post-incident

- Timeline dans le ticket d'incident.
- Root cause analysis (5-Whys) sous 48h.
- Action items trackés dans le backlog avec label `post-incident`.
- Update ce runbook avec le nouveau playbook si applicable.

## Ressources

- Dashboard : `grafana.internal/d/kubemanager-overview`
- Logs : `logs.internal/app/kubemanager` (Loki)
- Temporal UI : `temporal-ui.kubemanager`
- Runbook source : `docs/interne/runbook.md` (ce fichier)
