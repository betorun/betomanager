# Setup local (10 min)

> Objectif : avoir kubemanager qui tourne sur ta machine, pouvoir appeler `POST /projects`, et voir les dashboards Grafana.

## Prérequis

- Linux ou macOS (Windows via WSL2 uniquement)
- Docker 24+ avec compose
- Go 1.25+
- `make`, `jq`, `curl`
- 8 Go de RAM libre (la stack complète pèse ~4 Go)

Vérifier :

```sh
docker --version        # 24+
go version              # 1.25+
jq --version
```

## 1. Cloner

```sh
git clone git@github.com:thekrauss/kubemanager.git
cd kubemanager
```

## 2. Lancer la stack

```sh
make dev-up
```

Ça démarre tout via `docker-compose.yaml` :

| Service | Port | Rôle |
|---------|------|------|
| kubemanager | 8080 | API + worker Temporal |
| k8sdb (Postgres) | 5433 | DB kubemanager (port host 5433 pour éviter conflit avec un Postgres local) |
| keycloak | 18081 | Auth |
| keycloakdb (Postgres) | — | DB Keycloak (pas exposé) |
| mailhog | 8025 | UI emails capturés |
| centrifugo | 8000 | WebSocket broker |
| temporal | 7233 | Workflows |
| prometheus | 9090 | Metrics |
| loki | 3100 | Logs |
| grafana | 3000 | Dashboards |

Le premier run télécharge les images — ~3-5 min. Relances suivantes : ~15 s.

## 3. Bootstrap Keycloak + user de test

```sh
make dev-bootstrap
```

Crée le realm `kubemanager`, le client `kubemanager-api`, le client ROPC `kubemanager-dev-cli`, et le user :

```
email    : test.admin@kubemanager.local
password : dev-password
role     : SUPER_ADMIN
```

## 4. Vérifier que tout est up

```sh
curl -sf http://localhost:8080/api/v1/health && echo OK
```

Tu dois voir `OK`. Sinon : `docker compose logs kubemanager` et regarde les erreurs.

## 5. Obtenir un token

```sh
TOKEN=$(bash scripts/keycloak/get_test_token.sh)
echo $TOKEN     # doit commencer par "eyJ..."
```

## 6. Créer un projet

```sh
curl -sS -X POST http://localhost:8080/kmanager/v1/projects \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"hello","description":"demo","cpu_limit":"1000m","memory_limit":"1Gi"}' \
  | jq .
```

Tu dois recevoir un `project_id` + `workflow_id`.

## 7. Ouvrir les dashboards

- **Grafana** : <http://localhost:3000> (login `admin` / `admin`) → folder "KubeManager" → 3 dashboards prêts.
- **Swagger API** : <http://localhost:8080/swagger/> (auth basique `admin`/`admin`, voir `docker-compose.yaml`).
- **Scalar API** : <http://localhost:8080/docs> (public, read-only).
- **MailHog** : <http://localhost:8025> (emails capturés en dev).
- **Temporal UI** : <http://localhost:8233> (si temporal-ui est dans ton compose).

## 8. Stopper proprement

```sh
make dev-down
```

Garde les volumes. Pour tout purger (fresh start) :

```sh
make dev-reset
```

## Ça marche pas — checklist

1. **Port déjà pris** (typiquement 5432 Postgres local, 8080 un autre dev) :
   ```sh
   ss -tlnp | grep -E ':(8080|5432|3000|6379)'
   ```
   Si conflit, adapte `docker-compose.yaml`.

2. **`make dev-bootstrap` échoue** avec 401 — Keycloak pas encore prêt. Attendre 10 s et relancer.

3. **`curl /health` renvoie 502** — kubemanager a crashé au boot. Regarde :
   ```sh
   docker compose logs kubemanager --tail=50
   ```
   Cause fréquente : migrations SQL qui échouent parce que la DB n'est pas prête. Le container `kubemanager` a un `depends_on` mais pas de healthcheck — relance `docker compose restart kubemanager`.

4. **Docker "out of disk"** : `docker system prune -af --volumes` (⚠️ efface *tous* tes volumes docker, pas seulement ceux de kubemanager).

## Prochaine étape

→ [Première contribution](02-first-contribution.md)
