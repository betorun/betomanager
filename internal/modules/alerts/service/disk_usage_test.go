package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/i18n"
)

func TestDiskUsage_WorkloadStopped(t *testing.T) {
	n, m := newNotifier()
	project, wl := uuid.New(), uuid.New()
	n.DiskUsage(context.Background(), project, wl, 120, "workload", "api", 1288490189, 1<<30)
	require.Len(t, m.sent, 2)
	msg := m.sent[0]
	assert.Equal(t, "« api » a été arrêté : disque plein", msg.Subject)
	assert.Contains(t, msg.Text, "1,2 Go sur 1,0 Go")
	assert.Contains(t, msg.Text, "« boutique »")
	assert.Contains(t, msg.Text, "https://console.example.com/workloads/"+wl.String())
	assert.NotContains(t, msg.Text+msg.HTML, "{{")
}

func TestDiskUsage_DatabaseEnglishAndThrottle(t *testing.T) {
	n, m := newNotifier()
	ctx := i18n.With(context.Background(), i18n.EN)
	project, db := uuid.New(), uuid.New()
	n.DiskUsage(ctx, project, db, 80, "database", "shop", 900<<20, 1<<30)
	n.DiskUsage(ctx, project, db, 80, "database", "shop", 950<<20, 1<<30)
	require.Len(t, m.sent, 2, "une alerte par seuil et par période")
	assert.Equal(t, `The database "shop" is almost full`, m.sent[0].Subject)
	assert.Contains(t, m.sent[0].Text, "0.9 GB of 1.0 GB")
	assert.Contains(t, m.sent[0].Text, "https://console.example.com/services")
}

func TestDiskUsage_UnknownLevelOrKind(t *testing.T) {
	n, m := newNotifier()
	n.DiskUsage(context.Background(), uuid.New(), uuid.New(), 0, "workload", "api", 1, 2)
	n.DiskUsage(context.Background(), uuid.New(), uuid.New(), 80, "bucket", "x", 1, 2)
	assert.Empty(t, m.sent)
}

func TestDiskCopies_Complete(t *testing.T) {
	for level, byKind := range diskCopies {
		for kind, copies := range byKind {
			for _, lang := range []i18n.Locale{i18n.FR, i18n.EN} {
				c := copies[lang]
				require.NotEmpty(t, c.Subject, "%d %s %s", level, kind, lang)
				require.True(t, strings.Contains(c.Body, "{{.Usage}}"), "%d %s %s", level, kind, lang)
			}
		}
	}
}
