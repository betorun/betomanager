package edge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTokens struct {
	token       atomic.Value
	invalidated atomic.Int32
}

func (f *fakeTokens) Token(context.Context) (string, error) { return f.token.Load().(string), nil }
func (f *fakeTokens) Invalidate() {
	f.invalidated.Add(1)
	f.token.Store("fresh")
}

func TestHTTPResolver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/kmanager/v1/clusters/EdgeService/k3s-1/Resolve", r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("host") {
		case "vitrine.sites.betorun.app":
			_, _ = w.Write([]byte(`{"site_id":"s1","deployment_id":"d1","spa_fallback":true}`))
		case "suspendu.example":
			_, _ = w.Write([]byte(`{"site_id":"s2","deployment_id":"d2","suspended":true}`))
		case "panne.example":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	tokens := &fakeTokens{}
	tokens.token.Store("expired")
	r := &HTTPResolver{BaseURL: srv.URL + "/kmanager/v1/", ClusterID: "k3s-1", Tokens: tokens}

	site, err := r.Resolve(context.Background(), "vitrine.sites.betorun.app")
	require.NoError(t, err)
	assert.Equal(t, &Site{ID: "s1", DeploymentID: "d1", SPAFallback: true}, site)
	assert.Equal(t, int32(1), tokens.invalidated.Load(), "401 : un nouveau jeton, une fois")

	site, err = r.Resolve(context.Background(), "inconnu.example")
	require.NoError(t, err)
	assert.Nil(t, site, "404 : aucun site, pas une erreur")

	site, err = r.Resolve(context.Background(), "suspendu.example")
	require.NoError(t, err)
	assert.True(t, site.Suspended, "organisation suspendue : page neutre")

	_, err = r.Resolve(context.Background(), "panne.example")
	assert.Error(t, err)
}

type flakyResolver struct {
	site *Site
	err  error
}

func (f *flakyResolver) Resolve(context.Context, string) (*Site, error) { return f.site, f.err }

func TestCachedResolver_StaleOnError(t *testing.T) {
	next := &flakyResolver{site: &Site{ID: "s1", DeploymentID: "d1"}}
	c := NewCachedResolver(next, 10*time.Second, 0)
	c.StaleFor = time.Minute
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	_, err := c.Resolve(context.Background(), "h")
	require.NoError(t, err)

	next.err = errors.New("API injoignable")
	now = now.Add(30 * time.Second) // expirée, mais dans la marge
	site, err := c.Resolve(context.Background(), "h")
	require.NoError(t, err)
	assert.Equal(t, "s1", site.ID, "panne passagère : dernière réponse connue")

	now = now.Add(2 * time.Minute) // au-delà de la marge
	_, err = c.Resolve(context.Background(), "h")
	assert.Error(t, err)
}
