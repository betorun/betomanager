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
)

// newDatabases : « databases » (bases Postgres gérées).
func newDatabases() *cobra.Command {
	c := &cobra.Command{Use: "databases", Aliases: []string{"db", "database"}, Short: i18n.T("db.short"),
		Example: "betorun databases list\n" +
			"# " + i18n.T("ex.db.import") + "\n" +
			"pg_dump -Fc -d \"$OLD_DATABASE_URL\" -f app.dump\n" +
			"betorun databases import main app.dump"}
	c.AddCommand(newDatabasesList(), newDatabasesImport(), newDatabasesPower(true), newDatabasesPower(false))
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
				if d.Status == databasesdomain.StatusReady {
					tone = ui.ToneOK
				} else if d.Status == databasesdomain.StatusFailed {
					tone = ui.ToneError
				}
				t.Row(p.Bold(d.Name), p.Status(tone, d.Status), d.StorageSize)
			}
			return t.Flush()
		},
	}
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
