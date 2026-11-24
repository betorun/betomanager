package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
	storagedomain "github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// newStorage : « storage » (buckets S3 du projet, ADR 0015).
func newStorage() *cobra.Command {
	c := &cobra.Command{Use: "storage", Aliases: []string{"s3", "buckets"}, Short: i18n.T("st.short"),
		Example: "betorun storage create uploads --quota 10\n" +
			"betorun storage attach uploads web  # " + i18n.T("ex.st.attach") + "\n" +
			"betorun storage keys create uploads ci --read-only\n" +
			"betorun storage cp ./logo.png uploads:images/\n" +
			"betorun storage ls uploads:images/\n" +
			"betorun storage list"}
	c.AddCommand(newStorageList(), newStorageCreate(), newStorageQuota(), newStorageAttach(), newStorageKeys(),
		newStorageLs(), newStorageCp(), newStorageRm(), newStorageLink(), newStoragePublic(), newStorageDelete())
	return c
}

func storagePath(project string) string {
	return "/projects/" + url.PathEscape(project) + "/storage/buckets"
}

func listBuckets(ctx context.Context, cl *api.Client, project string) ([]storagedomain.BucketView, error) {
	var out []storagedomain.BucketView
	return out, cl.Do(ctx, http.MethodGet, storagePath(project), nil, &out)
}

func bucketByName(ctx context.Context, cl *api.Client, project, ref string) (*storagedomain.BucketView, error) {
	list, err := listBuckets(ctx, cl, project)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref || list[i].ID == ref || list[i].Bucket == ref {
			return &list[i], nil
		}
	}
	return nil, errors.New(i18n.T("st.not_found", ref))
}

func newStorageList() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: i18n.T("st.list.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			list, err := listBuckets(cmd.Context(), cl, project)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list) == 0 {
				p.Hint(i18n.T("st.list.empty"))
				return nil
			}
			t := p.Table(i18n.T("wl.col.name"), i18n.T("st.col.bucket"), i18n.T("proj.du.col.used"), i18n.T("st.col.keys"))
			for _, b := range list {
				used := humanBytes(b.UsedBytes) + " / " + humanBytes(b.QuotaBytes) + " (" + usedPercent(b.UsedBytes, b.QuotaBytes) + ")"
				if b.OverQuota {
					used = p.Status(ui.ToneError, used+" · "+i18n.T("st.read_only"))
				}
				name := p.Bold(b.Name)
				if b.Public {
					name += " " + p.Status(ui.ToneProgress, i18n.T("st.public_tag"))
				}
				if b.Suspended {
					used = p.Status(ui.ToneError, i18n.T("st.suspended"))
				}
				t.Row(name, b.Bucket, used, strconv.Itoa(len(b.Keys)))
			}
			if err := t.Flush(); err != nil {
				return err
			}
			p.Hint(i18n.T("st.list.endpoint", list[0].Endpoint))
			return nil
		},
	}
}

func newStorageCreate() *cobra.Command {
	var quota int
	c := &cobra.Command{
		Use: "create " + i18n.T("arg.name"), Short: i18n.T("st.create.short"), Long: i18n.T("st.create.long"),
		Example: "betorun storage create uploads\nbetorun storage create exports --quota 25", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			var out storagedomain.BucketView
			body := map[string]any{"name": args[0]}
			if quota > 0 {
				body["quota_gb"] = quota
			}
			if err := cl.Do(cmd.Context(), http.MethodPost, storagePath(project), body, &out); err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			p := printer(cmd)
			p.Success(i18n.T("st.create.ok", out.Name, humanBytes(out.QuotaBytes)))
			p.Fields([2]string{i18n.T("st.col.bucket"), out.Bucket}, [2]string{"Endpoint", out.Endpoint}, [2]string{"Region", out.Region})
			p.Hint(i18n.T("st.create.next", out.Name))
			return nil
		},
	}
	c.Flags().IntVar(&quota, "quota", 0, i18n.T("st.flag.quota"))
	return c
}

func newStorageQuota() *cobra.Command {
	return &cobra.Command{
		Use: "quota " + i18n.T("arg.bucket") + " " + i18n.T("arg.gb"), Short: i18n.T("st.quota.short"),
		Example: "betorun storage quota uploads 25", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			gb, err := strconv.Atoi(args[1])
			if err != nil {
				return errors.New(i18n.T("st.quota.invalid"))
			}
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			var out storagedomain.BucketView
			if err := cl.Do(ctx, http.MethodPatch, storagePath(project)+"/"+url.PathEscape(b.ID), map[string]any{"quota_gb": gb}, &out); err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			printer(cmd).Success(i18n.T("st.quota.ok", out.Name, humanBytes(out.QuotaBytes)))
			return nil
		},
	}
}

