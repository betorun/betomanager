package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
)

// Ce que la CLI lit des services : ses propres types, sans dépendre du
// serveur (le module addons tire Kubernetes).
type cliAddonVersion struct {
	ID    string `json:"id"`
	Major int    `json:"major"`
}

type cliAddonTemplate struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Category string            `json:"category"`
	Port     int               `json:"port"`
	Variable string            `json:"variable"`
	Versions []cliAddonVersion `json:"versions"`
	Modes    []struct {
		ID         string `json:"id"`
		Persistent bool   `json:"persistent"`
	} `json:"modes"`
	Disks       []string `json:"disks"`
	Recommended string   `json:"recommended_version"`
}

type cliAddonSize struct {
	ID         string `json:"id"`
	CPURequest string `json:"cpu_request"`
	CPULimit   string `json:"cpu_limit"`
	Memory     string `json:"memory"`
}

type cliAddonCatalog struct {
	Templates []cliAddonTemplate `json:"templates"`
	Sizes     []cliAddonSize     `json:"sizes"`
	Upcoming  []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"upcoming"`
}

type cliAddonAttachment struct {
	WorkloadID   string `json:"workload_id"`
	WorkloadName string `json:"workload_name"`
	Variable     string `json:"variable"`
}

type cliAddon struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	Template         string               `json:"template"`
	Title            string               `json:"title"`
	Version          string               `json:"version"`
	LatestVersion    string               `json:"latest_version"`
	UpgradeAvailable bool                 `json:"upgrade_available"`
	Mode             string               `json:"mode,omitempty"`
	Size             string               `json:"size"`
	Disk             string               `json:"disk,omitempty"`
	Status           string               `json:"status"`
	Host             string               `json:"host"`
	Variable         string               `json:"variable"`
	Attachments      []cliAddonAttachment `json:"attachments"`
}

type cliAddonList struct {
	Addons []cliAddon `json:"addons"`
	Max    int        `json:"max"`
}

type cliAddonCreate struct {
	Template string `json:"template"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Mode     string `json:"mode"`
	Size     string `json:"size"`
	Disk     string `json:"disk"`
}

// addonTimeout : au-delà, le service est suivi dans la console.
const addonTimeout = 10 * time.Minute

// newAddons : « addons » (services additionnels : Redis via Valkey…, ADR 0013).
func newAddons() *cobra.Command {
	c := &cobra.Command{Use: "addons", Aliases: []string{"addon", "services"}, Short: i18n.T("addon.short"),
		Long: i18n.T("addon.long"),
		Example: "betorun addons catalog\n" +
			"# " + i18n.T("ex.addon.create") + "\n" +
			"betorun addons create redis files-taches --attach api --attach worker\n" +
			"# " + i18n.T("ex.addon.cache") + "\n" +
			"betorun addons create redis cache --mode cache --size mini\n" +
			"betorun addons create mariadb boutique-db --attach api\n" +
			"betorun addons attach cache web"}
	c.AddCommand(newAddonsCatalog(), newAddonsList(), newAddonsCreate(), newAddonsAttach(), newAddonsUpgrade(),
		newAddonsDelete(), newAddonsNotify())
	return c
}

func addonsPath(project string) string {
	return "/projects/" + url.PathEscape(project) + "/addons"
}

func fetchCatalog(ctx context.Context, cl *api.Client, project string) (*cliAddonCatalog, error) {
	var out cliAddonCatalog
	return &out, cl.Do(ctx, http.MethodGet, addonsPath(project)+"/templates", nil, &out)
}

func listAddons(ctx context.Context, cl *api.Client, project string) (*cliAddonList, error) {
	var out cliAddonList
	return &out, cl.Do(ctx, http.MethodGet, addonsPath(project), nil, &out)
}

func addonByName(ctx context.Context, cl *api.Client, project, ref string) (*cliAddon, error) {
	list, err := listAddons(ctx, cl, project)
	if err != nil {
		return nil, err
	}
	for i := range list.Addons {
		if list.Addons[i].Name == ref || list.Addons[i].ID == ref {
			return &list.Addons[i], nil
		}
	}
	return nil, errors.New(i18n.T("addon.not_found", ref))
}

