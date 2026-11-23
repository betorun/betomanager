package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
	databasesdomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
)

const (
	importPollEvery = 3 * time.Second
	importTimeout   = time.Hour
	// Création : une base est prête en une minute environ.
	createPollEvery = 3 * time.Second
	createTimeout   = 10 * time.Minute
	// Branchement refusé pendant un déploiement : nouvel essai toutes les 10 s.
	attachRetryEvery     = 10 * time.Second
	codeDeployInProgress = "DEPLOY_IN_PROGRESS"
)

// newDatabases : « databases » (bases Postgres gérées).
func newDatabases() *cobra.Command {
	c := &cobra.Command{Use: "databases", Aliases: []string{"db", "database"}, Short: i18n.T("db.short"),
		Example: "betorun databases create main --size 5Gi\n" +
			"betorun databases attach main web  # " + i18n.T("ex.db.attach") + "\n" +
			"betorun databases list\n" +
			"# " + i18n.T("ex.db.import") + "\n" +
			"pg_dump -Fc -d \"$OLD_DATABASE_URL\" -f app.dump\n" +
			"betorun databases import main app.dump"}
	c.AddCommand(newDatabasesList(), newDatabasesCreate(), newDatabasesAttach(), newDatabasesDelete(), newDatabasesImport(), newDatabasesPower(true), newDatabasesPower(false))
	return c
}

func newDatabasesList() *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: i18n.T("db.list.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			list, err := listDatabases(cmd.Context(), cl, project)
			if err != nil {
				return err
			}
			if done, err := wantJSON(cmd, list); done {
				return err
			}
			p := printer(cmd)
			if len(list) == 0 {
				p.Hint(i18n.T("db.list.empty"))
				return nil
			}
			t := p.Table(i18n.T("wl.col.name"), i18n.T("wl.col.status"), i18n.T("db.col.size"))
			for _, d := range list {
				tone := ui.ToneProgress
				switch d.Status {
				case databasesdomain.StatusReady:
					tone = ui.ToneOK
				case databasesdomain.StatusFailed:
					tone = ui.ToneError
				}
				t.Row(p.Bold(d.Name), p.Status(tone, d.Status), d.StorageSize)
			}
			return t.Flush()
		},
	}
}

// newDatabasesCreate : « create <nom> » ; attend que la base soit prête, sauf --no-wait.
func newDatabasesCreate() *cobra.Command {
	var size string
	var noWait bool
	c := &cobra.Command{
		Use: "create " + i18n.T("arg.name"), Short: i18n.T("db.create.short"), Long: i18n.T("db.create.long"),
		Example: "betorun databases create main\n" +
			"betorun databases create main --size 5Gi",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx, p := cmd.Context(), printer(cmd)
			body := map[string]string{"name": args[0]}
			if size != "" {
				body["storage_size"] = size
			}
			var db cliDatabase
			if err := cl.Do(ctx, http.MethodPost, databasesPath(project), body, &db); err != nil {
				return err
			}
			if noWait {
				if ok, err := wantJSON(cmd, db); ok {
					return err
				}
				p.Success(i18n.T("db.create.started", db.Name))
				return nil
			}
			if !flagJSON {
				p.Hint(i18n.T("db.create.waiting", db.Name))
			}
			ready, err := waitDatabase(cmd, cl, project, db.ID)
			if err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, ready); ok {
				return err
			}
			p.Success(i18n.T("db.create.ok", ready.Name, ready.StorageSize))
			p.Hint(i18n.T("db.create.next", ready.Name))
			return nil
		},
	}
	c.Flags().StringVar(&size, "size", "", i18n.T("flag.db.size"))
	c.Flags().BoolVar(&noWait, "no-wait", false, i18n.T("flag.db.no_wait"))
	return c
}

