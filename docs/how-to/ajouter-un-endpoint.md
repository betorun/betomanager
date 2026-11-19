# Ajouter un endpoint HTTP

> De zéro à un endpoint tonic qui répond, avec validation, tests et doc OpenAPI générée.

## Exemple : `POST /workloads/:id/annotations`

## 1. DTO

Dans le module concerné, ajouter le struct d'entrée et de sortie.

`internal/modules/workloads/domain/dto.go` :

```go
type SetAnnotationsRequest struct {
    ID     string            `path:"id" binding:"required"`
    Body   SetAnnotationsBody `json:",inline"`
}

type SetAnnotationsBody struct {
    Annotations map[string]string `json:"annotations" binding:"required"`
}

type SetAnnotationsResponse struct {
    WorkloadID string `json:"workload_id"`
    Count      int    `json:"count"`
}
```

**Tags qui comptent** :
- `path:"id"` — path parameter.
- `query:"name"` — query string.
- `json:"foo"` — body JSON.
- `json:",inline"` — fusionne le sub-struct dans le body (pas de nesting).
- `binding:"required"` — validation par tonic.
- `description:"..."` — documentation OpenAPI, en français et sans jargon interne ; traduction anglaise dans `internal/core/i18n/catalog/apidoc.json`, comme pour la description de la route et de sa section (`TestPublicDoc_EveryTextIsTranslated` échoue sinon).

## 2. Interface du service

Dans `internal/modules/workloads/service/interface.go`, ajouter la méthode :

```go
type IWorkloadService interface {
    // ...
    SetAnnotations(ctx context.Context, id uuid.UUID, annotations map[string]string) (int, error)
}
```

## 3. Implémentation du service

Dans `internal/modules/workloads/service/lifecycle.go` :

```go
func (s *WorkloadService) SetAnnotations(
    ctx context.Context,
    id uuid.UUID,
    annotations map[string]string,
) (int, error) {
    w, err := s.Repo.GetByID(ctx, id)
    if err != nil {
        return 0, err
    }
    // effet externe ici (ex: patch K8s, update DB...)
    return len(annotations), nil
}
```

Si le service utilise Temporal → voir [ajouter-un-workflow-temporal.md](ajouter-un-workflow-temporal.md).

## 4. Controller

Dans `internal/modules/workloads/workloads.go`, ajouter la méthode sur `WorkloadController` **et** l'ajouter à l'interface `IWorkloadController` juste au-dessus :

```go
type IWorkloadController interface {
    // ...
    SetWorkloadAnnotations(c *gin.Context, in *domain.SetAnnotationsRequest) (*domain.SetAnnotationsResponse, error)
}

func (h *WorkloadController) SetWorkloadAnnotations(
    c *gin.Context,
    in *domain.SetAnnotationsRequest,
) (*domain.SetAnnotationsResponse, error) {
    id, err := uuid.Parse(in.ID)
    if err != nil {
        return nil, errors.New("invalid workload id")
    }
    count, err := h.WorkloadService.SetAnnotations(
        c.Request.Context(),
        id,
        in.Body.Annotations,
    )
    if err != nil {
        return nil, err
    }
    return &domain.SetAnnotationsResponse{
        WorkloadID: in.ID,
        Count:      count,
    }, nil
}
```

## 5. Route

Dans [internal/infrastructure/router/route.go](../../internal/infrastructure/router/route.go) (fonction `addWorkloadRoutes`) :

```go
WorkloadGroup.AddRoute(
    "/:id/annotations", http.MethodPost,
    "Set annotations K8s sur le workload",
    tonic.Handler(r.SetWorkloadAnnotations, http.StatusOK),
)
```

Premier arg : path (relatif au groupe `/workloads`).
Dernier arg : status code HTTP si succès.

## 6. Permissions (si applicable)

Si l'endpoint nécessite un droit projet, ajouter le check dans le service :

```go
ok, err := s.authRepo.CheckProjectPermission(
    ctx, userID, projectID,
    domain.PermWorkloadWrite,
)
if !ok || err != nil {
    return 0, betoerrors.New(betoerrors.CodeForbidden, "missing workload:write")
}
```

Ou côté route si c'est un guard groupe (`RequireGlobalRole`, `RequireAPIKeyKind`).

## 7. Test

Fichier `service/lifecycle_test.go` — suivre le pattern existant (fake repo). Au minimum :
- happy path (annotations set OK).
- workload not found → erreur.
- cas particulier (ex: annotations vides).

## 8. Vérifier OpenAPI + doc

```sh
go run ./cmd/gateway      # démarrer le server
open http://localhost:8080/docs          # Scalar
open http://localhost:8080/swagger/      # Swagger (basic auth admin/admin)
```

L'endpoint doit apparaître avec la bonne description, les path/body params corrects, et les response schemas (200 + 4xx par défaut).

## 9. Update la doc du module

Si c'est un ajout notable : ajouter une ligne dans la table "Endpoints HTTP" de [modules/workloads.md](../modules/workloads.md).

## Checklist avant PR

- [ ] DTO + tags tonic valides (path/query/json + binding).
- [ ] Méthode ajoutée à l'interface service **et** controller.
- [ ] Route déclarée dans `route.go`.
- [ ] Guard permission posé si non-public.
- [ ] Test service écrit (au minimum : happy + 1 erreur).
- [ ] `go build && go vet && go test ./internal/...` OK.
- [ ] OpenAPI visible dans `/docs`.
- [ ] Doc `modules/<x>.md` mise à jour.