// containerByName : conteneur du projet à brancher (jamais un porteur de service).
func containerByName(ctx context.Context, cl *api.Client, project, name string) (*api.Workload, error) {
	list, err := cl.ListWorkloads(ctx, project)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			if list[i].ManagedBy != "" {
				return nil, errors.New(i18n.T("addon.attach.managed", name))
			}
			return &list[i], nil
		}
	}
	return nil, errors.New(i18n.T("wl.not_found", name))
}

func sizeLabel(id string) string {
	switch id {
	case "mini", "small", "medium":
		return i18n.T("addon.size." + id)
	}
	return id
}

func newAddonsCatalog() *cobra.Command {
	return &cobra.Command{
		Use: "catalog", Short: i18n.T("addon.catalog.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			cat, err := fetchCatalog(cmd.Context(), cl, project)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, cat); done {
				return err
			}
			p := printer(cmd)
			for _, t := range cat.Templates {
				versions := make([]string, 0, len(t.Versions))
				for _, v := range t.Versions {
					label := v.ID
					if v.ID == t.Recommended {
						label += " " + i18n.T("addon.catalog.recommended")
					}
					versions = append(versions, label)
				}
				modes := make([]string, 0, len(t.Modes))
				for _, m := range t.Modes {
					modes = append(modes, m.ID)
				}
				p.Title(t.Title, t.ID)
				p.Fields(
					[2]string{i18n.T("addon.field.versions"), strings.Join(versions, ", ")},
					[2]string{i18n.T("addon.field.modes"), strings.Join(modes, ", ")},
					[2]string{i18n.T("addon.field.disks"), strings.Join(t.Disks, ", ")},
					[2]string{i18n.T("addon.field.variable"), t.Variable},
				)
			}
			if len(cat.Sizes) > 0 {
				t := p.Table(i18n.T("addon.col.size"), i18n.T("addon.col.memory"), "CPU")
				for _, s := range cat.Sizes {
					t.Row(p.Bold(s.ID), s.Memory, s.CPURequest+" → "+s.CPULimit)
				}
				if err := t.Flush(); err != nil {
					return err
				}
			}
			if len(cat.Upcoming) > 0 {
				names := make([]string, 0, len(cat.Upcoming))
				for _, u := range cat.Upcoming {
					names = append(names, u.ID)
				}
				p.Hint(i18n.T("addon.catalog.upcoming", strings.Join(names, ", ")))
			}
			return nil
		},
	}
}

func newAddonsList() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: i18n.T("addon.list.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			list, err := listAddons(cmd.Context(), cl, project)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list.Addons); done {
				return err
			}
			p := printer(cmd)
			if len(list.Addons) == 0 {
				p.Hint(i18n.T("addon.list.empty"))
				return nil
			}
			t := p.Table(i18n.T("wl.col.name"), i18n.T("addon.col.service"), i18n.T("wl.col.status"),
				i18n.T("addon.col.size"), i18n.T("addon.col.address"), i18n.T("addon.col.attached"))
			for _, a := range list.Addons {
				service := a.Title + " " + a.Version
				if a.UpgradeAvailable {
					service += " " + p.Yellow(i18n.T("addon.list.upgrade", a.LatestVersion))
				}
				size := sizeLabel(a.Size)
				if a.Disk != "" {
					size += " · " + a.Disk
				}
				names := make([]string, 0, len(a.Attachments))
				for _, x := range a.Attachments {
					names = append(names, x.WorkloadName)
				}
				attached := strings.Join(names, ", ")
				if attached == "" {
					attached = "—"
				}
				t.Row(p.Bold(a.Name), service, workloadStatus(p, a.Status), size, a.Host, attached)
			}
			if err := t.Flush(); err != nil {
				return err
			}
			if list.Max > 0 {
				p.Hint(i18n.T("addon.list.quota", len(list.Addons), list.Max))
			}
			return nil
		},
	}
}

