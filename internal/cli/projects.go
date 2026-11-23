package cli

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/config"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
)

// newProjects : « projects » : lister, créer, choisir le projet par défaut
// des autres commandes (--project et BETORUN_PROJECT restent prioritaires).
func newProjects() *cobra.Command {
	c := &cobra.Command{Use: "projects", Aliases: []string{"project"}, Short: i18n.T("proj.short"),
		Long: i18n.T("proj.long"),
		Example: "betorun projects list\n" +
			"betorun projects create \"Boutique préprod\" --use\n" +
			"betorun projects use \"Boutique préprod\"\n" +
			"betorun projects rename \"Boutique préprod\" \"Boutique staging\""}

	var description, org string
	var use bool
	create := &cobra.Command{
		Use: "create " + i18n.T("arg.project_name"), Short: i18n.T("proj.create.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			res, err := client(s).CreateProject(cmd.Context(), strings.TrimSpace(args[0]), strings.TrimSpace(description), org)
			if err != nil {
				return err
			}
			if use && res.ProjectID != "" {
				if err := saveProject(res.ProjectID); err != nil {
					return err
				}
			}
			if done, err := wantJSON(cmd, res); done {
				return err
			}
			p := printer(cmd)
			p.Success(i18n.T("proj.create.done", args[0], res.ProjectID))
			if use {
				p.Hint(i18n.T("proj.use.done", args[0]))
			} else {
				p.Hint(i18n.T("proj.create.hint", res.ProjectID))
			}
			return nil
		},
	}
	create.Flags().StringVar(&description, "description", "", i18n.T("flag.proj.description"))
	create.Flags().StringVar(&org, "org", "", i18n.T("flag.proj.org"))
	create.Flags().BoolVar(&use, "use", false, i18n.T("flag.proj.use"))

	c.AddCommand(newProjectsDiskUsage())
	c.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: i18n.T("proj.list.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			list, err := client(s).ListProjects(cmd.Context())
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list.Projects) == 0 {
				p.Hint(i18n.T("proj.list.empty"))
				return nil
			}
			orgs := map[string]string{}
			for _, o := range list.Organizations {
				orgs[o.OrganizationID] = o.DisplayName
			}
			t := p.Table("", i18n.T("col.name"), i18n.T("col.id"), i18n.T("col.proj.org"), i18n.T("col.status"))
			for _, pr := range list.Projects {
				mark := ""
				if pr.ProjectID == s.ProjectID {
					mark = "*"
				}
				t.Row(mark, pr.Name, pr.ProjectID, orgs[pr.OrganizationID], pr.Status)
			}
			if err := t.Flush(); err != nil {
				return err
			}
			for _, o := range list.Organizations {
				p.Hint(i18n.T("proj.list.quota", o.DisplayName, strconv.Itoa(o.Projects), strconv.Itoa(o.MaxProjects)))
			}
			return nil
		},
	}, create, &cobra.Command{
		Use: "use " + i18n.T("arg.project"), Short: i18n.T("proj.use.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			pr, err := findProject(cmd, client(s), args[0])
			if err != nil {
				return err
			}
			if err := saveProject(pr.ProjectID); err != nil {
				return err
			}
			if done, err := wantJSON(cmd, pr); done {
				return err
			}
			p := printer(cmd)
			p.Success(i18n.T("proj.use.done", pr.Name+" ("+pr.ProjectID+")"))
			if os.Getenv(config.EnvProject) != "" {
				p.Warn(i18n.T("proj.use.env", config.EnvProject))
			}
			return nil
		},
	}, &cobra.Command{
		Use: "rename " + i18n.T("arg.project") + " " + i18n.T("arg.project_name"), Short: i18n.T("proj.rename.short"), Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			cl := client(s)
			pr, err := findProject(cmd, cl, args[0])
			if err != nil {
				return err
			}
			out, err := cl.RenameProject(cmd.Context(), pr.ProjectID, strings.TrimSpace(args[1]), nil)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, out); done {
				return err
			}
			printer(cmd).Success(i18n.T("proj.rename.done", pr.Name, out.Name))
			return nil
		},
	})
	return c
}

// findProject : par ID, ou par nom (sans tenir compte de la casse).
func findProject(cmd *cobra.Command, cl *api.Client, ref string) (*api.ProjectSummary, error) {
	list, err := cl.ListProjects(cmd.Context())
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	var byName []api.ProjectSummary
	for _, pr := range list.Projects {
		if pr.ProjectID == ref {
			return &pr, nil
		}
		if strings.EqualFold(pr.Name, ref) {
			byName = append(byName, pr)
		}
	}
	switch len(byName) {
	case 1:
		return &byName[0], nil
	case 0:
		return nil, errors.New(i18n.T("proj.not_found", ref))
	}
	// Même nom dans deux organisations : l'ID tranche.
	return nil, errors.New(i18n.T("proj.ambiguous", ref))
}

func saveProject(id string) error {
	return config.SaveSettings(func(st *config.Settings) { st.ProjectID = id })
}
