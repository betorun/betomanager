package cli

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
	workloadsdomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

const (
	kindWeb     = workloadsdomain.KindWeb
	kindWorker  = workloadsdomain.KindWorker
	kindCron    = workloadsdomain.KindCron
	kindService = workloadsdomain.KindService
)

// processFlags : sorte du conteneur, commande de démarrage, horaire d'un cron.
type processFlags struct {
	kind        string
	command     string
	schedule    string
	timeZone    string
	maxDuration time.Duration
	// extraPorts : --extra-port nom=port (répétable).
	extraPorts []string
}

func (p *processFlags) addCreateFlags(f *pflag.FlagSet) {
	f.StringVar(&p.kind, "kind", kindWeb, i18n.T("flag.wl.kind"))
	f.StringVar(&p.command, "command", "", i18n.T("flag.wl.command"))
	f.StringVar(&p.schedule, "schedule", "", i18n.T("flag.wl.schedule"))
	f.StringVar(&p.timeZone, "timezone", "", i18n.T("flag.wl.timezone"))
	f.DurationVar(&p.maxDuration, "max-duration", 0, i18n.T("flag.wl.max_duration"))
	f.StringArrayVar(&p.extraPorts, "extra-port", nil, i18n.T("flag.wl.extra_port"))
}

func (p *processFlags) addUpdateFlags(f *pflag.FlagSet) {
	f.StringVar(&p.command, "command", "", i18n.T("flag.wl.command_update"))
	f.StringVar(&p.schedule, "schedule", "", i18n.T("flag.wl.schedule_update"))
}

func (p *processFlags) changed(cmd *cobra.Command) bool {
	f := cmd.Flags()
	return f.Changed("command") || f.Changed("schedule")
}

// applyCreate : un worker ou un cron n'a pas de port (l'API l'ignorerait,
// mais la valeur par défaut de --port ne doit pas laisser croire le contraire).
func (p *processFlags) applyCreate(cmd *cobra.Command, req *api.CreateWorkloadRequest) error {
	kind := strings.ToLower(strings.TrimSpace(p.kind))
	req.Kind = kind
	if kind == kindService {
		// Service interne : son vrai port est obligatoire, jamais public.
		if !cmd.Flags().Changed("port") {
			return errors.New(i18n.T("wl.kind.port_needed"))
		}
		if cmd.Flags().Changed("tls") {
			return errors.New(i18n.T("wl.kind.no_tls"))
		}
		req.TLSEnabled = false
	} else if kind != kindWeb && kind != "" {
		if cmd.Flags().Changed("port") || cmd.Flags().Changed("tls") {
			return errors.New(i18n.T("wl.kind.no_port", kind))
		}
		req.TargetPort, req.TLSEnabled = 0, false
	}
	if kind != kindCron && (p.schedule != "" || p.timeZone != "" || p.maxDuration > 0) {
		return errors.New(i18n.T("wl.kind.schedule_cron_only"))
	}
	if kind == kindCron && strings.TrimSpace(p.schedule) == "" {
		return errors.New(i18n.T("wl.kind.schedule_needed"))
	}
	if p.command != "" {
		parts, err := splitCommand(p.command)
		if err != nil {
			return err
		}
		req.Command = parts
	}
	req.Schedule, req.TimeZone = strings.TrimSpace(p.schedule), strings.TrimSpace(p.timeZone)
	req.MaxDurationSeconds = int(p.maxDuration / time.Second)
	if len(p.extraPorts) > 0 {
		if kind == kindCron {
			return errors.New(i18n.T("wl.extra_port.cron"))
		}
		ports, err := parseExtraPorts(p.extraPorts)
		if err != nil {
			return err
		}
		req.ExtraPorts = ports
	}
	return nil
}

