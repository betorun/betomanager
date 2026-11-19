# Module `realtime`

> Intégration Centrifugo v5 : tokens JWT HS256 pour WebSocket + Publisher HTTP API pour broadcast serveur → client.

## Endpoints

| Méthode | Chemin | Description | Auth |
|---------|--------|-------------|------|
| `GET` | `/realtime/token` | Token de connexion WS (TTL 1h) | Bearer user |
| `GET` | `/realtime/subscribe?channel=...` | Token de subscription scopé (TTL 10min) | Bearer + membership projet |

Déclarations : `internal/infrastructure/router/route.go:57-64`.

## TokenSigner (HS256)

Fichier : `internal/infrastructure/realtime/token.go`.

```go
type TokenSigner interface {
    GenerateConnectionToken(ctx, subject, exp, info map[string]any) (string, error)
    GenerateSubscriptionToken(ctx, subject, channel, exp) (string, error)
}
```

Implémentation HMAC-SHA256 maison (pas de lib externe) :

- Header : `{"alg":"HS256","typ":"JWT"}`.
- Payload : claims `{sub, exp, info?}` ou `{sub, channel, exp}`.
- Signature : `HMAC-SHA256(secret, base64url(header) + "." + base64url(payload))`.
- Secret : `config.Centrifugo.TokenHMACSecret` (env `KUBEMANAGER_CENTRIFUGO_TOKEN_HMAC_SECRET`).

## Controller

`internal/modules/realtime/hrealtime.go` :

```go
func (ctrl *RealtimeController) GetConnectionToken(c *gin.Context) (*TokenResponse, error) {
    userID := c.GetString("userID")
    // generate JWT avec sub=userID, exp=now+1h, info={email}
    return &TokenResponse{Token, WSURL, Subject: userID, ExpiresAt}, nil
}

func (ctrl *RealtimeController) GetSubscriptionToken(c *gin.Context, in *SubscribeInput) (*TokenResponse, error) {
    userID := c.GetString("userID")
    projectID := parseProjectFromChannel(in.Channel)      // project:<uuid>:...
    requireProjectMembership(authRepo, userID, projectID) // status ACTIVE ou APPROVED
    // generate JWT avec sub=userID, channel=in.Channel, exp=now+10m
    return &TokenResponse{Token, ...}, nil
}
```

## Publisher (serveur → client)

Fichier : `internal/infrastructure/realtime/publish.go`.

```go
type Publisher interface {
    Publish(ctx context.Context, channel string, data any) error
}
```

Implémentation HTTP :
- POST `{ApiURL}/publish` avec header `X-API-Key: {ApiKey}`.
- Body : `{"channel":"...","data":{...}}`.
- Timeout 5s.
- **Fail-open** : erreur loggée, jamais remontée.

### Helpers channels

```go
ProjectChannel(projectID)                // project:X:events
WorkloadChannel(projectID, workloadID)   // project:X:workload:Y
DeploymentChannel(projectID, workloadID) // project:X:workload:Y:deploy
```

## Flow end-to-end

```
┌──────────┐                 ┌────────────┐
│ Frontend │                 │ KubeManager│
└────┬─────┘                 └──────┬─────┘
     │  GET /realtime/token         │
     │─────────────────────────────▶│  signe JWT HS256
     │◀──── { token, ws_url }       │
     │                              │
     │  WS open {token}             │        ┌────────────┐
     │─────────────────────────────────────▶│ Centrifugo │
     │                              │        └────────────┘
     │  GET /realtime/subscribe?channel=project:X:workload:Y:deploy
     │─────────────────────────────▶│  check membership + signe JWT
     │◀──── { token }               │
     │  SUB {channel, token}        │
     │─────────────────────────────────────▶│
     │                              │                 │
     │                              │  activité Helm finit 
     │                              │  publisher.Publish(channel, event)
     │                              │──────── HTTP /publish ──────▶
     │                              │                 │ broadcast WS
     │◀────────────────────────────────────────────── │
```

## Config & env vars

Struct `config.Centrifugo` (`internal/core/configs/config.go:105`) :

| Champ | Env var | Description |
|-------|---------|-------------|
| `Enabled` | `KUBEMANAGER_CENTRIFUGO_ENABLED` | Master switch |
| `URL` | `KUBEMANAGER_CENTRIFUGO_URL` | URL HTTP publique (pour `ws_url`) |
| `ApiURL` | `KUBEMANAGER_CENTRIFUGO_API_URL` | Endpoint HTTP `/publish` |
| `ApiKey` | `KUBEMANAGER_CENTRIFUGO_API_KEY` | API key Centrifugo |
| `TokenHMACSecret` | `KUBEMANAGER_CENTRIFUGO_TOKEN_HMAC_SECRET` | Secret signer JWT |
| `AllowedOrigins` | … | CORS |

Config Centrifugo (docker-compose) : var d'env `CENTRIFUGO_TOKEN_HMAC_SECRET_KEY` (attention au suffixe `_KEY`).

## Usages côté serveur

- `workloads/activities/db.go:23` — Publisher injecté (optional, noop si nil).
- `clusters/service.go` — publish sur `DeploymentChannel` + `ProjectChannel` dans OnPodEvent.
- `projects/workflows/create.go` — publish progression création projet.
