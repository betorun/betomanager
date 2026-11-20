package alerts

import (
	"bytes"
	"context"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	buildsDomain "github.com/thekrauss/kubemanager/internal/modules/builds/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Types d'alerte (colonne kind de alert_deliveries).
const (
	KindWorkloadIncident = "workload.incident"
	KindBuildFailed      = "build.failed"
)

// Fréquence maximale des envois, par sujet.
const (
	// WorkloadEvery : un conteneur qui tombe et revient sans cesse n'envoie
	// qu'une alerte par période.
	WorkloadEvery = 6 * time.Hour
	// BuildEvery : par projet ; une suite de pushes cassés n'envoie qu'une
	// alerte par période.
	BuildEvery  = time.Hour
	sendTimeout = 30 * time.Second
)

// Pages de la console.
const (
	pageWorkload = "/workloads/"
	pageBuild    = "/builds/"
)

// Notifier : envoie les alertes. Les textes sont fixes et ne reprennent jamais
// le message brut du cluster ou du build (il peut contenir des chemins, des
// variables ou des extraits de journaux) : seulement le nom du projet, du
// conteneur et un lien vers la console.
type Notifier struct {
	Store      Store
	Mail       mail.Sender
	ConsoleURL string
	Logger     *zap.SugaredLogger
	// Sync : envoi dans l'appel (tests) ; sinon en arrière-plan.
	Sync bool
	// UnsubscribeKey : signe les liens de désabonnement (vide : pas de lien).
	UnsubscribeKey []byte
	// APIURL : URL publique de l'API (…/kmanager/v1), pour le désabonnement
	// en un clic des messageries ; vide : lien de la console seulement.
	APIURL string
	// Webhooks : envoi des alertes de projet vers Slack ou un JSON signé (nil : aucun).
	Webhooks WebhookSender
}

// WebhookSender : WebhookService.
type WebhookSender interface {
	Send(ctx context.Context, project uuid.UUID, kind string, subject uuid.UUID, projectName, link string, texts map[i18n.Locale][2]string)
}

// WorkloadIncident : conteneur passé dans un statut d'incident (CRASHLOOP,
// OOM_KILLED…). Les autres statuts sont ignorés.
func (n *Notifier) WorkloadIncident(ctx context.Context, project, workload uuid.UUID, name, status string) {
	if n == nil {
		return
	}
	copies, ok := workloadCopies[status]
	if !ok {
		return
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, workload, KindWorkloadIncident, WorkloadEvery, copies, name, pageWorkload+workload.String())
	})
}

// BuildFailed : build terminé en échec. Les échecs sans action possible pour
// le client (remplacé par un build plus récent, refusé à la soumission) sont
// ignorés.
func (n *Notifier) BuildFailed(ctx context.Context, project, build uuid.UUID, failure string) {
	if n == nil {
		return
	}
	copies, ok := buildCopies[failure]
	if !ok {
		return
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, project, KindBuildFailed, BuildEvery, copies, "", pageBuild+build.String())
	})
}

func (n *Notifier) run(ctx context.Context, f func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	if n.Sync {
		defer cancel()
		f(ctx)
		return
	}
	go func() {
		defer cancel()
		f(ctx)
	}()
}

func (n *Notifier) deliver(ctx context.Context, project, subject uuid.UUID, kind string, every time.Duration,
	copies map[i18n.Locale]alertCopy, name, page string) {
	if n.Mail == nil || n.Store == nil {
		return
	}
	claimed, err := n.Store.Claim(ctx, subject, kind, every)
	if err != nil || !claimed {
		if err != nil {
			n.log().Warnw("alert throttle check failed", "kind", kind, "subject", subject, zap.Error(err))
		}
		return
	}
	recipients, err := n.Store.Recipients(ctx, project)
	if err != nil {
		n.log().Warnw("alert recipients lookup failed", "kind", kind, "project", project, zap.Error(err))
		return
	}
	projectName, err := n.Store.ProjectName(ctx, project)
	if err != nil {
		n.log().Warnw("alert project lookup failed", "project", project, zap.Error(err))
		return
	}
	link := strings.TrimRight(n.ConsoleURL, "/") + page
	if n.Webhooks != nil {
		n.Webhooks.Send(ctx, project, kind, subject, projectName, link, webhookTexts(copies, projectName, name))
	}
	category := categoryOf(kind)
	if category != "" {
		recipients = n.withoutMuted(ctx, recipients, category)
	}
	for _, r := range recipients {
		unsub, oneClick := n.unsubscribeLinks(r.ID, category)
		msg, err := renderFull(ctx, copies, r, projectName, name, link, outro, unsub)
		msg.Unsubscribe = oneClick
		if err == nil {
			err = n.Mail.Send(ctx, msg)
		}
		if err != nil {
			n.log().Warnw("alert email failed", "kind", kind, "subject", subject, zap.Error(err))
		}
	}
}

