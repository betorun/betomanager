package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
)

// printDNSRecords : enregistrements à créer, avec le nom tel que le saisit la
// plupart des fournisseurs (relatif à la zone) et un conseil pour le sien.
func printDNSRecords(p *ui.Printer, d *api.WorkloadDomainView) error {
	if d.SharedProof {
		p.Hint(i18n.T("wl.dom.shared_proof", d.Hostname))
	}
	// API antérieure aux noms relatifs : nom complet.
	relative := d.Zone != ""
	if relative {
		p.Println(i18n.T("wl.dom.records", d.Zone))
	} else {
		p.Println(i18n.T("wl.dom.records_full"))
	}
	nameCol := i18n.T("col.dns.host")
	if !relative {
		nameCol = i18n.T("col.dns.name")
	}
	t := p.Table(i18n.T("col.dns.type"), nameCol, i18n.T("col.dns.value"))
	alternative := false
	for _, r := range d.Records {
		typ := r.Type
		if r.Alternative {
			typ += " *"
			alternative = true
		}
		name := r.Host
		if !relative || name == "" {
			name = r.Name
		}
		t.Row(typ, name, r.Value)
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if alternative {
		p.Hint(i18n.T("wl.dom.alias"))
	}
	if d.Apex && !alternative {
		p.Hint(i18n.T("wl.dom.apex"))
	}
	p.Hint(providerTip(d.DNSProvider))
	return nil
}

// providerTip : conseil propre au fournisseur DNS détecté.
func providerTip(provider string) string {
	switch provider {
	case "cloudflare", "ovh", "godaddy", "namecheap", "gandi", "google", "route53", "ionos", "hostinger", "lws":
		return i18n.T("wl.dom.tip." + provider)
	}
	return i18n.T("wl.dom.tip.generic")
}

// splitRoute : « www.exemple.sn/auth » → nom et chemin (« / » par défaut).
func splitRoute(arg string) (host, path string) {
	host, rest, found := strings.Cut(strings.TrimSpace(arg), "/")
	host = strings.TrimSuffix(host, ".")
	if !found {
		return host, "/"
	}
	path = "/" + strings.TrimRight(rest, "/")
	return host, path
}

func routeLabel(host, path string) string {
	if path == "" || path == "/" {
		return host
	}
	return host + path
}

// findWorkloadDomain : route du conteneur désignée par « nom[/chemin] ».
func findWorkloadDomain(cmd *cobra.Command, cl *api.Client, workloadID, arg string) (*api.WorkloadDomainView, error) {
	list, err := cl.ListWorkloadDomains(cmd.Context(), workloadID)
	if err != nil {
		return nil, err
	}
	host, path := splitRoute(arg)
	for i := range list {
		p := list[i].Path
		if p == "" {
			p = "/"
		}
		if strings.EqualFold(list[i].Hostname, host) && p == path {
			return &list[i], nil
		}
	}
	return nil, errors.New(i18n.T("wl.dom.not_found", arg))
}

// newWorkloadsDomains : « workloads domains ».
func newWorkloadsDomains() *cobra.Command {
	c := &cobra.Command{Use: "domains", Aliases: []string{"domain"}, Short: i18n.T("wl.dom.short"),
		Example: "betorun workloads domains add web www.exemple.sn\n" +
			"betorun workloads domains verify web www.exemple.sn\n" +
			"betorun workloads domains list web\n" +
			"# " + i18n.T("ex.dom.path") + "\n" +
			"betorun workloads domains add auth backoffice.exemple.sn/auth"}
	c.AddCommand(&cobra.Command{
		Use: "list " + i18n.T("arg.name"), Short: i18n.T("wl.dom.list.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, w, err := workloadByName(cmd, args[0])
			if err != nil {
				return err
			}
			list, err := cl.ListWorkloadDomains(cmd.Context(), w.ID.String())
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list) == 0 {
				p.Hint(i18n.T("wl.dom.empty", args[0]))
				return nil
			}
			t := p.Table(i18n.T("col.dom.host"), i18n.T("col.dom.status"))
			for _, d := range list {
				st := p.Status(ui.ToneProgress, i18n.T("wl.dom.st.pending"))
				if d.Verified {
					st = p.Status(ui.ToneOK, i18n.T("wl.dom.st.verified"))
				}
				t.Row(routeLabel(d.Hostname, d.Path), st)
			}
			return t.Flush()
		},
	}, &cobra.Command{
		Use: "add " + i18n.T("arg.name") + " " + i18n.T("arg.hostname"), Short: i18n.T("wl.dom.add.short"), Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, w, err := workloadByName(cmd, args[0])
			if err != nil {
				return err
			}
			host, path := splitRoute(args[1])
			d, err := cl.AddWorkloadDomain(cmd.Context(), w.ID.String(), host, path)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, d); done {
				return err
			}
			p := printer(cmd)
			if err := printDNSRecords(p, d); err != nil {
				return err
			}
			p.Hint(i18n.T("wl.dom.then", args[0], routeLabel(d.Hostname, d.Path)))
			return nil
		},
	}, &cobra.Command{
		Use: "verify " + i18n.T("arg.name") + " " + i18n.T("arg.hostname"), Short: i18n.T("wl.dom.verify.short"), Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, w, err := workloadByName(cmd, args[0])
			if err != nil {
				return err
			}
			found, err := findWorkloadDomain(cmd, cl, w.ID.String(), args[1])
			if err != nil {
				return err
			}
			d, err := cl.VerifyWorkloadDomain(cmd.Context(), w.ID.String(), found.ID.String())
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, d); done {
				return err
			}
			p := printer(cmd)
			if !d.Verified {
				p.Warn(i18n.T("wl.dom.pending", d.Hostname))
				return printDNSRecords(p, d)
			}
			p.Success(i18n.T("wl.dom.ok", d.Hostname))
			if !d.Pointing {
				p.Warn(i18n.T("wl.dom.not_pointing", d.Hostname))
			}
			return nil
		},
	}, &cobra.Command{
		Use: "remove " + i18n.T("arg.name") + " " + i18n.T("arg.hostname"), Short: i18n.T("wl.dom.remove.short"), Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, w, err := workloadByName(cmd, args[0])
			if err != nil {
				return err
			}
			found, err := findWorkloadDomain(cmd, cl, w.ID.String(), args[1])
			if err != nil {
				return err
			}
			if err := cl.RemoveWorkloadDomain(cmd.Context(), w.ID.String(), found.ID.String()); err != nil {
				return err
			}
			printer(cmd).Success(i18n.T("wl.dom.removed", found.Hostname))
			return nil
		},
	})
	return c
}
