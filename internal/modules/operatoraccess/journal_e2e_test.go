package operatoraccess

// Vrai Postgres, ignoré sans IAM_E2E (bash scripts/iam_e2e_db.sh).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/core/opjournal"
)

func TestE2E_JournalImport(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open("postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	j := &JournalImporter{DB: db}
	inv := uuid.New()
	a, b := uuid.New(), uuid.New()
	at := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Microsecond)
	batch := opjournal.Batch{Entries: []opjournal.Entry{
		{UID: a, At: at, Actor: "rescue:awa@betorun.test", Kind: "rescue.login", Detail: map[string]any{"reason": "API en panne"}},
		{UID: b, At: at.Add(time.Minute), Actor: "rescue:awa@betorun.test", Kind: "investigation.opened", InvestigationID: &inv},
	}}
	res, err := j.Import(ctx, "k1", batch)
	if err != nil || res.Imported != 2 || res.Accepted != 2 {
		t.Fatal(res, err)
	}
	// Rejeu (coupure après l'envoi) : rien de neuf.
	if res, err := j.Import(ctx, "k1", batch); err != nil || res.Imported != 0 || res.Accepted != 2 {
		t.Fatal(res, err)
	}
	var rows []struct {
		Principal, Action, Resource, Decision string
		At                                    time.Time
		Reason                                string
	}
	if err := db.Raw(`SELECT a.principal, a.action, a.resource, a.decision, a.at, a.metadata->>'reason' AS reason
		FROM audit_log a JOIN operator_journal_imports i ON i.audit_id = a.id WHERE i.uid IN (?, ?) ORDER BY a.at`, a, b).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Principal != "operator:rescue:awa@betorun.test" || rows[0].Action != "operator.rescue.login" ||
		rows[0].Reason != "API en panne" || !rows[0].At.Equal(at) || rows[1].Resource != "platform/operator/investigations/"+inv.String() {
		t.Fatalf("%+v", rows)
	}
}