// waitDatabase : relit la base jusqu'à READY (FAILED : erreur).
func waitDatabase(cmd *cobra.Command, cl *api.Client, project, id string) (*cliDatabase, error) {
	ctx := cmd.Context()
	deadline := time.Now().Add(createTimeout)
	for {
		var db cliDatabase
		if err := cl.Do(ctx, http.MethodGet, databasesPath(project)+"/"+url.PathEscape(id), nil, &db); err != nil {
			return nil, err
		}
		switch db.Status {
		case databasesdomain.StatusReady:
			return &db, nil
		case databasesdomain.StatusFailed:
			return nil, errors.New(i18n.T("db.create.failed", db.Name))
		}
		if time.Now().After(deadline) {
			return nil, errors.New(i18n.T("db.create.timeout", db.Name))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(createPollEvery):
		}
	}
}

// newDatabasesAttach : « attach <base> <conteneur> » ; l'URI de connexion
// devient une variable secrète du conteneur, qui est redéployé.
func newDatabasesAttach() *cobra.Command {
	var variable string
	c := &cobra.Command{
		Use:   "attach " + i18n.T("arg.database") + " " + i18n.T("arg.container"),
		Short: i18n.T("db.attach.short"), Long: i18n.T("db.attach.long"),
		Example: "betorun databases attach main web\n" +
			"betorun databases attach main worker --variable PG_URL",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := databaseByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			if db.Status != databasesdomain.StatusReady {
				return errors.New(i18n.T("db.attach.not_ready", db.Name, db.Status))
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
			if variable != "" {
				body["variable"] = variable
			}
			p := printer(cmd)
			var out databasesdomain.AttachResponse
			path := databasesPath(project) + "/" + url.PathEscape(db.ID) + "/attach"
			// Un déploiement en cours (souvent le premier, qui échoue faute de
			// base) bloque le branchement : on attend qu'il se termine.
			deadline, warned := time.Now().Add(createTimeout), false
			for {
				err := cl.Do(ctx, http.MethodPost, path, body, &out)
				var apiErr *api.Error
				if err == nil || !errors.As(err, &apiErr) || apiErr.Code != codeDeployInProgress {
					if err != nil {
						return err
					}
					break
				}
				if time.Now().After(deadline) {
					return errors.New(i18n.T("db.attach.busy_timeout", args[1]))
				}
				if !warned && !flagJSON {
					p.Hint(i18n.T("db.attach.busy", args[1]))
					warned = true
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(attachRetryEvery):
				}
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			p.Success(i18n.T("db.attach.ok", db.Name, args[1], out.Variable))
			if out.Redeploy {
				p.Hint(i18n.T("db.attach.redeploy", args[1], args[1]))
			}
			return nil
		},
	}
	c.Flags().StringVar(&variable, "variable", "", i18n.T("flag.db.variable"))
	return c
}

// newDatabasesDelete : « delete <base> » ; le nom est à retaper, sauf --yes.
func newDatabasesDelete() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use: "delete " + i18n.T("arg.database"), Short: i18n.T("db.delete.short"), Long: i18n.T("db.delete.long"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := databaseByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			if !yes && !confirm(cmd, i18n.T("db.delete.confirm", db.Name), db.Name) {
				return errors.New(i18n.T("db.delete.aborted"))
			}
			if err := cl.Do(ctx, http.MethodDelete, databasesPath(project)+"/"+url.PathEscape(db.ID), nil, nil); err != nil {
				return err
			}
			printer(cmd).Success(i18n.T("db.delete.ok", db.Name))
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, i18n.T("flag.yes"))
	return c
}

// newDatabasesPower : « stop » ou « start » (hibernation, disque gardé).
func newDatabasesPower(stop bool) *cobra.Command {
	use, short, done, action := "start", "db.start.short", "db.start.done", "/start"
	if stop {
		use, short, done, action = "stop", "db.stop.short", "db.stop.done", "/stop"
	}
	return &cobra.Command{
		Use: use + " " + i18n.T("arg.database"), Short: i18n.T(short), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := databaseByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			var out cliDatabase
			if err := cl.Do(ctx, http.MethodPost, databasesPath(project)+"/"+url.PathEscape(db.ID)+action, map[string]any{}, &out); err != nil {
				return err
			}
			if ok, err := wantJSON(cmd, out); ok {
				return err
			}
			printer(cmd).Success(i18n.T(done, db.Name))
			return nil
		},
	}
}

