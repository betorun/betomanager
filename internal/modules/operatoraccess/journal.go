package operatoraccess

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/core/opjournal"
)

// JournalImporter : reçoit le journal du serveur de gestion (signé par
// l'Opérateur) et le recopie dans l'audit de la plateforme. Ni session ni
// jeton : seule la signature Ed25519 compte, et la route ne fait qu'ajouter
// des lignes d'audit.
type JournalImporter struct {
	DB   *gorm.DB
	Keys map[string]ed25519.PublicKey
	Now  func() time.Time
}

var journalKind = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// errInvalidBatch : lot mal formé (400).
var errInvalidBatch = errors.New("invalid operator journal batch")

// Journal : POST /admin/operator/journal.
func (j *JournalImporter) Journal(c *gin.Context) {
	if j == nil || len(j.Keys) == 0 {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, opjournal.MaxBody+1))
	if err != nil || len(body) > opjournal.MaxBody {
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}
	keyID := c.GetHeader(opjournal.HeaderKey)
	if err := opjournal.Verify(j.Keys, keyID, c.GetHeader(opjournal.HeaderTimestamp),
		c.GetHeader(opjournal.HeaderSignature), body, j.now()); err != nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	var batch opjournal.Batch
	if err := json.Unmarshal(body, &batch); err != nil || validate(batch) != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	res, err := j.Import(c.Request.Context(), keyID, batch)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, res)
}

func validate(b opjournal.Batch) error {
	if len(b.Entries) == 0 || len(b.Entries) > opjournal.MaxEntries {
		return errInvalidBatch
	}
	for _, e := range b.Entries {
		if e.UID == uuid.Nil || e.At.IsZero() || !journalKind.MatchString(e.Kind) ||
			e.Actor == "" || utf8.RuneCountInString(e.Actor) > 200 {
			return errInvalidBatch
		}
		if raw, _ := json.Marshal(e.Detail); len(raw) > 4096 {
			return errInvalidBatch
		}
	}
	return nil
}

// Import : chaque entrée nouvelle devient une ligne d'audit, dans la même
// transaction que son UID (jamais deux fois, jamais à moitié).
func (j *JournalImporter) Import(ctx context.Context, keyID string, b opjournal.Batch) (opjournal.Result, error) {
	res := opjournal.Result{Accepted: len(b.Entries)}
	err := j.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		w := &audit.GormWriter{DB: tx}
		for _, e := range b.Entries {
			ins := tx.Exec(`INSERT INTO operator_journal_imports (uid, key_id, imported_at) VALUES (?, ?, ?) ON CONFLICT (uid) DO NOTHING`,
				e.UID, keyID, j.now())
			if ins.Error != nil {
				return ins.Error
			}
			if ins.RowsAffected == 0 {
				continue
			}
			entry := AuditEntry(e, keyID)
			if err := w.Write(ctx, &entry); err != nil {
				return err
			}
			if err := tx.Exec(`UPDATE operator_journal_imports SET audit_id = ? WHERE uid = ?`, entry.ID, e.UID).Error; err != nil {
				return err
			}
			res.Imported++
		}
		return nil
	})
	return res, err
}

// AuditEntry : une entrée du journal de l'Opérateur vue par l'audit.
//
//	acteur <uuid>            → user:<uuid> (chemin normal, jeton délégué)
//	acteur rescue:… ou cli   → operator:rescue:…, operator:cli
//
// L'heure est celle du serveur de gestion (l'action a eu lieu alors, même si
// l'API était en panne) ; imported_at dit quand elle est arrivée.
func AuditEntry(e opjournal.Entry, keyID string) audit.Entry {
	principal := "operator:" + e.Actor
	if id, err := uuid.Parse(e.Actor); err == nil {
		principal = "user:" + id.String()
	}
	resource := "platform/operator"
	if e.InvestigationID != nil {
		resource += "/investigations/" + e.InvestigationID.String()
	}
	decision := audit.DecisionAllow
	if strings.HasSuffix(e.Kind, ".failed") || strings.HasSuffix(e.Kind, ".locked") {
		decision = audit.DecisionDeny
	}
	meta := map[string]any{"source": "operator", "journal_uid": e.UID.String(), "operator_key": keyID,
		"imported_at": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range e.Detail {
		if _, taken := meta[k]; !taken {
			meta[k] = v
		}
	}
	return audit.Entry{At: e.At.UTC(), Principal: principal, AuthMethod: "operator_journal", Action: "operator." + e.Kind,
		Resource: resource, Decision: decision, Metadata: meta}
}

func (j *JournalImporter) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}
