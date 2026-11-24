package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Suppression programmée d'un projet : les propriétaires et éditeurs sont
// prévenus à la demande (avec la date d'effacement et le lien pour
// restaurer), puis à la restauration. Ces e-mails ne se coupent pas.

const (
	KindProjectDeletion = "project.deletion"
	KindProjectRestored = "project.restored"
	// projectDeletionEvery : une demande répétée (idempotente) n'envoie pas
	// deux e-mails ; une nouvelle demande après restauration, si.
	projectDeletionEvery = time.Minute
	pageProject          = "/projects"
)

var projectDeletionCopies = pair(
	alertCopy{"Le projet « {{.Project}} » sera supprimé le {{.When}}", "Suppression du projet programmée",
		"La suppression du projet « {{.Project}} » a été demandée. Ses conteneurs sont arrêtés et ses bases mises en veille ; rien n'est encore effacé. Le projet et toutes ses données seront supprimés définitivement le {{.When}}. D'ici là, vous pouvez le restaurer depuis la console : tout redémarre comme avant.",
		"Restaurer le projet"},
	alertCopy{"The project \"{{.Project}}\" will be deleted on {{.When}}", "Project deletion scheduled",
		"Deletion of the project \"{{.Project}}\" was requested. Its containers are stopped and its databases paused; nothing is erased yet. The project and all its data will be deleted for good on {{.When}}. Until then, you can restore it from the console: everything starts again as before.",
		"Restore the project"})

var projectRestoredCopies = pair(
	alertCopy{"Le projet « {{.Project}} » a été restauré", "Projet restauré",
		"La suppression du projet « {{.Project}} » a été annulée. Ses conteneurs et ses bases redémarrent ; rien n'a été effacé.",
		"Voir le projet"},
	alertCopy{"The project \"{{.Project}}\" was restored", "Project restored",
		"Deletion of the project \"{{.Project}}\" was cancelled. Its containers and databases are starting again; nothing was erased.",
		"View the project"})

// whenText : « 24/11/2026 à 12:15 UTC » / « 2026-11-24 12:15 UTC ».
func whenText(lang i18n.Locale, at time.Time) string {
	at = at.UTC()
	if lang == i18n.EN {
		return at.Format("2006-01-02 15:04") + " UTC"
	}
	return at.Format("02/01/2006") + " à " + at.Format("15:04") + " UTC"
}

// ProjectDeletionScheduled : suppression demandée, effacement à at.
func (n *Notifier) ProjectDeletionScheduled(ctx context.Context, project uuid.UUID, name string, at time.Time, _ string) {
	if n == nil {
		return
	}
	copies := make(map[i18n.Locale]alertCopy, len(projectDeletionCopies))
	for lang, c := range projectDeletionCopies {
		w := whenText(lang, at)
		c.Subject = strings.ReplaceAll(c.Subject, "{{.When}}", w)
		c.Body = strings.ReplaceAll(c.Body, "{{.When}}", w)
		copies[lang] = c
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, project, KindProjectDeletion, projectDeletionEvery, copies, name, pageProject)
	})
}

// ProjectRestored : suppression annulée.
func (n *Notifier) ProjectRestored(ctx context.Context, project uuid.UUID, name string, _ string) {
	if n == nil {
		return
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, project, KindProjectRestored, projectDeletionEvery, projectRestoredCopies, name, pageProject)
	})
}
