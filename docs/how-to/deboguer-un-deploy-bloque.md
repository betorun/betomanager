# Déboguer un déploiement bloqué

> Le user dit "mon workload reste en STARTING depuis 10 min". Voici le cheminement.

## 1. Confirmer le symptôme

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://api.kubemanager/kmanager/v1/workloads/<id> | jq
```

Note :
- `status` : `STARTING`, `DEPLOYING`, `FAILED`…
- `current_phase` : `HELM_CHART_LOADING`, `HELM_INSTALLING`, `DEPLOYED`, `FAILED`…
- `namespace` : pour les commandes kubectl suivantes.

## 2. Regarder le workflow Temporal

Temporal UI : <http://localhost:8233> (dev) ou `temporal-ui.kubemanager` (prod).

Cherche le workflow `deploy-workload-<id>` :

- **Running** — soit l'activity `InstallChart` est encore en cours (attend le readiness des pods), soit elle a timeout.
- **Completed** — le workflow a fini OK mais la DB pas encore update ? Rare, check les logs worker.
- **Failed** — onglet "Stack Trace" donne la cause : Helm error, K8s error, timeout.

```sh
# Ou via CLI Temporal
temporal workflow describe --workflow-id deploy-workload-<id> \
  --namespace kubemanager
```

## 3. Regarder les logs du worker

```sh
# Dev
docker compose logs kubemanager --tail=200 | grep workflow_id

# Prod
kubectl -n kubemanager logs -l app.kubernetes.io/component=worker \
  --tail=200 | grep <workload_id>
```

Cherche les erreurs d'activity :
- `InstallChart failed` → problème Helm. Go to step 4.
- `EnsureSecret failed` → K8s API injoignable ou namespace qui n'existe pas (projet supprimé ?).
- `UpdateStatus failed` → DB injoignable.

## 4. Helm — voir l'état de la release

```sh
NAMESPACE=$(curl -s -H "Authorization: Bearer $TOKEN" \
  https://api.kubemanager/kmanager/v1/workloads/<id> | jq -r .namespace)

kubectl -n $NAMESPACE get all,ingress,pvc
helm -n $NAMESPACE list
helm -n $NAMESPACE history <release-name>
helm -n $NAMESPACE status <release-name>
```

Scenarios courants :

### 4a. Helm `pending-install` / `pending-upgrade`

Release bloquée parce qu'une opération précédente a crash en plein milieu. `helm history` montre la dernière révision en `pending-*`.

```sh
helm -n $NAMESPACE rollback <release> 0    # rollback à la précédente
# Si ça marche pas, reset manuel :
helm -n $NAMESPACE uninstall <release>
# Puis refaire un POST /workloads côté app
```

### 4b. Deployment avec ready=0/N

```sh
kubectl -n $NAMESPACE describe deployment <release>
kubectl -n $NAMESPACE get pods -l app=<release>
kubectl -n $NAMESPACE describe pod <pod-name>
```

Cherche dans `describe pod` → section "Events" :

| Event | Diagnostic |
|-------|-----------|
| `ImagePullBackOff` | image invalide, tag inexistant, registry auth KO. Vérifier `kubectl get secret` dans le namespace + `imagePullSecrets` du Deployment. |
| `CrashLoopBackOff` | container qui crash au boot. `kubectl logs <pod> --previous` pour voir pourquoi. |
| `CreateContainerConfigError` | typiquement un env var référence un secret manquant. Vérifier que `EnsureSecret` a bien tourné. |
| `Insufficient cpu / memory` | pas assez de ressources sur les nodes. Si trop strict → diminuer `requests` ou ajouter un node. |
| `OOMKilled` | container tué pour excès mémoire. Augmenter `memory_limit` via PATCH. |

### 4c. Pod Ready mais service pas accessible

```sh
kubectl -n $NAMESPACE get svc <release>
kubectl -n $NAMESPACE run -it --rm debug --image=curlimages/curl --restart=Never -- \
  curl -sv http://<release>.<namespace>.svc:<port>/
```

Vérifier :
- `targetPort` du Service = `containerPort` du Deployment.
- Le container écoute bien sur `0.0.0.0:<port>`, pas `127.0.0.1`.
- NetworkPolicy n'a pas bloqué — `kubectl -n $NAMESPACE describe networkpolicy <release>`.

### 4d. Ingress mais pas de TLS / URL inaccessible

```sh
kubectl -n $NAMESPACE describe ingress <release>
kubectl get certificate -A | grep <release>
```

Si `Ready=False` sur le Certificate → cert-manager / Let's Encrypt : rate limit, DNS non résolu, ACME challenge KO.

## 5. Côté DB — voir l'état exact

```sql
SELECT id, name, status, current_phase, last_workflow_id,
       created_at, updated_at
FROM workloads
WHERE id = '<uuid>';
```

Si `status=STARTING` et `updated_at > 10 min` sans changement, le workflow est probablement bloqué ou crashed sans avoir update.

## 6. Récupérer — les recours

### Option A : retry propre

Si Helm a failed → UPDATE l'image/config, puis `PATCH /workloads/:id` pour re-déclencher le workflow.

### Option B : reset manuel

Si release Helm coincée → `helm uninstall` manuellement + UPDATE DB :

```sql
UPDATE workloads SET status = 'DELETED', current_phase = 'MANUAL_CLEANUP'
WHERE id = '<uuid>';
```

Puis le user peut créer un nouveau workload.

### Option C : force delete

```bash
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  https://api.kubemanager/kmanager/v1/workloads/<id>
```

Si le DeleteWorkflow bloque → Temporal UI → "Reset" sur le workflow à l'étape qui bloque, ou "Terminate" + cleanup manuel (voir runbook).

## 7. Observabilité pour éviter le debug manuel

- **Grafana → Workloads dashboard** : latence POST /workloads, rate limit 429, logs Loki filtré par `workflow_id`.
- **Trace Jaeger** : si le workflow a démarré avec une trace, tu peux suivre toutes les activités + les spans DB/K8s.
- **Alertes PrometheusRule** : `WorkloadSagaDeleteFailures` détecte les DELETE_FAILED — si ça page régulièrement, c'est un bug à corriger, pas à contourner.

## Escalation

Si tu ne trouves pas → `#incidents-kubemanager` Slack avec :
- `workload_id`.
- `namespace` K8s cible.
- Lien Temporal UI vers le workflow.
- Logs pertinents (derniers 200 lignes avec le `trace_id`).
- Output de `kubectl describe pod <pod>`.

Un senior peut escalade L2 via runbook si besoin.
