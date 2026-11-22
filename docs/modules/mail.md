# Module mail

Envoi des e-mails de betorun : vérification d'adresse, mot de passe, alertes,
facturation, membres… Les modules ne voient qu'une interface,
`mail.Sender` (`internal/infrastructure/mail`) : ils rendent eux-mêmes le
message (texte et HTML, dans la langue du destinataire) et l'envoient. Le
module décide **comment** il part.

## Parcours d'un e-mail

```
module ──Send──▶ QueueSender ──(adresse mise de côté ? fin)──▶ file asynq (Redis, « default »)
                                                                      │
                                       worker : Deliverer.HandleSendMail ◀┘
                                                     │ SMTP
                     envoyé ◀───┬──── Retry : 4xx, réseau, 530/535, refus côté expéditeur → reprise espacée
                                ├──── Rejected : 550/551/553 sur l'adresse → mise de côté, fin
                                └──── Failed : message refusé (554, 552, en-tête ou pièce invalide) → archivé
```

- **API** : `QueueSender` met le message en file (3 s au plus) ; une erreur ne
  revient à l'appelant que si Redis ne répond pas.
- **Worker** (`APP_MODE=worker`) : `startMailWorker` consomme la file
  (`mail.queue_concurrency`, 10 par défaut). Sans worker, les mails attendent
  dans Redis : rien n'est perdu.
- **Reprises** : 8, espacées (30 s, 2 min, 10 min, 30 min, 1 h, puis 3 h),
  soit environ une journée : un SMTP en panne ou mal configuré a le temps
  d'être réparé. Une tâche épuisée ou refusée reste **archivée** dans Redis
  (asynq `archived`), jamais perdue sans trace.
- **Sans Redis** : envoi SMTP direct, comme avant.
- Les journaux ne portent jamais l'adresse complète (`a***@ex.sn`), ni le contenu.

Redis ne sert qu'aux tâches de fond (e-mails) : les opérations sur les
ressources des clients restent des workflows Temporal.

## Message

`mail.Message` : `To`, `Subject`, `Text`, `HTML`, `Unsubscribe` (RFC 8058),
`ReplyTo`, `Attachments` (5 Mo au total, voyagent en base64 dans la file).
Avec des pièces jointes : `multipart/mixed` (corps `multipart/alternative`,
puis les pièces). Noms de fichier et en-têtes contrôlés (pas d'injection).

## Adresses mises de côté (migration 000056)

Table `mail_suppressions` (adresse en minuscules, raison, origine, début du
message du serveur). Plus rien n'est envoyé à une adresse de la table ; la
vérification a lieu à la mise en file et à l'envoi. Origines :

- **SMTP** : refus définitif de l'adresse (550, 551, 553), sauf quand la
  cause est l'expéditeur (réputation, SPF/DKIM/DMARC, volume, relais) ;
- **SendGrid** : webhook d'événements signé (`POST /mail/webhooks/sendgrid`,
  public, signature ECDSA sur horodatage + corps, horodatage à 5 min près).
  `bounce` (sauf `blocked`), `dropped` sur une adresse invalide ou déjà en
  rebond, `spamreport` (plainte).

Configuration : `KUBEMANAGER_MAIL_SENDGRID_WEBHOOK_PUBLIC_KEY` (clé de
vérification du tableau de bord SendGrid, Settings → Mail Settings → Event
Webhook, « Signed Event Webhook ») ; vide : webhook désactivé (404). Dans
SendGrid, cocher au moins Bounced, Dropped et Spam Reports, URL
`https://<api>/kmanager/v1/mail/webhooks/sendgrid`.

**Équipe** (`platform.mailSuppressions.manage`, rôle `roles/platform.admin`) :

| Méthode | Chemin | Description |
|---|---|---|
| GET | `/admin/mail/suppressions?search=` | Adresses mises de côté, les plus récentes d'abord (200 au plus) |
| DELETE | `/admin/mail/suppressions?email=` | Relâcher une adresse |

## Plus tard (option 1 · VPS OVH)

- Notifications et code à 6 chiffres des actions sensibles des VPS : même
  `mail.Sender`, rien à ajouter ici.
- **Interception des e-mails OVH** (identifiants du mode secours…) : un
  lecteur IMAP de la boîte technique, dans ce module, en tâche de fond.

## Tests

- `internal/infrastructure/mail` : multipart, Reply-To, pièce jointe, injection d'en-tête, taille.
- `domain` : tri des erreurs SMTP, normalisation et masquage des adresses.
- `service` : mise en file, adresses mises de côté, chaque issue du worker, webhook SendGrid (signature, rejeu, types d'événements).
- `repository/repository_e2e_test.go` (`IAM_E2E=1`) : table réelle.
- `queue_e2e_test.go` (`MAIL_E2E_REDIS=…`) : file asynq réelle, de la mise en file à la livraison.