func (n *Notifier) log() *zap.SugaredLogger {
	if n.Logger == nil {
		return zap.NewNop().Sugar()
	}
	return n.Logger
}

// alertCopy : textes d'une alerte dans une langue. {{.Name}} : conteneur ;
// {{.Project}} : projet.
type alertCopy struct {
	Subject, Title, Body, Button string
}

func pair(fr, en alertCopy) map[i18n.Locale]alertCopy {
	return map[i18n.Locale]alertCopy{i18n.FR: fr, i18n.EN: en}
}

var workloadCopies = map[string]map[i18n.Locale]alertCopy{
	utils.WorkloadCrashLoop: pair(
		alertCopy{"« {{.Name}} » redémarre en boucle", "Votre conteneur redémarre en boucle",
			"Le conteneur « {{.Name}} » du projet « {{.Project}} » s'arrête peu après chaque démarrage. Ses journaux, dans la console, indiquent en général l'erreur.",
			"Voir le conteneur"},
		alertCopy{"\"{{.Name}}\" keeps restarting", "Your container keeps restarting",
			"The container \"{{.Name}}\" of project \"{{.Project}}\" stops shortly after each start. Its logs, in the console, usually show the error.",
			"View the container"}),
	utils.WorkloadOOMKilled: pair(
		alertCopy{"« {{.Name}} » a manqué de mémoire", "Votre conteneur a manqué de mémoire",
			"Le conteneur « {{.Name}} » du projet « {{.Project}} » a été arrêté parce qu'il dépassait sa limite de mémoire. Augmentez cette limite ou réduisez sa consommation.",
			"Voir le conteneur"},
		alertCopy{"\"{{.Name}}\" ran out of memory", "Your container ran out of memory",
			"The container \"{{.Name}}\" of project \"{{.Project}}\" was stopped because it exceeded its memory limit. Raise the limit or reduce its usage.",
			"View the container"}),
	utils.WorkloadImagePullFailed: pair(
		alertCopy{"L'image de « {{.Name}} » est inaccessible", "Image inaccessible",
			"L'image du conteneur « {{.Name}} » du projet « {{.Project}} » ne peut pas être téléchargée. Vérifiez son nom, son étiquette et, pour un registre privé, l'accès enregistré dans le projet.",
			"Voir le conteneur"},
		alertCopy{"The image of \"{{.Name}}\" cannot be pulled", "Image unavailable",
			"The image of container \"{{.Name}}\" in project \"{{.Project}}\" cannot be downloaded. Check its name, its tag and, for a private registry, the access saved in the project.",
			"View the container"}),
	utils.WorkloadConfigError: pair(
		alertCopy{"« {{.Name}} » ne peut pas démarrer", "Configuration invalide",
			"Le conteneur « {{.Name}} » du projet « {{.Project}} » ne peut pas démarrer : sa configuration fait référence à un élément absent. Vérifiez ses variables.",
			"Voir le conteneur"},
		alertCopy{"\"{{.Name}}\" cannot start", "Invalid configuration",
			"The container \"{{.Name}}\" of project \"{{.Project}}\" cannot start: its configuration refers to something missing. Check its variables.",
			"View the container"}),
	utils.WorkloadEvicted: pair(
		alertCopy{"« {{.Name}} » a été interrompu", "Conteneur interrompu",
			"Le conteneur « {{.Name}} » du projet « {{.Project}} » a été arrêté faute de ressources sur le serveur. Il redémarre automatiquement ; s'il consomme beaucoup de mémoire ou de disque, réduisez sa consommation.",
			"Voir le conteneur"},
		alertCopy{"\"{{.Name}}\" was interrupted", "Container interrupted",
			"The container \"{{.Name}}\" of project \"{{.Project}}\" was stopped because the server ran short of resources. It restarts automatically; if it uses a lot of memory or disk, reduce its usage.",
			"View the container"}),
	utils.WorkloadFailed: pair(
		alertCopy{"« {{.Name}} » s'est arrêté", "Votre conteneur s'est arrêté",
			"Le conteneur « {{.Name}} » du projet « {{.Project}} » s'est arrêté en erreur. Ses journaux, dans la console, indiquent en général la cause.",
			"Voir le conteneur"},
		alertCopy{"\"{{.Name}}\" stopped", "Your container stopped",
			"The container \"{{.Name}}\" of project \"{{.Project}}\" stopped with an error. Its logs, in the console, usually show the cause.",
			"View the container"}),
	utils.WorkloadRunFailed: pair(
		alertCopy{"La tâche « {{.Name}} » a échoué", "Votre tâche planifiée a échoué",
			"La dernière exécution de la tâche « {{.Name}} » du projet « {{.Project}} » n'a pas réussi (erreur, durée maximale dépassée ou manque de mémoire). Ses journaux, dans la console, montrent cette exécution ; « Exécuter maintenant » permet de réessayer.",
			"Voir la tâche"},
		alertCopy{"The task \"{{.Name}}\" failed", "Your scheduled task failed",
			"The last run of the task \"{{.Name}}\" of project \"{{.Project}}\" did not succeed (error, maximum duration exceeded or out of memory). Its logs, in the console, show that run; \"Run now\" tries again.",
			"View the task"}),
}

