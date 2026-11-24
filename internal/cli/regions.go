package cli

import (
	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
)

// newRegions : « regions » : où créer un projet (betorun projects create
// --region). Tout ce que contient un projet (conteneurs, bases, services)
// est hébergé dans sa région.
func newRegions() *cobra.Command {
	return &cobra.Command{
		Use: "regions", Short: i18n.T("regions.short"), Long: i18n.T("regions.long"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			list, err := client(s).Regions(cmd.Context())
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list) == 0 {
				p.Hint(i18n.T("regions.empty"))
				return nil
			}
			t := p.Table(i18n.T("regions.col.code"), i18n.T("col.status"), "")
			for _, r := range list {
				status := p.Status(ui.ToneOK, i18n.T("regions.available"))
				if !r.Available {
					status = p.Status(ui.ToneError, i18n.T("regions.full"))
				}
				mark := ""
				if r.Default {
					mark = p.Dim(i18n.T("regions.default"))
				}
				t.Row(p.Bold(r.Code), status, mark)
			}
			if err := t.Flush(); err != nil {
				return err
			}
			p.Hint(i18n.T("regions.hint"))
			return nil
		},
	}
}
