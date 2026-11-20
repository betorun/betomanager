# ADR 0009 — Alertes email minimales

Date : 2026-11-18 · Statut : implémenté

## Contexte

Un conteneur peut tomber (CrashLoopBackOff, OOMKilled, image introuvable…) ou un
build échouer, sans que personne ne regarde la console. V1 : prévenir les owners
par email, sans bruit et sans divulguer le message brut du cluster.

## Décisions

- Déclencheurs :
  - incident de conteneur, au **passage** dans un statut d'incident
    (`clusters.OnPodEvent`, serveur) : CRASHLOOP, OOM_KILLED, IMAGE_PULL_FAILED,
    CONFIG_ERROR, EVICTED, FAILED (pod) ;
  - build échoué (`builds/activities.Finish`, worker) : BUILD_FAILED, TIMEOUT,
    DISK_LIMIT, NO_BUILD_PLAN, GIT_ACCESS_LOST, INTERNAL. Pas SUPERSEDED ni
    TOO_MANY_BUILDS (rien à faire pour le client).
- Destinataires : propriétaires et éditeurs du projet, directs ou par un groupe,
  liaison en cours de validité, comptes actifs, 20 au plus (décision du
  2026-11-19 : ceux qui peuvent réparer ; V1 : propriétaires directs seulement).
  Désinscription par personne : plus tard.
- Fréquence : table `alert_deliveries (subject_id, kind)` ; réservation atomique
  par `INSERT … ON CONFLICT … WHERE sent_at < now() - période RETURNING`.
  6 h par conteneur, 1 h par projet pour les builds.
- Contenu : textes fixes FR/EN (FR par défaut), nom du projet et du conteneur,
  lien vers `/workloads/<id>` ou `/builds/<id>`. **Jamais** le message du pod ni
  un extrait de journal. Noms échappés dans le HTML.
- Désactivé si l'envoi d'emails ou l'URL de la console n'est pas configuré.

## Retour à la normale

Le watcher ne signale que les pannes. `workloads/service.Recoverer` (worker,
chaque minute) remet RUNNING (phase RECOVERED) un conteneur en incident `POD_*`
dont le déploiement est à jour et dont tous les pods tournent depuis 2 min sans
redémarrage. Écriture conditionnelle : un déploiement lancé entre-temps garde la
main.

## Code

- `internal/modules/alerts/` (Notifier, Store, textes)
- `internal/infrastructure/router/alerts_wiring.go`
- `migrations/000024_alert_deliveries.*`
- `internal/modules/workloads/service/recover.go`, `repository/incidents.go`

## Suite (2026-11-20)

Fait : langue préférée enregistrée sur le compte (`users.locale`), coupure
par catégorie et par personne avec lien de désabonnement en un clic
(`List-Unsubscribe`), webhooks Slack ou JSON signé par projet. Détails dans
[docs/modules/alerts.md](../modules/alerts.md).