// Échecs de build signalés ; les autres (SUPERSEDED, TOO_MANY_BUILDS) ne le
// sont pas.
var buildCopies = map[string]map[i18n.Locale]alertCopy{
	buildsDomain.FailureBuild: pair(
		alertCopy{"Build échoué dans « {{.Project}} »", "La construction de l'image a échoué",
			"Un build du projet « {{.Project}} » a échoué : erreur dans le Dockerfile ou dans le code. Le journal du build indique l'étape en cause. La version en ligne n'a pas changé.",
			"Voir le build"},
		alertCopy{"Build failed in \"{{.Project}}\"", "The image build failed",
			"A build of project \"{{.Project}}\" failed: an error in the Dockerfile or in the code. The build log shows the failing step. The live version did not change.",
			"View the build"}),
	buildsDomain.FailureTimeout: pair(
		alertCopy{"Build trop long dans « {{.Project}} »", "Le build a dépassé la durée maximale",
			"Un build du projet « {{.Project}} » a été arrêté parce qu'il durait trop longtemps. La version en ligne n'a pas changé.",
			"Voir le build"},
		alertCopy{"Build too long in \"{{.Project}}\"", "The build exceeded the time limit",
			"A build of project \"{{.Project}}\" was stopped because it took too long. The live version did not change.",
			"View the build"}),
	buildsDomain.FailureDisk: pair(
		alertCopy{"Build trop volumineux dans « {{.Project}} »", "Le build a dépassé l'espace disque",
			"Un build du projet « {{.Project}} » a été arrêté parce qu'il utilisait trop d'espace disque. La version en ligne n'a pas changé.",
			"Voir le build"},
		alertCopy{"Build too large in \"{{.Project}}\"", "The build exceeded its disk space",
			"A build of project \"{{.Project}}\" was stopped because it used too much disk space. The live version did not change.",
			"View the build"}),
	buildsDomain.FailureNoPlan: pair(
		alertCopy{"Build impossible dans « {{.Project}} »", "Aucune façon de construire ce code",
			"Un build du projet « {{.Project}} » n'a pas pu démarrer : aucun Dockerfile ni type de projet reconnu. La version en ligne n'a pas changé.",
			"Voir le build"},
		alertCopy{"Build impossible in \"{{.Project}}\"", "No way to build this code",
			"A build of project \"{{.Project}}\" could not start: no Dockerfile or known project type. The live version did not change.",
			"View the build"}),
	buildsDomain.FailureGitAccess: pair(
		alertCopy{"Accès au dépôt perdu dans « {{.Project}} »", "betorun n'a plus accès au dépôt",
			"Un build du projet « {{.Project}} » n'a pas pu lire le dépôt GitHub : l'accès accordé à betorun a été retiré. Reconnectez le dépôt depuis la console.",
			"Voir le build"},
		alertCopy{"Repository access lost in \"{{.Project}}\"", "betorun lost access to the repository",
			"A build of project \"{{.Project}}\" could not read the GitHub repository: the access granted to betorun was removed. Reconnect the repository from the console.",
			"View the build"}),
	buildsDomain.FailureInternal: pair(
		alertCopy{"Build interrompu dans « {{.Project}} »", "Le build a été interrompu",
			"Un build du projet « {{.Project}} » a été interrompu par une erreur de notre côté. Relancez-le ; si cela se reproduit, contactez-nous. La version en ligne n'a pas changé.",
			"Voir le build"},
		alertCopy{"Build interrupted in \"{{.Project}}\"", "The build was interrupted",
			"A build of project \"{{.Project}}\" was interrupted by an error on our side. Run it again; if it happens again, contact us. The live version did not change.",
			"View the build"}),
}

var textLayout = texttemplate.Must(texttemplate.New("text").Parse(`{{.Title}}

{{.Greeting}}

{{.Body}}

{{.Button}} : {{.Link}}

{{.Outro}}
{{- if .Unsubscribe}}

{{.UnsubscribeText}} : {{.Unsubscribe}}
{{- end}}

— betorun
`))