func newStorageAttach() *cobra.Command {
	var prefix string
	var readOnly bool
	c := &cobra.Command{
		Use:   "attach " + i18n.T("arg.bucket") + " " + i18n.T("arg.container"),
		Short: i18n.T("st.attach.short"), Long: i18n.T("st.attach.long"),
		Example: "betorun storage attach uploads web\n" +
			"betorun storage attach exports worker --prefix EXPORTS --read-only",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			workloads, err := cl.ListWorkloads(ctx, project)
			if err != nil {
				return err
			}
			workloadID := ""
			for _, w := range workloads {
				if w.Name == args[1] || w.ID.String() == args[1] {
					workloadID = w.ID.String()
					break
				}
			}
			if workloadID == "" {
				return errors.New(i18n.T("wl.not_found", args[1]))
			}
			body := map[string]string{"workload_id": workloadID}
			if prefix != "" {
				body["prefix"] = prefix
			}
			if readOnly {
				body["access"] = storagedomain.AccessReadOnly
			}
			p := printer(cmd)
			var out storagedomain.AttachResponse
			path := storagePath(project) + "/" + url.PathEscape(b.ID) + "/attach"
			err = retryWhileDeploying(ctx, p, args[1], "st.attach.busy", "st.attach.busy_timeout", func() error {
				return cl.Do(ctx, http.MethodPost, path, body, &out)
			})
			if err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			p.Success(i18n.T("st.attach.ok", b.Name, args[1]))
			for _, v := range out.Variables {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+v)
			}
			if out.Redeploy {
				p.Hint(i18n.T("db.attach.redeploy", args[1], args[1]))
			}
			return nil
		},
	}
	c.Flags().StringVar(&prefix, "prefix", "", i18n.T("st.flag.prefix"))
	c.Flags().BoolVar(&readOnly, "read-only", false, i18n.T("st.flag.read_only"))
	return c
}

func newStorageKeys() *cobra.Command {
	c := &cobra.Command{Use: "keys", Short: i18n.T("st.keys.short")}
	var readOnly bool
	create := &cobra.Command{
		Use: "create " + i18n.T("arg.bucket") + " " + i18n.T("arg.name"), Short: i18n.T("st.keys.create.short"),
		Long: i18n.T("st.keys.create.long"), Example: "betorun storage keys create uploads ci --read-only", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			body := map[string]string{"name": args[1]}
			if readOnly {
				body["access"] = storagedomain.AccessReadOnly
			}
			var out storagedomain.CreatedKey
			if err := cl.Do(ctx, http.MethodPost, storagePath(project)+"/"+url.PathEscape(b.ID)+"/keys", body, &out); err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			p := printer(cmd)
			p.Success(i18n.T("st.keys.create.ok", out.Key.Name, b.Name))
			p.Fields([2]string{"Endpoint", out.Endpoint}, [2]string{"Region", out.Region}, [2]string{i18n.T("st.col.bucket"), out.Bucket},
				[2]string{"Access key ID", out.Key.AccessKeyID}, [2]string{"Secret access key", out.SecretAccessKey})
			p.Hint(i18n.T("st.keys.create.once"))
			return nil
		},
	}
	create.Flags().BoolVar(&readOnly, "read-only", false, i18n.T("st.flag.read_only"))
	list := &cobra.Command{
		Use: "list " + i18n.T("arg.bucket"), Short: i18n.T("st.keys.list.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			b, err := bucketByName(cmd.Context(), cl, project, args[0])
			if err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, b.Keys); ok {
				return err
			}
			p := printer(cmd)
			if len(b.Keys) == 0 {
				p.Hint(i18n.T("st.keys.empty", b.Name))
				return nil
			}
			t := p.Table(i18n.T("wl.col.name"), "Access key ID", i18n.T("st.col.access"), i18n.T("st.col.kind"))
			for _, k := range b.Keys {
				kind := i18n.T("st.kind.manual")
				if k.Kind == storagedomain.KindAttachment {
					kind = i18n.T("st.kind.attachment", k.Prefix)
				}
				t.Row(p.Bold(k.Name), k.AccessKeyID, i18n.T("st.access."+k.Access), kind)
			}
			return t.Flush()
		},
	}
	var yes bool
	revoke := &cobra.Command{
		Use: "revoke " + i18n.T("arg.bucket") + " " + i18n.T("arg.key"), Short: i18n.T("st.keys.revoke.short"),
		Long: i18n.T("st.keys.revoke.long"), Example: "betorun storage keys revoke uploads ci\nbetorun storage keys revoke uploads web",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			var key *storagedomain.KeyView
			for i := range b.Keys {
				if k := b.Keys[i]; k.Name == args[1] || k.ID == args[1] || k.AccessKeyID == args[1] {
					key = &b.Keys[i]
					break
				}
			}
			if key == nil {
				return errors.New(i18n.T("st.keys.not_found", args[1], b.Name))
			}
			if !yes && !confirm(cmd, i18n.T("st.keys.revoke.confirm", key.Name), key.Name) {
				return errors.New(i18n.T("st.aborted"))
			}
			if err := cl.Do(ctx, http.MethodDelete, storagePath(project)+"/"+url.PathEscape(b.ID)+"/keys/"+url.PathEscape(key.ID), nil, nil); err != nil {
				return err
			}
			printer(cmd).Success(i18n.T("st.keys.revoke.ok", key.Name))
			return nil
		},
	}
	revoke.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	c.AddCommand(list, create, newStorageKeysRotate(), revoke)
	return c
}

func newStorageDelete() *cobra.Command {
	var yes, purge bool
	c := &cobra.Command{
		Use: "delete " + i18n.T("arg.bucket"), Short: i18n.T("st.delete.short"), Long: i18n.T("st.delete.long"),
		Example: "betorun storage delete uploads\nbetorun storage delete uploads --purge", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			prompt := i18n.T("st.delete.confirm", b.Name)
			if purge {
				prompt = i18n.T("st.delete.confirm_purge", b.Name)
			}
			if !yes && !confirm(cmd, prompt, b.Name) {
				return errors.New(i18n.T("st.aborted"))
			}
			path := storagePath(project) + "/" + url.PathEscape(b.ID)
			if purge {
				path += "?confirm=" + url.QueryEscape(b.Name)
			}
			if err := cl.Do(ctx, http.MethodDelete, path, nil, nil); err != nil {
				return err
			}
			printer(cmd).Success(i18n.T("st.delete.ok", b.Name))
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	c.Flags().BoolVar(&purge, "purge", false, i18n.T("st.flag.purge"))
	return c
}
