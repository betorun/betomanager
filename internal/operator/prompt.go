package operator

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/operator/store"
)

// Prompt : le prompt système d'un dossier. Stable d'un tour à l'autre (le
// début est mis en cache par l'API) : seuls le dossier et la date varient.
func Prompt(inv store.Investigation, now time.Time) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	fmt.Fprintf(&b, "\n<dossier>\nsujet : %s %s\nmotif : %s\nmembre de l'équipe : %s\ndate : %s\n</dossier>\n",
		inv.SubjectKind, inv.SubjectRef, orNone(inv.Reason), inv.ActorEmail, now.UTC().Format("2006-01-02 15:04 UTC"))
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(aucun : ne lire aucune donnée de client)"
	}
	return s
}

// systemPrompt : consignes stables de l'Opérateur.
//
//go:embed prompt.md
var systemPrompt string
