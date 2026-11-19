# Ajouter une migration SQL

> `golang-migrate` est câblé à l'init — chaque démarrage applique les migrations pending. Fichiers dans [migrations/](../../migrations/).

## Nommage

```
migrations/
├── 000001_init.up.sql
├── 000001_init.down.sql
├── 000002_add_api_keys_kind.up.sql          ← TA NOUVELLE MIGRATION
├── 000002_add_api_keys_kind.down.sql
```

**Format** : `NNNNNN_<slug>.up.sql` + `.down.sql`. Le numéro doit être **strictement supérieur** à la dernière migration existante. Jamais renuméroter une migration déjà commitée.

## Golden rules

1. **Idempotence** : utiliser `IF NOT EXISTS`, `IF EXISTS`, `DROP ... IF EXISTS`. Une migration doit pouvoir être réjouée sans casser.
2. **Réversible** : le `.down.sql` doit annuler le `.up.sql`. Même si en prod on ne "down" jamais, c'est une discipline qui force à réfléchir.
3. **Pas de data lourde dans les migrations** — les seeds et backfills vont dans un script dédié (`scripts/seed/*.go`), la migration pose juste le schéma.
4. **Pas de `DROP COLUMN` sans plan de coexistence** — toujours en 2 temps : 1) l'app n'utilise plus la colonne, 2) migration qui la drop. Sinon un rollback déploiement casse la DB.
5. **Indexes concurrents** — si la table est grosse (> 1M rows), utiliser `CREATE INDEX CONCURRENTLY` pour ne pas lock l'écriture.

## Exemple — ajouter une colonne

`migrations/000002_add_workload_annotations.up.sql` :

```sql
-- Add free-form annotations JSON on workloads.
-- Used by the new SetAnnotations endpoint (see how-to/ajouter-un-endpoint.md).

ALTER TABLE workloads
    ADD COLUMN IF NOT EXISTS annotations JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS idx_workloads_annotations_gin
    ON workloads USING GIN (annotations);
```

`migrations/000002_add_workload_annotations.down.sql` :

```sql
DROP INDEX IF EXISTS idx_workloads_annotations_gin;
ALTER TABLE workloads DROP COLUMN IF EXISTS annotations;
```

## Update le GORM entity correspondant

`internal/modules/workloads/domain/entity.go` :

```go
type Workload struct {
    // ...
    Annotations datatypes.JSON `gorm:"type:jsonb;default:'{}'::jsonb;not null"`
}
```

Nécessite `gorm.io/datatypes`. L'alternative est un `string` qu'on JSON-unmarshal côté service.

## Tester localement

```sh
# Reset DB (dev uniquement — perds tout)
make dev-reset

# Ou, si tu veux garder les autres tables :
docker exec -it kubemanager-k8sdb-1 psql -U kubemanager -d kubemanager \
  -f /migrations/000002_add_workload_annotations.up.sql
```

Au boot, kubemanager applique les migrations via `golang-migrate` au démarrage. Vérifier les logs :

```sh
docker compose logs kubemanager | grep -i migration
```

## Schéma de suivi

La table `schema_migrations` (créée auto par `golang-migrate`) stocke :
- `version` : numéro de la dernière migration appliquée.
- `dirty` : `true` si la migration précédente a échoué à mi-chemin.

**Dirty = 1 = bloqué**. Pour sortir :

1. Corriger la migration (un fix côté SQL ou un nouveau numéro de migration qui complète l'action).
2. `UPDATE schema_migrations SET dirty = FALSE, version = <N-1>;` — revient à l'état d'avant.
3. Relancer.

## Checklist

- [ ] Nom `NNNNNN_<slug>.{up,down}.sql`, numéro > dernier.
- [ ] `.up.sql` idempotent (`IF NOT EXISTS`/`IF EXISTS`).
- [ ] `.down.sql` annule le `.up.sql`.
- [ ] GORM entity synchro avec le schéma.
- [ ] Test manuel en dev : `make dev-reset` OU application manuelle.
- [ ] Si data lourde : pas dans la migration, script dédié.
- [ ] Si table > 1M rows : `CREATE INDEX CONCURRENTLY`.
- [ ] Review par un senior avant merge (règle du runbook).

## Après merge — prod

Déploiement standard : kubemanager applique la migration au démarrage. Points d'attention :

- **Rolling update** : pendant que la migration applique, l'ancien pod peut encore tourner. Si la migration ajoute une column NOT NULL sans DEFAULT, l'ancien pod cassera sur les INSERT → toujours avec DEFAULT, ou NULL-able d'abord.
- **Temps d'application** : pour une grosse table, l'app reste bloquée sur son démarrage jusqu'à la fin. Vérifier les timeouts readiness/liveness du Deployment.
- **Plan de rollback** : si prod casse, rollback = `helm rollback` + run manuel du `.down.sql`. Éviter — préparer une migration `000003_fix.up.sql` qui remet en état sans toucher à `000002`.