func newAddonsCreate() *cobra.Command {
	var req cliAddonCreate
	var attach []string
	var noWait bool
	c := &cobra.Command{
		Use:   "create " + i18n.T("arg.template") + " " + i18n.T("arg.name"),
		Short: i18n.T("addon.create.short"),
		Long:  i18n.T("addon.create.long"),
		Example: "betorun addons create redis files-taches --attach api --attach worker\n" +
			"betorun addons create redis cache --mode cache --size mini",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx, p := cmd.Context(), printer(cmd)
			// Conteneurs vérifiés avant la création : une faute de frappe ne
			// laisse pas un service créé à moitié branché.
			targets := make([]*api.Workload, 0, len(attach))
			for _, name := range attach {
				w, err := containerByName(ctx, cl, project, name)
				if err != nil {
					return err
				}
				targets = append(targets, w)
			}
			req.Template, req.Name = args[0], args[1]
			var a cliAddon
			if err := cl.Do(ctx, http.MethodPost, addonsPath(project), req, &a); err != nil {
				return err
			}
			p.Success(i18n.T("addon.create.started", a.Name, a.Title, a.Version))
			for _, w := range targets {
				msg, err := attachAddon(ctx, cl, project, &a, w, "")
				if err != nil {
					p.Fail(i18n.T("addon.attach.failed", w.Name, describe(err)))
					continue
				}
				p.Success(msg)
			}
			if noWait {
				if done, err := wantJSON(cmd, a); done {
					return err
				}
				p.Hint(i18n.T("addon.address", a.Host))
				return nil
			}
			ready, err := waitAddon(cmd, cl, project, a.ID, a.Name)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, ready); done {
				return err
			}
			p.Hint(i18n.T("addon.address", ready.Host))
			if len(targets) == 0 {
				p.Hint(i18n.T("addon.create.next", ready.Name))
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&req.Version, "version", "", i18n.T("flag.addon.version"))
	f.StringVar(&req.Mode, "mode", "", i18n.T("flag.addon.mode"))
	f.StringVar(&req.Size, "size", "", i18n.T("flag.addon.size"))
	f.StringVar(&req.Disk, "disk", "", i18n.T("flag.addon.disk"))
	f.StringArrayVar(&attach, "attach", nil, i18n.T("flag.addon.attach"))
	f.BoolVar(&noWait, "no-wait", false, i18n.T("flag.addon.no_wait"))
	return c
}

// waitAddon : jusqu'à ce que le conteneur porteur soit en service.
func waitAddon(cmd *cobra.Command, cl *api.Client, project, id, name string) (*cliAddon, error) {
	ctx, p := cmd.Context(), printer(cmd)
	p.Hint(i18n.T("addon.create.waiting"))
	deadline := time.Now().Add(addonTimeout)
	for {
		var a cliAddon
		if err := cl.Do(ctx, http.MethodGet, addonsPath(project)+"/"+url.PathEscape(id), nil, &a); err != nil {
			return nil, err
		}
		switch tone := workloadTone(a.Status); {
		case a.Status == "RUNNING":
			p.Success(i18n.T("addon.create.ready", name))
			return &a, nil
		case tone == ui.ToneError:
			return nil, errors.New(i18n.T("addon.create.failed", name, a.Status))
		}
		if time.Now().After(deadline) {
			return nil, errors.New(i18n.T("addon.create.timeout", name))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(workloadPollEvery):
		}
	}
}

// attachAddon : branche le service ; renvoie le message de réussite (la
// variable, et celle de la clé pour Meilisearch).
func attachAddon(ctx context.Context, cl *api.Client, project string, a *cliAddon, w *api.Workload, variable string) (string, error) {
	body := map[string]string{"workload_id": w.ID.String()}
	if variable != "" {
		body["variable"] = variable
	}
	var out struct {
		Variable    string `json:"variable"`
		KeyVariable string `json:"key_variable"`
	}
	if err := cl.Do(ctx, http.MethodPost, addonsPath(project)+"/"+url.PathEscape(a.ID)+"/attach", body, &out); err != nil {
		return "", err
	}
	if out.KeyVariable != "" {
		return i18n.T("addon.attach.ok_key", out.Variable, out.KeyVariable, w.Name), nil
	}
	return i18n.T("addon.attach.ok", out.Variable, w.Name), nil
}

