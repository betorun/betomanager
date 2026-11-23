package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
	diskusagedomain "github.com/thekrauss/kubemanager/internal/modules/diskusage/domain"
)

// humanBytes : unités binaires, comme les tailles demandées (5Gi).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// usedPercent : part utilisée de la taille payée ; « — » si inconnue.
func usedPercent(used, capacity int64) string {
	if capacity <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f %%", float64(used)*100/float64(capacity))
}

// usageTone : couleur selon les seuils d'alerte de la plateforme.
func usageTone(level int) ui.Tone {
	switch {
	case level >= diskusagedomain.LevelFull:
		return ui.ToneError
	case level >= diskusagedomain.LevelWarning:
		return ui.ToneProgress
	}
	return ui.ToneOK
}

// newProjectsDiskUsage : « projects disk-usage » (projet courant ou -p).
func newProjectsDiskUsage() *cobra.Command {
	return &cobra.Command{
		Use: "disk-usage", Aliases: []string{"du"}, Short: i18n.T("proj.du.short"), Long: i18n.T("proj.du.long"),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			res, err := cl.DiskUsage(cmd.Context(), project)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, res); done {
				return err
			}
			p := printer(cmd)
			if len(res.Disks) == 0 {
				p.Hint(i18n.T("proj.du.empty"))
				return nil
			}
			t := p.Table(i18n.T("proj.du.col.kind"), i18n.T("wl.col.name"), i18n.T("proj.du.col.used"),
				i18n.T("db.col.size"), "%", i18n.T("proj.du.col.measured"))
			for _, d := range res.Disks {
				kind := i18n.T("proj.du.kind.workload")
				if d.Kind == diskusagedomain.KindDatabase {
					kind = i18n.T("proj.du.kind.database")
				}
				t.Row(kind, p.Bold(d.TargetName), humanBytes(d.UsedBytes), humanBytes(d.CapacityBytes),
					p.Status(usageTone(d.Level), usedPercent(d.UsedBytes, d.CapacityBytes)),
					d.MeasuredAt.Local().Format(time.DateTime[:16]))
			}
			return t.Flush()
		},
	}
}
