# Module `alerts`

> Alertes email minimales aux propriétaires et éditeurs d'un projet : conteneur en incident (crash en boucle, mémoire, image, configuration, éviction, arrêt, tâche planifiée échouée) et build échoué. Textes fixes FR/EN, fréquence limitée, jamais le message brut du cluster ni un extrait de journal. Décision : [ADR 0009](../adr/0009-alertes-email.md).

## Modèle

Le module n'expose aucune route HTTP : c'est un `Notifier` appelé par d'autres modules, et un `Store` en base.

- **Table** `alert_deliveries (subject_id, kind, sent_at)`, clé primaire `(subject_id, kind)` (migration 000024). Une ligne par sujet et par type d'alerte ; elle sert uniquement à limiter la fréquence, y compris entre plusieurs instances de l'API et après un redémarrage.
- **Types** (`kind`) :
  - `workload.incident` : sujet = le conteneur, au plus une alerte toutes les **6 h** (`WorkloadEvery`) ;
  - `build.failed` : sujet = **le projet** (pas le build), au plus une alerte par **heure** (`BuildEvery`).
- **Réservation atomique** (`Store.Claim`) : `INSERT … ON CONFLICT (subject_id, kind) DO UPDATE SET sent_at = now() WHERE sent_at < now() - période RETURNING`. Une ligne renvoyée = envoi réservé ; sinon l'alerte est abandonnée.
- **Destinataires** (`Store.Recipients`) : personnes ayant le rôle `roles/owner` ou `roles/editor` sur `projects/<id>` (table `iam_bindings`), directement ou par un groupe (`iam_group_members`), liaison en cours de validité, comptes actifs et non supprimés, dédoublonnées et triées par email, **20 au plus** (`maxRecipients`).

## Déclencheurs

| Appelant | Méthode | Statuts signalés |
|---|---|---|
| `clusters` (serveur, événement de pod, au passage dans le statut) | `WorkloadIncident` | `CRASHLOOP`, `OOM_KILLED`, `IMAGE_PULL_FAILED`, `CONFIG_ERROR`, `EVICTED`, `FAILED` |
| `workloads/service.Recoverer` (worker, chaque minute) | `WorkloadIncident` | `RUN_FAILED` (dernière exécution d'un cron en échec) |
| `builds/activities.Finish` (worker) | `BuildFailed` | `BUILD_FAILED`, `TIMEOUT`, `DISK_LIMIT`, `NO_BUILD_PLAN`, `GIT_ACCESS_LOST`, `INTERNAL` |

Tout autre statut est ignoré sans bruit, notamment `SUPERSEDED` et `TOO_MANY_BUILDS` côté build (rien à faire pour le client). Le `Recoverer` remet aussi un conteneur à `RUNNING` (phase `RECOVERED`) après 2 min de stabilité, mais ce retour à la normale n'envoie **aucun** email.

## Déroulé d'un envoi

1. L'appelant invoque le `Notifier` ; un `Notifier` nil (alertes désactivées) ne fait rien.
2. L'envoi part en arrière-plan (goroutine), avec un contexte détaché de la requête et un délai de **30 s** (`sendTimeout`). En test, `Sync: true` l'exécute dans l'appel.
3. `Claim` réserve l'envoi ; puis lecture des destinataires et du nom du projet.
4. Pour chaque destinataire : rendu texte + HTML (gabarit à la couleur de la console), envoi SMTP. Un échec est journalisé (`alert email failed`) et n'interrompt pas les autres envois.

Contenu : titre, salutation (nom complet, sinon partie locale de l'email), corps fixe reprenant le **nom du projet** et le **nom du conteneur**, bouton vers `<frontend.url>/workloads/<id>` ou `<frontend.url>/builds/<id>`, mention « vous recevez cet email parce que vous êtes propriétaire ou éditeur ». Les noms sont insérés par `text/template` puis échappés par `html/template` dans la version HTML.

Langue : celle du contexte (`i18n.From(ctx)`), anglais si `EN`, français sinon.

## Configuration

Le `Notifier` est construit par `alertNotifier()` ([alerts_wiring.go](../../internal/infrastructure/router/alerts_wiring.go)) à trois endroits : contrôleur des clusters (serveur), activités de build et `Recoverer` (worker). Il vaut nil, avec un avertissement au démarrage, si :

- `mail.enabled` est faux, ou la configuration SMTP est invalide (`mail.smtp_host`, `mail.smtp_port`, `mail.smtp_user`, `mail.smtp_password`, `mail.from_email`, `mail.from_name`) ;
- `frontend.url` est vide (liens impossibles).

Aucun réglage propre aux alertes (périodes, nombre de destinataires) : ce sont des constantes du code.

## Pièges

- **Pas de désinscription** ni de préférence par utilisateur : tout propriétaire ou éditeur actif du projet reçoit les alertes (jusqu'à 20).
- **Destinataires** : propriétaires et éditeurs du projet, directs ou membres d'un groupe lié au projet ; une liaison expirée (condition) est ignorée ; dédoublonnés par email, 20 au plus. Les rôles hérités de l'organisation ne sont pas lus.
- **Build : limitation par projet**. Deux conteneurs qui échouent leur build dans l'heure ne produisent qu'une alerte, qui pointe vers le premier build.
- **Réservation avant envoi** : si le SMTP échoue après `Claim`, la période est consommée ; l'alerte n'est pas renvoyée avant 6 h (ou 1 h).
- **Langue** : les déclencheurs viennent d'événements du cluster ou du worker, sans langue de requête ; en pratique l'email est le plus souvent en français. La langue enregistrée sur le compte n'est pas utilisée.
- **Jamais de message brut** : ne pas ajouter au texte le message du pod, la raison Kubernetes ou un extrait de journal (chemins, variables, secrets possibles).
- Une erreur de lecture (`Claim`, destinataires, nom du projet) est seulement journalisée : l'alerte est perdue, l'appelant n'en sait rien.

## Fichiers clés

- [alerts.go](../../internal/modules/alerts/alerts.go) — `Notifier`, types, périodes, textes FR/EN, gabarits.
- [store.go](../../internal/modules/alerts/store.go) — réservation atomique, destinataires, nom du projet.
- [alerts_wiring.go](../../internal/infrastructure/router/alerts_wiring.go) — construction et conditions d'activation.
- [clusters/service.go](../../internal/modules/clusters/service.go), [workloads/service/recover.go](../../internal/modules/workloads/service/recover.go), [builds/activities/activities.go](../../internal/modules/builds/activities/activities.go) — appelants.
- [000024_alert_deliveries.up.sql](../../migrations/000024_alert_deliveries.up.sql) — table de limitation.