// parseExtraPorts : --extra-port nom=port (ex. smtp=1025). Les règles (nom,
// doublons, port principal) sont vérifiées par l'API.
func parseExtraPorts(list []string) ([]api.ExtraPort, error) {
	out := make([]api.ExtraPort, 0, len(list))
	for _, v := range list {
		name, port, ok := strings.Cut(v, "=")
		n, err := strconv.Atoi(strings.TrimSpace(port))
		if !ok || strings.TrimSpace(name) == "" || err != nil {
			return nil, errors.New(i18n.T("wl.extra_port.invalid", v))
		}
		out = append(out, api.ExtraPort{Name: strings.TrimSpace(name), Port: n})
	}
	return out, nil
}

// internalAddresses : adresses nom:port des ports supplémentaires (rendues
// par l'API ; sinon relues de la spec).
func internalAddresses(w *api.Workload) []string {
	if len(w.InternalAddresses) > 0 {
		return w.InternalAddresses
	}
	return workloadsdomain.InternalAddresses(w.Name, workloadsdomain.SpecExtraPorts(w.Spec))
}

// applyUpdate : --command "" rend la commande de l'image.
func (p *processFlags) applyUpdate(cmd *cobra.Command, req *api.UpdateWorkloadRequest) error {
	if cmd.Flags().Changed("command") {
		parts, err := splitCommand(p.command)
		if err != nil {
			return err
		}
		if parts == nil {
			parts = []string{}
		}
		req.Command = &parts
	}
	req.Schedule = strings.TrimSpace(p.schedule)
	return nil
}

// splitCommand : découpe façon shell (guillemets simples ou doubles, \),
// sans interprétation : ni variables, ni redirections. Pour un vrai shell :
// --command "sh -c 'npm run migrate && npm start'".
func splitCommand(s string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		esc   bool
		has   bool
	)
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc, has = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, has = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if quote != 0 || esc {
		return nil, errors.New(i18n.T("wl.command.unterminated"))
	}
	if has {
		out = append(out, cur.String())
	}
	return out, nil
}

// workloadSpec : spec enregistrée (horaire, fuseau, commande).
func workloadSpec(w *api.Workload) api.CreateWorkloadRequest {
	var spec api.CreateWorkloadRequest
	if len(w.Spec) > 0 {
		_ = json.Unmarshal(w.Spec, &spec)
	}
	return spec
}

func kindLabel(kind string) string {
	switch kind {
	case kindWorker:
		return i18n.T("wl.kind.worker")
	case kindCron:
		return i18n.T("wl.kind.cron")
	}
	return i18n.T("wl.kind.web")
}

// kindTag : « (cron) » à côté du nom ; rien pour le web.
func kindTag(p *ui.Printer, w *api.Workload) string {
	if w.Kind == "" || w.Kind == kindWeb {
		return ""
	}
	return " " + p.Dim("("+w.Kind+")")
}

// newWorkloadsRun : exécuter un cron tout de suite, hors horaire.
func newWorkloadsRun() *cobra.Command {
	return &cobra.Command{
		Use:     "run " + i18n.T("arg.name"),
		Short:   i18n.T("wl.run.short"),
		Example: "betorun workloads run nightly",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, w, err := workloadByName(cmd, args[0])
			if err != nil {
				return err
			}
			if w.Kind != kindCron {
				return errors.New(i18n.T("wl.run.not_cron", w.Name))
			}
			if err := cl.WorkloadAction(cmd.Context(), w.ID.String(), "restart"); err != nil {
				return err
			}
			printer(cmd).Success(i18n.T("wl.run.ok", w.Name))
			printer(cmd).Hint(i18n.T("wl.run.logs", w.Name))
			return nil
		},
	}
}

// internalURL : adresse dans le réseau privé du projet (web : http://nom ;
// service : nom:port).
func internalURL(w *api.Workload) string {
	if w.Name == "" {
		return ""
	}
	switch w.Kind {
	case "", kindWeb:
		return "http://" + w.Name
	case kindService:
		if w.TargetPort > 0 {
			return w.Name + ":" + strconv.Itoa(w.TargetPort)
		}
	}
	return ""
}