func newDatabasesImport() *cobra.Command {
	var clean, noWait bool
	c := &cobra.Command{
		Use:   "import " + i18n.T("arg.database") + " " + i18n.T("arg.dump"),
		Short: i18n.T("db.import.short"),
		Long:  i18n.T("db.import.long"),
		Example: "pg_dump -Fc -d \"$OLD_DATABASE_URL\" -f app.dump\n" +
			"betorun databases import main app.dump\n" +
			"betorun databases import main app.dump --clean  # " + i18n.T("ex.db.clean"),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, project, _, err := projectClient()
			if err != nil {
				return err
			}
			ctx, p := cmd.Context(), printer(cmd)
			db, err := databaseByName(ctx, cl, project, args[0])
			if err != nil {
				return err
			}
			f, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			head := make([]byte, 5)
			if n, _ := f.Read(head); !databasesdomain.IsCustomDump(head[:n]) {
				return errors.New(i18n.T("db.import.not_dump"))
			}
			if _, err := f.Seek(0, 0); err != nil {
				return err
			}
			fields := map[string]string{}
			if clean {
				fields["clean"] = "true"
			}
			p.Title(i18n.T("db.import.sending", filepath.Base(args[1]), db.Name), "")
			var imp databasesdomain.DatabaseImport
			path := databasesPath(project) + "/" + url.PathEscape(db.ID) + "/import"
			if err := cl.UploadWithFields(ctx, path, fields, databasesdomain.DumpFormField, filepath.Base(args[1]), f, &imp); err != nil {
				return err
			}
			if noWait {
				p.Success(i18n.T("db.import.started", imp.ID))
				return nil
			}
			return waitImport(cmd, cl, project, db, imp.ID.String())
		},
	}
	c.Flags().BoolVar(&clean, "clean", false, i18n.T("flag.db.clean"))
	c.Flags().BoolVar(&noWait, "no-wait", false, i18n.T("flag.no_wait"))
	return c
}

func waitImport(cmd *cobra.Command, cl *api.Client, project string, db *cliDatabase, id string) error {
	ctx, p := cmd.Context(), printer(cmd)
	p.Hint(i18n.T("db.import.waiting"))
	deadline := time.Now().Add(importTimeout)
	for {
		var imp databasesdomain.DatabaseImport
		if err := cl.Do(ctx, http.MethodGet, databasesPath(project)+"/"+url.PathEscape(db.ID)+"/imports/"+url.PathEscape(id), nil, &imp); err != nil {
			return err
		}
		switch imp.Status {
		case databasesdomain.ImportSucceeded:
			p.Success(i18n.T("db.import.ok", db.Name))
			return nil
		case databasesdomain.ImportFailed:
			if imp.Failure != "" {
				p.Println(imp.Failure)
			}
			return errors.New(i18n.T("db.import.failed", db.Name))
		}
		if time.Now().After(deadline) {
			return errors.New(i18n.T("db.import.timeout"))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(importPollEvery):
		}
	}
}

// cliDatabase : ce que la CLI lit d'une base.
type cliDatabase struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	StorageSize string `json:"storage_size"`
}

func databasesPath(project string) string {
	return "/projects/" + url.PathEscape(project) + "/databases"
}

func listDatabases(ctx context.Context, cl *api.Client, project string) ([]cliDatabase, error) {
	var out []cliDatabase
	return out, cl.Do(ctx, http.MethodGet, databasesPath(project), nil, &out)
}

func databaseByName(ctx context.Context, cl *api.Client, project, ref string) (*cliDatabase, error) {
	list, err := listDatabases(ctx, cl, project)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref || list[i].ID == ref {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("%s", i18n.T("db.not_found", ref))
}
