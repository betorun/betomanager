package cli

import (
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
)

// variableNameRe : même règle que l'API (workloads/domain), vérifiée avant
// toute saisie : un mot de passe tapé à la place du nom est refusé tout de
// suite, au lieu d'ouvrir une invite à son nom.
var variableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// checkVariableNames : le nom fautif n'est pas répété (ce peut être un secret).
func checkVariableNames(names []string) error {
	for _, name := range names {
		if strings.Contains(name, "=") {
			return errors.New(i18n.T("wl.secrets.no_argv"))
		}
		if !variableNameRe.MatchString(name) {
			return errors.New(i18n.T("wl.env.bad_name"))
		}
	}
	return nil
}

// secretsFromEnv : valeurs lues dans les variables d'environnement du même nom.
func secretsFromEnv(names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		if strings.Contains(name, "=") {
			return nil, errors.New(i18n.T("wl.secrets.no_argv"))
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, errors.New(i18n.T("wl.secrets.env_missing", name))
		}
		out[name] = value
	}
	return out, nil
}

func applyVariables(cmd *cobra.Command, name string, req api.PatchVariablesRequest) error {
	cl, w, err := workloadByName(cmd, name)
	if err != nil {
		return err
	}
	var res *api.VariablesResponse
	err = retryWhileDeploying(cmd.Context(), printer(cmd), w.Name, "wl.env.busy", "db.attach.busy_timeout", func() (err error) {
		res, err = cl.PatchVariables(cmd.Context(), w.ID.String(), req)
		return err
	})
	if err != nil {
		return err
	}
	if done, err := wantJSON(cmd, res); done {
		return err
	}
	p := printer(cmd)
	if res.Changed {
		p.Success(i18n.T("wl.env.applied"))
		return waitWorkload(cmd, cl, w.ID.String(), w.Name)
	}
	p.Hint(i18n.T("wl.env.unchanged"))
	return nil
}

func listVariables(cmd *cobra.Command, name string) error {
	cl, w, err := workloadByName(cmd, name)
	if err != nil {
		return err
	}
	res, err := cl.ListVariables(cmd.Context(), w.ID.String())
	if err != nil {
		return err
	}
	if done, err := wantJSON(cmd, res); done {
		return err
	}
	p := printer(cmd)
	if len(res.Variables) == 0 {
		p.Hint(i18n.T("wl.env.empty", name))
		return nil
	}
	t := p.Table(i18n.T("col.var.name"), i18n.T("col.var.kind"), i18n.T("col.var.value"), i18n.T("col.var.updated"))
	for _, v := range res.Variables {
		kind, value := i18n.T("var.kind.env"), v.Value
		if v.Secret {
			kind, value = p.Yellow(i18n.T("var.kind.secret")), p.Dim(i18n.T("var.hidden"))
		}
		t.Row(v.Name, kind, value, v.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return t.Flush()
}

func newVariablesList() *cobra.Command {
	return &cobra.Command{
		Use: "list " + i18n.T("arg.name"), Short: i18n.T("wl.env.list.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return listVariables(cmd, args[0]) },
	}
}

func newVariablesUnset() *cobra.Command {
	return &cobra.Command{
		Use: "unset " + i18n.T("arg.name") + " <KEY>...", Short: i18n.T("wl.env.unset.short"), Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkVariableNames(args[1:]); err != nil {
				return err
			}
			return applyVariables(cmd, args[0], api.PatchVariablesRequest{Unset: args[1:]})
		},
	}
}

// newWorkloadsEnv : « workloads env ».
func newWorkloadsEnv() *cobra.Command {
	c := &cobra.Command{Use: "env", Short: i18n.T("wl.env.short"),
		Example: "betorun workloads env set web LOG_LEVEL=debug PUBLIC_URL=https://exemple.sn\nbetorun workloads env list web\nbetorun workloads env unset web LOG_LEVEL"}
	c.AddCommand(newVariablesList(), newVariablesUnset(), &cobra.Command{
		Use: "set " + i18n.T("arg.name") + " <KEY=VALUE>...", Short: i18n.T("wl.env.set.short"), Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env := make(map[string]string, len(args)-1)
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					// L'argument n'est pas répété : ce peut être un secret collé par erreur.
					return errors.New(i18n.T("wl.env.bad"))
				}
				if !variableNameRe.MatchString(k) {
					return errors.New(i18n.T("wl.env.bad_name"))
				}
				env[k] = v
			}
			return applyVariables(cmd, args[0], api.PatchVariablesRequest{Env: env})
		},
	})
	return c
}

// newWorkloadsSecrets : « workloads secrets ». La valeur n'est jamais acceptée
// en argument : elle resterait dans l'historique du shell et dans la liste
// des processus.
func newWorkloadsSecrets() *cobra.Command {
	var fromEnv bool
	var fromFile string
	set := &cobra.Command{
		Use: "set " + i18n.T("arg.name") + " <KEY>...", Short: i18n.T("wl.secrets.set.short"), Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			names := args[1:]
			if err := checkVariableNames(names); err != nil {
				return err
			}
			secrets := make(map[string]string, len(names))
			switch {
			case fromFile != "":
				if len(names) != 1 {
					return errors.New(i18n.T("wl.secrets.from_file_one"))
				}
				raw, err := os.ReadFile(fromFile)
				if err != nil {
					return err
				}
				secrets[names[0]] = string(raw)
			case fromEnv:
				var err error
				if secrets, err = secretsFromEnv(names); err != nil {
					return err
				}
			default:
				for _, name := range names {
					if strings.Contains(name, "=") {
						return errors.New(i18n.T("wl.secrets.no_argv"))
					}
					value, err := readCode(i18n.T("wl.secrets.prompt", name))
					if err != nil {
						return err
					}
					if value == "" {
						return errors.New(i18n.T("wl.secrets.empty_value", name))
					}
					secrets[name] = value
				}
			}
			return applyVariables(cmd, args[0], api.PatchVariablesRequest{Secrets: secrets})
		},
	}
	set.Flags().BoolVar(&fromEnv, "from-env", false, i18n.T("flag.secret.from_env"))
	set.Flags().StringVar(&fromFile, "from-file", "", i18n.T("flag.secret.from_file"))
	c := &cobra.Command{Use: "secrets", Aliases: []string{"secret"}, Short: i18n.T("wl.secrets.short"),
		Example: "betorun workloads secrets set web DATABASE_URL  # " + i18n.T("ex.secrets.prompt") + "\n" +
			"betorun workloads secrets set web DATABASE_URL --from-env  # " + i18n.T("ex.secrets.env") + "\n" +
			"betorun workloads secrets set web TLS_KEY --from-file tls.key  # " + i18n.T("ex.secrets.file") + "\n" +
			"betorun workloads secrets list web\n" +
			"betorun workloads secrets unset web DATABASE_URL"}
	c.AddCommand(newVariablesList(), set, newVariablesUnset())
	return c
}
