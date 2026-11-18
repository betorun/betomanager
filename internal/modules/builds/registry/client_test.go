package registry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	cert, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	require.NoError(t, os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	i, err := NewIssuer("betorun", "betorun-registry", cert, keyFile)
	require.NoError(t, err)
	return i
}

// actions : actions accordées par le jeton Bearer de la requête.
func actions(t *testing.T, r *http.Request) []string {
	t.Helper()
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var claims tokenClaims
	_, _, err := jwt.NewParser().ParseUnverified(raw, &claims)
	require.NoError(t, err)
	require.Len(t, claims.Access, 1)
	return claims.Access[0].Actions
}

func TestClient_ImageSizeOfIndex(t *testing.T) {
	project := uuid.New()
	repo := project.String() + "/web"
	docs := map[string]any{
		"sha256:index": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]any{{"digest": "sha256:amd64"}, {"digest": "sha256:attest"}}},
		"sha256:amd64": map[string]any{"config": map[string]any{"digest": "sha256:cfg", "size": 100},
			"layers": []map[string]any{{"digest": "sha256:l1", "size": 1000}, {"digest": "sha256:l2", "size": 2000}}},
		"sha256:attest": map[string]any{"config": map[string]any{"digest": "sha256:cfg2", "size": 10},
			"layers": []map[string]any{{"digest": "sha256:l1", "size": 1000}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, []string{ActionPull}, actions(t, r))
		digest := strings.TrimPrefix(r.URL.Path, "/v2/"+repo+"/manifests/")
		doc, ok := docs[digest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Issuer: testIssuer(t)}

	size, err := c.ImageSize(context.Background(), project, repo, "sha256:index")

	require.NoError(t, err)
	assert.Equal(t, int64(100+1000+2000+10), size, "couche commune comptée une fois")
}

func TestClient_Delete(t *testing.T) {
	project := uuid.New()
	repo := project.String() + "/web"
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, []string{ActionDelete}, actions(t, r))
		got = append(got, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "gone") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", Issuer: testIssuer(t)}

	require.NoError(t, c.Delete(context.Background(), project, repo, "sha256:a"))
	require.NoError(t, c.Delete(context.Background(), project, repo, "sha256:gone"), "déjà absent")
	assert.Equal(t, []string{"/v2/" + repo + "/manifests/sha256:a", "/v2/" + repo + "/manifests/sha256:gone"}, got)
}

func TestRepository(t *testing.T) {
	p := uuid.New().String()
	assert.Equal(t, p+"/web", Repository("r.example:5000", "r.example:5000/"+p+"/web:b-1234"))
	assert.Equal(t, p+"/web", Repository("r.example:5000", "r.example:5000/"+p+"/web@sha256:abc"))
	assert.Equal(t, "", Repository("r.example:5000", "nginx:1"))
}
