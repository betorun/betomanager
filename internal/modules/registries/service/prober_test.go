package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
)

// fakeRegistry : registre avec défi Bearer (comme ghcr.io et Docker Hub) ou
// Basic. Seul moi / bon est accepté.
func fakeRegistry(t *testing.T, scheme string) (*httptest.Server, *Prober) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		good := ok && user == "moi" && pw == "bon"
		switch {
		case r.URL.Path == "/v2/" && scheme == "basic":
			if !good {
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)
			}
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token" && r.URL.Query().Get("service") == "reg":
			if !good {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"token":"t"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &Prober{Client: srv.Client()}
}

func TestProber_Bearer(t *testing.T) {
	srv, p := fakeRegistry(t, "bearer")
	host := strings.TrimPrefix(srv.URL, "https://")
	assert.Equal(t, domain.TestOK, p.Check(context.Background(), domain.Auth{Host: host, Username: "moi", Password: "bon"}))
	assert.Equal(t, domain.TestDenied, p.Check(context.Background(), domain.Auth{Host: host, Username: "moi", Password: "faux"}))
}

func TestProber_Basic(t *testing.T) {
	srv, p := fakeRegistry(t, "basic")
	host := strings.TrimPrefix(srv.URL, "https://")
	assert.Equal(t, domain.TestOK, p.Check(context.Background(), domain.Auth{Host: host, Username: "moi", Password: "bon"}))
	assert.Equal(t, domain.TestDenied, p.Check(context.Background(), domain.Auth{Host: host, Username: "moi", Password: "faux"}))
}

// Le client de production refuse les adresses internes : un client ne peut pas
// faire sonder le réseau de la plateforme par l'API.
func TestProber_RefusesPrivateAddresses(t *testing.T) {
	srv, _ := fakeRegistry(t, "bearer")
	host := strings.TrimPrefix(srv.URL, "https://")
	assert.Equal(t, domain.TestUnreachable, NewProber().Check(context.Background(), domain.Auth{Host: host, Username: "moi", Password: "bon"}))
	for _, addr := range []string{"127.0.0.1:443", "10.0.0.1:443", "169.254.169.254:80", "100.64.1.1:443", "[::1]:443"} {
		assert.ErrorIs(t, refusePrivate("tcp", addr, nil), errPrivateAddress, addr)
	}
	assert.NoError(t, refusePrivate("tcp", "140.82.112.34:443", nil))
}

func TestParseChallenge(t *testing.T) {
	scheme, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:x:pull"`)
	assert.Equal(t, "bearer", scheme)
	assert.Equal(t, "https://auth.docker.io/token", p["realm"])
	assert.Equal(t, "registry.docker.io", p["service"])
	assert.Equal(t, "repository:x:pull", p["scope"])
	scheme, _ = parseChallenge(`Basic realm="r"`)
	assert.Equal(t, "basic", scheme)
}
