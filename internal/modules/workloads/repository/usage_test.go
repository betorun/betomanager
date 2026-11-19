package repository

import (
	"context"
	"github.com/google/uuid"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Le stockage utilisé est compté en Mi, comme les quotas du service.
func TestParseSizeToMi(t *testing.T) {
	r := &workloadRepository{}
	assert.EqualValues(t, 1024, r.parseSizeToMi("1Gi"))
	assert.EqualValues(t, 512, r.parseSizeToMi("512Mi"))
	assert.Zero(t, r.parseSizeToMi("n'importe quoi"))
}

// Quota : un conteneur en échec compte (ses pods peuvent tourner) ; seul un
// conteneur supprimé est exclu.
func TestGetTotalUsageByProject_CountsFailed(t *testing.T) {
	repo, d := recordingRepo(t)
	_, _, _, _ = repo.GetTotalUsageByProject(context.Background(), uuid.New())
	q, _ := d.lastQuery.Load().(string)
	if !strings.Contains(q, "status <>") || strings.Contains(q, "NOT IN") {
		t.Fatalf("requête du quota : %s", q)
	}
}
