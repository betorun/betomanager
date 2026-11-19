# Ajouter un workflow Temporal

> Quand l'action touche > 1 système, dure > quelques secondes, ou doit résister à un crash du serveur.

Prérequis : lire [architecture/temporal-sagas.md](../architecture/temporal-sagas.md).

## Étapes

### 1. Définir l'Input/Result du workflow

Fichier `internal/modules/<module>/workflows/<action>.go` :

```go
package workflows

import (
    "time"
    "go.temporal.io/sdk/temporal"
    "go.temporal.io/sdk/workflow"
)

type BackupWorkloadInput struct {
    WorkloadID string
    ProjectID  string
    StorageURL string
}

type BackupWorkloadResult struct {
    BackupID   string
    SizeBytes  int64
    DurationMs int64
}
```

**Règle** : les inputs/outputs doivent être serializables (ni pointeurs vers interfaces, ni channels, ni `time.Time` avec timezones locales).

### 2. Écrire le workflow

```go
func BackupWorkloadWorkflow(ctx workflow.Context, input BackupWorkloadInput) (BackupWorkloadResult, error) {
    opts := workflow.ActivityOptions{
        StartToCloseTimeout: 10 * time.Minute,
        RetryPolicy: &temporal.RetryPolicy{
            InitialInterval: time.Second,
            MaximumAttempts: 3,
        },
    }
    ctx = workflow.WithActivityOptions(ctx, opts)

    var acts *activities.WorkloadBackupActivities
    var snapshot SnapshotMeta
    if err := workflow.ExecuteActivity(ctx, acts.CreateSnapshot, input.WorkloadID).Get(ctx, &snapshot); err != nil {
        return BackupWorkloadResult{}, err
    }

    var result BackupWorkloadResult
    if err := workflow.ExecuteActivity(ctx, acts.UploadToStorage, snapshot, input.StorageURL).Get(ctx, &result); err != nil {
        // Compensation : supprimer le snapshot local créé.
        _ = workflow.ExecuteActivity(ctx, acts.DeleteSnapshot, snapshot.ID).Get(ctx, nil)
        return result, err
    }

    return result, nil
}
```

**Règles workflow** :

- Pas d'appel direct K8s/DB dans le workflow — tout passe par une activity.
- Pas de `time.Now()` — utiliser `workflow.Now(ctx)`.
- Pas de `rand` — utiliser `workflow.SideEffect` ou un UUID passé en input.
- Les activities sont déclarées via un pointeur nil sur la struct (`var acts *X`) — Temporal résout par reflection le nom de la méthode.

### 3. Écrire les activities

Fichier `internal/modules/<module>/activities/<topic>.go` :

```go
package activities

import (
    "context"
    "time"
    idempotency "github.com/thekrauss/kubemanager/internal/infrastructure/idempotence"
)

type WorkloadBackupActivities struct {
    DB          *gorm.DB
    S3Client    *s3.Client
    Idempotency idempotency.AtomicManager
    Logger      *zap.SugaredLogger
}

func (a *WorkloadBackupActivities) CreateSnapshot(ctx context.Context, workloadID string) (SnapshotMeta, error) {
    payload := struct{ WorkloadID string }{workloadID}
    result, _, err := idempotency.ExecuteActivity(ctx, a.Idempotency, payload,
        idempotency.ActivityOptions{ProcessingTTL: 2 * time.Minute},
        func() (SnapshotMeta, error) {
            // effet externe ici
            return SnapshotMeta{ID: "...", SizeBytes: 42}, nil
        },
    )
    return result, err
}
```

**Règles activity** :

- Toujours encapsuler dans `idempotency.ExecuteActivity` si l'effet est externe (DB/K8s/HTTP).
- Le `ctx` est obligatoirement `context.Context` (pas `any`) — Temporal reconnaît le type.
- Retourner des types **serializables**. Pas de `*gorm.DB` en output.

### 4. Enregistrer workflow + activities dans le worker

Dans [infrastructure/temporal/processor.go](../../internal/infrastructure/temporal/processor.go) :

```go
func (p *TemporalProcessor) registerWorkflows(w worker.Worker) {
    // ...
    w.RegisterWorkflow(workloadWorkflows.BackupWorkloadWorkflow)
}

// Et dans Start() :
backupActs := &workloadActivities.WorkloadBackupActivities{
    DB:          p.cfg.DB,
    S3Client:    p.cfg.S3,
    Idempotency: p.cfg.Idempotency,
    Logger:      p.cfg.Logger,
}
p.registerActivities(w, projDBActs, projK8sActs, workloadDBActs, helmActs, backupActs)
```

### 5. Appeler le workflow depuis le service

Dans le service concerné :

```go
handle, err := s.Distributor.ExecuteWorkflow(ctx,
    temporal.ExecuteOptions{WorkflowID: "workload-backup-" + id.String()},
    workflowsPkg.BackupWorkloadWorkflow,
    workflowsPkg.BackupWorkloadInput{
        WorkloadID: id.String(),
        ProjectID:  projectID,
        StorageURL: "s3://backups/",
    },
)
if err != nil {
    return nil, err
}
// handle.WorkflowID + handle.RunID sont exploitables pour polling / signals.
```

### 6. Tests

Fichier `workflows/<action>_test.go` — pattern :

```go
func TestBackupWorkloadWorkflow_HappyPath(t *testing.T) {
    var ts testsuite.WorkflowTestSuite
    env := ts.NewTestWorkflowEnvironment()

    fake := &fakeBackupActs{}
    env.RegisterActivity(fake.CreateSnapshot)
    env.RegisterActivity(fake.UploadToStorage)
    env.RegisterActivity(fake.DeleteSnapshot)

    env.ExecuteWorkflow(BackupWorkloadWorkflow, BackupWorkloadInput{
        WorkloadID: "wl-1",
        ProjectID:  "p-1",
        StorageURL: "s3://x",
    })

    require.True(t, env.IsWorkflowCompleted())
    require.NoError(t, env.GetWorkflowError())
}
```

**Piège fréquent** : les méthodes du fake DOIVENT avoir `ctx context.Context` (pas `_ any`). Sinon Temporal ne les matche pas correctement.

### 7. Ajouter un test d'erreur et de compensation

Au minimum : un test où une activity échoue, vérifier que la compensation s'exécute.

## Retry / idempotence — check-list

- [ ] Chaque activity externe est enveloppée par `idempotency.ExecuteActivity`.
- [ ] L'input d'une activity est **déterministe** (pas de timestamp non-stable, pas de UUID généré dedans).
- [ ] Retry policy configurée (3 tentatives par défaut, à ajuster si coût externe).
- [ ] Compensation définie pour chaque activity à effet externe.
- [ ] Workflow testé avec `testsuite` (happy + au moins 1 échec).

## Après merge

1. Redéployer le worker : `kubectl -n kubemanager rollout restart deploy/kubemanager-worker`.
2. Vérifier les métriques `temporal_workflow_*{workflow_type="BackupWorkloadWorkflow"}` dans Grafana.
3. Faire un smoke : lancer un workflow et vérifier qu'il termine (Temporal UI).

## Pour aller plus loin

- [Architecture — sagas](../architecture/temporal-sagas.md) — pattern compensation.
- [Module workloads](../modules/workloads.md) — exemple de 2 workflows complets (Deploy + Delete).
