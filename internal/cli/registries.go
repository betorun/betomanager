package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
)

// newRegistries : « registries » : accès du projet à des registres d'images
// externes. Le jeton n'est jamais accepté en argument (historique du shell,
// liste des processus) : il est demandé, ou lu sur l'entrée standard.
func newRegistries() *cobra.Command {
	c := &cobra.Command{Use: "registries", Aliases: []string{"registry"}, Short: i18n.T("reg.short"),
		Long: i18n.T("reg.long"),
		Example: "betorun registries add ghcr.io --user moi  # " + i18n.T("ex.reg.prompt") + "\n" +
			"echo \"$GHCR_TOKEN\" | betorun registries add ghcr.io --user moi  # " + i18n.T("ex.reg.stdin") + "\n" +
			"betorun registries test ghcr.io\n" +
			"betorun registries list\n" +
			"betorun registries remove ghcr.io"}

	var user string
	add := &cobra.Command{
		Use: "add " + i18n.T("arg.registry"), Short: i18n.T("reg.add.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(user) == "" {
				return errors.New(i18n.T("reg.add.need_user"))
			}
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			token, err := readCode(i18n.T("reg.add.prompt", args[0]))
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New(i18n.T("reg.add.empty"))
			}
			r, err := cl.PutRegistry(cmd.Context(), project, args[0], user, token)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, r); done {
				return err
			}
			p := printer(cmd)
			p.Success(i18n.T("reg.add.done", r.Host))
			p.Hint(i18n.T("reg.add.hint", r.Host))
			return nil
		},
	}
	add.Flags().StringVarP(&user, "user", "u", "", i18n.T("flag.reg.user"))

	c.AddCommand(&cobra.Command{
		Use: "list", Short: i18n.T("reg.list.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			list, err := cl.ListRegistries(cmd.Context(), project)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list) == 0 {
				p.Hint(i18n.T("reg.list.empty"))
				return nil
			}
			t := p.Table(i18n.T("col.reg.host"), i18n.T("col.reg.user"), i18n.T("col.reg.updated"))
			for _, r := range list {
				t.Row(r.Host, r.Username, r.UpdatedAt.Local().Format("2006-01-02 15:04"))
			}
			return t.Flush()
		},
	}, add, &cobra.Command{
		Use: "test " + i18n.T("arg.registry"), Short: i18n.T("reg.test.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			res, err := cl.TestRegistry(cmd.Context(), project, args[0])
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, res); done {
				return err
			}
			return printRegistryTest(printer(cmd), args[0], res)
		},
	}, &cobra.Command{
		Use: "remove " + i18n.T("arg.registry"), Aliases: []string{"rm", "delete"}, Short: i18n.T("reg.remove.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			if err := cl.DeleteRegistry(cmd.Context(), project, args[0]); err != nil {
				return err
			}
			printer(cmd).Success(i18n.T("reg.remove.done", args[0]))
			return nil
		},
	})
	return c
}

func printRegistryTest(p *ui.Printer, host string, res *api.RegistryTest) error {
	switch res.Result {
	case "ok":
		p.Success(i18n.T("reg.test.ok", host))
		return nil
	case "denied":
		return errors.New(i18n.T("reg.test.denied", host))
	default:
		return errors.New(i18n.T("reg.test.unreachable", host))
	}
}
