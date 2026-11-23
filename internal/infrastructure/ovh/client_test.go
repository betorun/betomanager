package ovh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RequiresCredentials(t *testing.T) {
	_, err := New(Credentials{ApplicationKey: "a"})
	assert.ErrorIs(t, err, ErrIncompleteCredentials)
}

// Exemple de la documentation OVH, recalculé :
//
//	printf 'as+ck+GET+https://eu.api.ovh.com/1.0/me++1700000000' | sha1sum
func TestSign(t *testing.T) {
	c, err := New(Credentials{ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck"})
	require.NoError(t, err)
	assert.Equal(t, "$1$212ae9d571c1dbc085f3630b6c2b68d5b3fdc482", c.Sign(http.MethodGet, "https://eu.api.ovh.com/1.0/me", "", 1700000000))
}

func TestCall_HeadersAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "ak", r.Header.Get("X-Ovh-Application"))
		assert.Equal(t, "1700000000", r.Header.Get("X-Ovh-Timestamp"))
		assert.NotEmpty(t, r.Header.Get("X-Ovh-Signature"))
		if r.URL.Path == "/vps/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"The requested object (serviceName = missing) does not exist"}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"vps-123"}`))
	}))
	defer srv.Close()
	c, err := New(Credentials{Endpoint: srv.URL + "/", ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck"})
	require.NoError(t, err)
	c.Now = func() time.Time { return time.Unix(1700000000, 0) }
	var out struct{ Name string }
	require.NoError(t, c.Call(context.Background(), http.MethodGet, "/vps/vps-123", nil, &out))
	assert.Equal(t, "vps-123", out.Name)
	err = c.Call(context.Background(), http.MethodGet, "/vps/missing", nil, nil)
	assert.True(t, IsNotFound(err))
}

// Une lecture refusée pour débit est reprise ; une écriture jamais.
func TestCall_RetriesReadsOnly(t *testing.T) {
	var gets, posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		gets++
		if gets == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"name":"vps-1"}`))
	}))
	defer srv.Close()
	c, err := New(Credentials{Endpoint: srv.URL, ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck"})
	require.NoError(t, err)
	var out struct{ Name string }
	require.NoError(t, c.Call(context.Background(), http.MethodGet, "/vps/vps-1", nil, &out))
	assert.Equal(t, 2, gets)
	assert.Equal(t, "vps-1", out.Name)
	require.Error(t, c.Call(context.Background(), http.MethodPost, "/order", map[string]string{}, nil))
	assert.Equal(t, 1, posts)
	assert.Equal(t, maxRetryWait, retryAfter("3600"))
}
