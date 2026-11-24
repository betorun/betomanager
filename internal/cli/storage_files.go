package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	storagedomain "github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// Fichiers d'un bucket, par l'API (comme l'explorateur de la console) :
// « <bucket>:<chemin> » désigne un fichier ou un dossier (terminé par /).

// remoteRef : « uploads:images/logo.png » → (uploads, images/logo.png).
func remoteRef(s string) (bucket, key string, ok bool) {
	i := strings.Index(s, ":")
	// i < 2 : « C:\… » est une lettre de lecteur, pas un bucket.
	if i < 2 || strings.Contains(s[:i], "/") || strings.Contains(s[:i], `\`) {
		return "", "", false
	}
	return s[:i], strings.TrimPrefix(s[i+1:], "/"), true
}

func objectsPath(project, bucketID string) string {
	return storagePath(project) + "/" + url.PathEscape(bucketID) + "/objects"
}

// listAll : toutes les pages d'un dossier.
func listAll(ctx context.Context, cl *api.Client, project, bucketID, prefix string) ([]storagedomain.Object, error) {
	var all []storagedomain.Object
	after := ""
	for {
		q := url.Values{"prefix": {prefix}, "limit": {strconv.Itoa(storagedomain.MaxPageSize)}}
		if after != "" {
			q.Set("after", after)
		}
		var page storagedomain.ObjectPage
		if err := cl.Do(ctx, http.MethodGet, objectsPath(project, bucketID)+"?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Objects...)
		if page.Next == "" {
			return all, nil
		}
		after = page.Next
	}
}

func newStorageLs() *cobra.Command {
	return &cobra.Command{
		Use: "ls " + i18n.T("arg.bucket") + "[:" + i18n.T("arg.folder") + "]", Short: i18n.T("st.ls.short"),
		Example: "betorun storage ls uploads\nbetorun storage ls uploads:images/", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ref, prefix := args[0], ""
			if b, k, ok := remoteRef(args[0]); ok {
				ref, prefix = b, k
			}
			if prefix != "" && !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, ref)
			if err != nil {
				return err
			}
			list, err := listAll(ctx, cl, project, b.ID, prefix)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list) == 0 {
				p.Hint(i18n.T("st.ls.empty"))
				return nil
			}
			t := p.Table(i18n.T("wl.col.name"), i18n.T("st.col.size"), i18n.T("st.col.modified"))
			for _, o := range list {
				name := strings.TrimPrefix(o.Key, prefix)
				if o.Dir {
					t.Row(p.Bold(name), "—", "")
					continue
				}
				mod := ""
				if o.LastModified != nil {
					mod = o.LastModified.Local().Format("2006-01-02 15:04")
				}
				t.Row(name, humanBytes(o.Size), mod)
			}
			return t.Flush()
		},
	}
}

func newStorageCp() *cobra.Command {
	return &cobra.Command{
		Use: "cp " + i18n.T("arg.source") + " " + i18n.T("arg.destination"), Short: i18n.T("st.cp.short"), Long: i18n.T("st.cp.long"),
		Example: "betorun storage cp ./logo.png uploads:images/\nbetorun storage cp uploads:images/logo.png .\nbetorun storage cp uploads:export.csv -",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			srcB, srcK, srcRemote := remoteRef(args[0])
			dstB, dstK, dstRemote := remoteRef(args[1])
			switch {
			case srcRemote && !dstRemote:
				return download(ctx, cmd, cl, project, srcB, srcK, args[1])
			case !srcRemote && dstRemote:
				return upload(ctx, cmd, cl, project, args[0], dstB, dstK)
			}
			return errors.New(i18n.T("st.cp.usage"))
		},
	}
}

func upload(ctx context.Context, cmd *cobra.Command, cl *api.Client, project, local, ref, key string) error {
	st, err := os.Stat(local)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New(i18n.T("st.cp.dir"))
	}
	if st.Size() > storagedomain.MaxConsoleUpload {
		return errors.New(i18n.T("st.cp.too_big", humanBytes(storagedomain.MaxConsoleUpload)))
	}
	if key == "" || strings.HasSuffix(key, "/") {
		key += filepath.Base(local)
	}
	b, err := bucketByName(ctx, cl, project, ref)
	if err != nil {
		return err
	}
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	var out storagedomain.Object
	if err := cl.Upload(ctx, objectsPath(project, b.ID)+"?key="+url.QueryEscape(key), storagedomain.ObjectFormField, filepath.Base(local), f, &out); err != nil {
		return err
	}
	if done, err := wantJSON(cmd, out); done {
		return err
	}
	printer(cmd).Success(i18n.T("st.cp.uploaded", b.Name+":"+key, humanBytes(out.Size)))
	return nil
}

func download(ctx context.Context, cmd *cobra.Command, cl *api.Client, project, ref, key, local string) error {
	if key == "" || strings.HasSuffix(key, "/") {
		return errors.New(i18n.T("st.cp.dir"))
	}
	b, err := bucketByName(ctx, cl, project, ref)
	if err != nil {
		return err
	}
	src := objectsPath(project, b.ID) + "/download?key=" + url.QueryEscape(key)
	if local == "-" {
		return cl.Stream(ctx, src, cmd.OutOrStdout())
	}
	if st, err := os.Stat(local); err == nil && st.IsDir() {
		local = filepath.Join(local, path.Base(key))
	}
	tmp := local + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	cw := &countWriter{w: f}
	err = cl.Stream(ctx, src, cw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, local); err != nil {
		return err
	}
	printer(cmd).Success(i18n.T("st.cp.downloaded", local, humanBytes(cw.n)))
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func newStorageRm() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "rm " + i18n.T("arg.bucket") + ":" + i18n.T("arg.path") + "...", Short: i18n.T("st.rm.short"), Long: i18n.T("st.rm.long"),
		Example: "betorun storage rm uploads:old.png\nbetorun storage rm uploads:tmp/ --yes", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ref, keys := "", make([]string, 0, len(args))
			for _, a := range args {
				b, k, ok := remoteRef(a)
				if !ok || k == "" || (ref != "" && b != ref) {
					return errors.New(i18n.T("st.rm.usage"))
				}
				ref = b
				keys = append(keys, k)
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, ref)
			if err != nil {
				return err
			}
			if !yes && !confirm(cmd, i18n.T("st.rm.confirm", len(keys), b.Name, b.Name), b.Name) {
				return errors.New(i18n.T("st.aborted"))
			}
			var out storagedomain.DeleteObjectsResponse
			if err := cl.Do(ctx, http.MethodPost, objectsPath(project, b.ID)+"/delete", map[string]any{"keys": keys}, &out); err != nil {
				return err
			}
			if done, err := wantJSON(cmd, out); done {
				return err
			}
			printer(cmd).Success(i18n.T("st.rm.ok", out.Deleted))
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	return c
}

func newStorageLink() *cobra.Command {
	var hours int
	c := &cobra.Command{
		Use: "link " + i18n.T("arg.bucket") + ":" + i18n.T("arg.path"), Short: i18n.T("st.link.short"), Long: i18n.T("st.link.long"),
		Example: "betorun storage link uploads:rapport.pdf\nbetorun storage link uploads:rapport.pdf --hours 168", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, key, ok := remoteRef(args[0])
			if !ok || key == "" {
				return errors.New(i18n.T("st.rm.usage"))
			}
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			b, err := bucketByName(ctx, cl, project, ref)
			if err != nil {
				return err
			}
			var out storagedomain.ShareLink
			if err := cl.Do(ctx, http.MethodPost, objectsPath(project, b.ID)+"/link", map[string]any{"key": key, "expires_hours": hours}, &out); err != nil {
				return err
			}
			if done, err := wantJSON(cmd, out); done {
				return err
			}
			p := printer(cmd)
			fmt.Fprintln(cmd.OutOrStdout(), out.URL)
			p.Hint(i18n.T("st.link.until", out.ExpiresAt.Local().Format(time.DateTime)))
			return nil
		},
	}
	c.Flags().IntVar(&hours, "hours", storagedomain.DefaultLinkHours, i18n.T("st.flag.hours"))
	return c
}

func newStoragePublic() *cobra.Command {
	return &cobra.Command{
		Use: "public " + i18n.T("arg.bucket") + " on|off", Short: i18n.T("st.public.short"), Long: i18n.T("st.public.long"),
		Example: "betorun storage public assets on\nbetorun storage public assets off", Args: cobra.ExactArgs(2),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 1 {
				return []string{"on", "off"}, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var public bool
			switch args[1] {
			case "on":
				public = true
			case "off":
			default:
				return errors.New(i18n.T("st.public.usage"))
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
			if err := cl.Do(ctx, http.MethodPut, storagePath(project)+"/"+url.PathEscape(b.ID)+"/public", map[string]bool{"public": public}, &out); err != nil {
				return err
			}
			if done, err := wantJSON(cmd, out); done {
				return err
			}
			p := printer(cmd)
			if out.Public {
				p.Warn(i18n.T("st.public.on", out.Name))
				p.Fields([2]string{i18n.T("st.col.address"), out.PublicURL + "<" + i18n.T("arg.path") + ">"})
				return nil
			}
			p.Success(i18n.T("st.public.off", out.Name))
			return nil
		},
	}
}

// keyRef : clé d'un bucket par nom, ID ou identifiant public.
func keyRef(b *storagedomain.BucketView, ref string) (*storagedomain.KeyView, error) {
	for i := range b.Keys {
		if k := b.Keys[i]; k.Name == ref || k.ID == ref || k.AccessKeyID == ref {
			return &b.Keys[i], nil
		}
	}
	return nil, errors.New(i18n.T("st.keys.not_found", ref, b.Name))
}

func newStorageKeysRotate() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "rotate " + i18n.T("arg.bucket") + " " + i18n.T("arg.key"), Short: i18n.T("st.keys.rotate.short"), Long: i18n.T("st.keys.rotate.long"),
		Example: "betorun storage keys rotate uploads ci\nbetorun storage keys rotate uploads web", Args: cobra.ExactArgs(2),
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
			key, err := keyRef(b, args[1])
			if err != nil {
				return err
			}
			if key.Kind == storagedomain.KindManual && !yes && !confirm(cmd, i18n.T("st.keys.rotate.confirm", key.Name), key.Name) {
				return errors.New(i18n.T("st.aborted"))
			}
			var out storagedomain.CreatedKey
			if err := cl.Do(ctx, http.MethodPost, storagePath(project)+"/"+url.PathEscape(b.ID)+"/keys/"+url.PathEscape(key.ID)+"/rotate", nil, &out); err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			p := printer(cmd)
			p.Success(i18n.T("st.keys.rotate.ok", key.Name))
			if out.SecretAccessKey == "" {
				p.Hint(i18n.T("st.keys.rotate.container", key.WorkloadName))
				return nil
			}
			p.Fields([2]string{"Access key ID", out.Key.AccessKeyID}, [2]string{"Secret access key", out.SecretAccessKey})
			p.Hint(i18n.T("st.keys.create.once"))
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	return c
}