func newAddonsAttach() *cobra.Command {
	var variable string
	c := &cobra.Command{
		Use:     "attach " + i18n.T("arg.addon") + " " + i18n.T("arg.container"),
		Short:   i18n.T("addon.attach.short"),
		Long:    i18n.T("addon.attach.long"),
		Example: "betorun addons attach cache web\nbetorun addons attach files-taches worker --variable QUEUE_URL",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			a, err := addonByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			w, err := containerByName(ctx, cl, project, args[1])
			if err != nil {
				return err
			}
			msg, err := attachAddon(ctx, cl, project, a, w, variable)
			if err != nil {
				return err
			}
			printer(cmd).Success(msg)
			return nil
		},
	}
	c.Flags().StringVar(&variable, "variable", "", i18n.T("flag.addon.variable"))
	return c
}

func newAddonsUpgrade() *cobra.Command {
	var version string
	var yes bool
	c := &cobra.Command{
		Use:   "upgrade " + i18n.T("arg.addon"),
		Short: i18n.T("addon.upgrade.short"),
		Long:  i18n.T("addon.upgrade.long"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx, p := cmd.Context(), printer(cmd)
			a, err := addonByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			target := version
			if target == "" {
				target = a.LatestVersion
			}
			if target == a.Version {
				p.Hint(i18n.T("addon.upgrade.none", a.Name, a.Version))
				return nil
			}
			// Changement de version majeure : à confirmer (clients à vérifier).
			if major(target) != major(a.Version) && !yes &&
				!confirm(cmd, i18n.T("addon.upgrade.confirm_major", a.Version, target, a.Name), a.Name) {
				return errors.New(i18n.T("addon.upgrade.aborted"))
			}
			var out cliAddon
			if err := cl.Do(ctx, http.MethodPost, addonsPath(project)+"/"+url.PathEscape(a.ID)+"/upgrade",
				map[string]string{"version": target}, &out); err != nil {
				return err
			}
			p.Success(i18n.T("addon.upgrade.ok", a.Name, target))
			return nil
		},
	}
	c.Flags().StringVar(&version, "version", "", i18n.T("flag.addon.upgrade_version"))
	c.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	return c
}

// major : « 9.1 » → « 9 ».
func major(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}

func newAddonsDelete() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "delete " + i18n.T("arg.addon"),
		Short: i18n.T("addon.delete.short"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx, p := cmd.Context(), printer(cmd)
			a, err := addonByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			if !yes && !confirm(cmd, i18n.T("addon.delete.confirm", a.Name), a.Name) {
				return errors.New(i18n.T("addon.delete.aborted"))
			}
			var out struct {
				StillAttached []cliAddonAttachment `json:"still_attached"`
			}
			if err := cl.Do(ctx, http.MethodDelete, addonsPath(project)+"/"+url.PathEscape(a.ID), nil, &out); err != nil {
				return err
			}
			p.Success(i18n.T("addon.delete.ok", a.Name))
			if len(out.StillAttached) > 0 {
				names := make([]string, 0, len(out.StillAttached))
				for _, x := range out.StillAttached {
					names = append(names, fmt.Sprintf("%s (%s)", x.WorkloadName, x.Variable))
				}
				p.Warn(i18n.T("addon.delete.still", strings.Join(names, ", ")))
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	return c
}

// newAddonsNotify : « Me prévenir » d'un service annoncé (personnel, hors projet).
func newAddonsNotify() *cobra.Command {
	var off bool
	c := &cobra.Command{
		Use:     "notify " + i18n.T("arg.template"),
		Short:   i18n.T("addon.notify.short"),
		Example: "betorun addons notify mariadb\nbetorun addons notify mariadb --off",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			cl := client(s)
			method := http.MethodPut
			if off {
				method = http.MethodDelete
			}
			var out map[string]any
			if err := cl.Do(cmd.Context(), method, "/addons/waitlist/"+url.PathEscape(args[0]), nil, &out); err != nil {
				return err
			}
			if off {
				printer(cmd).Success(i18n.T("addon.notify.off", args[0]))
			} else {
				printer(cmd).Success(i18n.T("addon.notify.on", args[0]))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&off, "off", false, i18n.T("flag.addon.notify_off"))
	return c
}
