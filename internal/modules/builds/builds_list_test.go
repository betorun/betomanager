package builds

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/builds/domain"
)

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 19, 21, 5, 3, 123456789, time.UTC)
	id := uuid.New()
	gotAt, gotID, err := decodeCursor(encodeCursor(at, id))
	require.NoError(t, err)
	assert.True(t, gotAt.Equal(at))
	assert.Equal(t, id, gotID)

	for _, bad := range []string{"x", "12_pas-un-uuid", "abc_" + id.String()} {
		_, _, err := decodeCursor(bad)
		assert.Error(t, err, bad)
	}
}

func TestBuildFilter(t *testing.T) {
	f, err := buildFilter(&domain.ListBuildsRequest{Status: "active", Image: "available", Query: "  api ", Limit: 500})
	require.NoError(t, err)
	assert.Equal(t, domain.FilterActive, f.Status)
	assert.Equal(t, "api", f.Query)
	assert.Equal(t, domain.DefaultListLimit, f.Limit, "au-delà du maximum : valeur par défaut")

	for _, in := range []domain.ListBuildsRequest{{Status: "DONE"}, {Image: "gone"}, {Before: "abc"}} {
		_, err := buildFilter(&in)
		assert.ErrorIs(t, err, domain.ErrInvalidFilter)
	}
}
