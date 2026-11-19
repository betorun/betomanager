# Helm chart hardening (PSA restricted)

> Le chart `standard-app` est PSA restricted-compliant + HPA + PDB + NetworkPolicy egress.

## Conformité Pod Security Admission

- `runAsNonRoot: true`
- `runAsUser`, `runAsGroup` > 0
- `allowPrivilegeEscalation: false`
- `readOnlyRootFilesystem: true`
- `capabilities.drop: [ALL]`
- `seccompProfile.type: RuntimeDefault`

## Features

- **HPA** : min/max replicas + CPU target.
- **PDB** : `minAvailable: 50%` par défaut.
- **NetworkPolicy egress** : deny par défaut, allow DNS + kube-system.
- **ImagePullSecrets** : injectés par `projects.EnsureSecret`.
- **TLS** : cert-manager ClusterIssuer configurable.

## Fichiers

- `internal/infrastructure/helm/charts/standard-app/values.yaml`
- `internal/infrastructure/helm/charts/standard-app/templates/deployment.yaml`
- `internal/infrastructure/helm/charts/standard-app/templates/hpa.yaml`
- `internal/infrastructure/helm/charts/standard-app/templates/pdb.yaml`
- `internal/infrastructure/helm/charts/standard-app/templates/networkpolicy.yaml`

> **Stub** — ajouter la checklist de validation pre-prod + les tests avec `helm template | kube-linter`.