var htmlLayout = htmltemplate.Must(htmltemplate.New("html").Parse(`<!doctype html>
<html lang="{{.Lang}}"><body style="margin:0;padding:24px;background:#f6f4f2;font-family:'IBM Plex Sans',Arial,sans-serif;color:#1d1a19">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">
<table role="presentation" width="560" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:8px;padding:32px">
<tr><td style="font-size:20px;font-weight:600;color:#7A1F3D;padding-bottom:16px">betorun</td></tr>
<tr><td style="font-size:18px;font-weight:600;padding-bottom:12px">{{.Title}}</td></tr>
<tr><td style="font-size:15px;line-height:1.5;padding-bottom:8px">{{.Greeting}}</td></tr>
<tr><td style="font-size:15px;line-height:1.5;padding-bottom:24px">{{.Body}}</td></tr>
<tr><td style="padding-bottom:24px"><a href="{{.Link}}" style="display:inline-block;background:#7A1F3D;color:#ffffff;text-decoration:none;padding:12px 20px;border-radius:6px;font-weight:600">{{.Button}}</a></td></tr>
<tr><td style="font-size:13px;line-height:1.5;color:#6b6461">{{.Outro}}</td></tr>
{{- if .Unsubscribe}}
<tr><td style="font-size:13px;line-height:1.5;color:#6b6461;padding-top:12px"><a href="{{.Unsubscribe}}" style="color:#6b6461">{{.UnsubscribeText}}</a></td></tr>
{{- end}}
</table></td></tr></table></body></html>`))

var (
	greeting        = map[i18n.Locale]string{i18n.FR: "Bonjour %s,", i18n.EN: "Hello %s,"}
	unsubscribeText = map[i18n.Locale]string{i18n.FR: "Ne plus recevoir ce type d'alertes", i18n.EN: "Stop receiving this kind of alert"}
	outro           = map[i18n.Locale]string{
		i18n.FR: "Vous recevez cet email parce que vous êtes propriétaire ou éditeur de ce projet. Au plus une alerte de ce type par période de quelques heures.",
		i18n.EN: "You receive this email because you are an owner or editor of this project. At most one alert of this kind every few hours.",
	}
)

// render : email d'une alerte, dans la langue de ctx (français par défaut).
func render(ctx context.Context, copies map[i18n.Locale]alertCopy, to Recipient, project, name, link string) (mail.Message, error) {
	return renderWith(ctx, copies, to, project, name, link, outro)
}

// renderWith : render avec une phrase de fin choisie (ex. alertes d'organisation).
func renderWith(ctx context.Context, copies map[i18n.Locale]alertCopy, to Recipient, project, name, link string,
	outro map[i18n.Locale]string) (mail.Message, error) {
	return renderFull(ctx, copies, to, project, name, link, outro, "")
}

// renderFull : langue du destinataire s'il l'a choisie, sinon celle de ctx ;
// unsub : lien de désabonnement (vide : aucun).
func renderFull(ctx context.Context, copies map[i18n.Locale]alertCopy, to Recipient, project, name, link string,
	outro map[i18n.Locale]string, unsub string) (mail.Message, error) {
	lang := i18n.From(ctx)
	switch to.Locale {
	case "en":
		lang = i18n.EN
	case "fr":
		lang = i18n.FR
	}
	if lang != i18n.EN {
		lang = i18n.FR
	}
	c := copies[lang]
	vars := map[string]string{"Name": name, "Project": project}
	subject, err := fill(c.Subject, vars)
	if err != nil {
		return mail.Message{}, err
	}
	body, err := fill(c.Body, vars)
	if err != nil {
		return mail.Message{}, err
	}
	who := strings.TrimSpace(to.FullName)
	if who == "" {
		who, _, _ = strings.Cut(to.Email, "@")
	}
	data := map[string]string{"Lang": string(lang), "Title": c.Title, "Greeting": fmt.Sprintf(greeting[lang], who),
		"Body": body, "Button": c.Button, "Link": link, "Outro": outro[lang],
		"Unsubscribe": unsub, "UnsubscribeText": unsubscribeText[lang]}
	var text, html bytes.Buffer
	if err := textLayout.Execute(&text, data); err != nil {
		return mail.Message{}, err
	}
	if err := htmlLayout.Execute(&html, data); err != nil {
		return mail.Message{}, err
	}
	return mail.Message{To: to.Email, Subject: subject, Text: text.String(), HTML: html.String()}, nil
}

// fill : gabarit texte ; le HTML est échappé ensuite par htmlLayout.
func fill(tpl string, vars map[string]string) (string, error) {
	t, err := texttemplate.New("copy").Parse(tpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
