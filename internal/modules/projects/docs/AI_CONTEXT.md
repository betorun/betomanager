# `projects` — AI Context

## Invariants
- Un projet = un namespace K8s unique `km-<project-id>`.
- Le owner est toujours membre avec rôle `owner` et status `ACTIVE`.
- Toute modification projet qui touche K8s passe par une activité Temporal (jamais appel direct depuis le handler HTTP).

## Règles
- La compensation est obligatoire pour toute activité avec effet externe.
- Les secrets projet sont isolés par namespace et jamais partagés.
