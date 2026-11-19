package alerts

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

// Nouvelle clé API d'un compte de service : une clé donne tous les droits du
// compte ; les propriétaires et éditeurs du projet en sont prévenus.

// KindAPIKeyCreated : une alerte par clé.
const KindAPIKeyCreated = "iam.api_key_created"

const pageServiceAccounts = "/service-accounts"

var apiKeyCopies = pair(
	alertCopy{"Nouvelle clé API pour « {{.Name}} »", "Nouvelle clé API",
		"Une clé API « {{.Key}} » vient d'être créée pour le compte de service « {{.Name}} » du projet « {{.Project}} », par {{.By}}. Elle donne tous les droits de ce compte jusqu'au {{.Expires}}. Si vous ne l'attendiez pas, révoquez-la.",
		"Voir les comptes de service"},
	alertCopy{"New API key for \"{{.Name}}\"", "New API key",
		"An API key \"{{.Key}}\" was just created for service account \"{{.Name}}\" in project \"{{.Project}}\", by {{.By}}. It grants all of this account's permissions until {{.Expires}}. If you did not expect it, revoke it.",
		"View service accounts"},
)

// plainValue : valeur saisie par un client, insérée dans un modèle : sans
// accolades (pas d'injection de modèle), courte.
func plainValue(s string) string {
	s = strings.NewReplacer("{", "", "}", "").Replace(strings.TrimSpace(s))
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80]) + "…"
	}
	if s == "" {
		return "?"
	}
	return s
}

// APIKeyCreated : clé `key` créée pour le compte `account` par `by` (adresse
// e-mail ou nom), valable jusqu'à `expires`.
func (n *Notifier) APIKeyCreated(ctx context.Context, project, key uuid.UUID, account, keyName, by string, expires time.Time) {
	if n == nil {
		return
	}
	copies := make(map[i18n.Locale]alertCopy, len(apiKeyCopies))
	for lang, c := range apiKeyCopies {
		date := expires.UTC().Format("02/01/2006")
		if lang == i18n.EN {
			date = expires.UTC().Format("Jan 2, 2006")
		}
		if strings.TrimSpace(keyName) == "" {
			keyName = "—"
		}
		c.Body = strings.NewReplacer("{{.Key}}", plainValue(keyName), "{{.By}}", plainValue(by), "{{.Expires}}", date).Replace(c.Body)
		copies[lang] = c
	}
	n.run(ctx, func(ctx context.Context) {
		n.deliver(ctx, project, key, KindAPIKeyCreated, DiskEvery, copies, account, pageServiceAccounts)
	})
}
