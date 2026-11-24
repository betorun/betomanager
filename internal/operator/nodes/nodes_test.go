package nodes

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/operator/action"
)

type pair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func issue(t *testing.T, cn string, parent *pair, client bool) *pair {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	signer, signKey := tpl, key
	if parent == nil {
		tpl.IsCA, tpl.BasicConstraintsValid, tpl.KeyUsage = true, true, x509.KeyUsageCertSign
	} else {
		signer, signKey = parent.cert, parent.key
		tpl.KeyUsage = x509.KeyUsageDigitalSignature
		if client {
			tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &pair{cert: c, key: key, der: der}
}

func (p *pair) write(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	cf, kf := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	kb, _ := x509.MarshalECPrivateKey(p.key)
	_ = os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.der}), 0o600)
	_ = os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cf, kf
}

// fakeAgent : répond comme node-agent, en exigeant un certificat client
// signé par l'autorité de l'Opérateur ; vérifie les ordres.
func fakeAgent(t *testing.T, operatorCA *pair, nodeCA *pair, orderPub ed25519.PublicKey, got *[]map[string]any) *httptest.Server {
	t.Helper()
	srvPair := issue(t, "server-1", nodeCA, false)
	pool := x509.NewCertPool()
	pool.AddCert(operatorCA.cert)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/status":
			io.WriteString(w, `{"node":"server-1","status":{"load":[3.9,2.1,1.0],"cpus":4,"memory_total_bytes":8589934592,"memory_available_bytes":429496729,
				"swap_total_bytes":0,"swap_free_bytes":0,"uptime_seconds":90000,
				"mounts":[{"path":"/","total_bytes":100000000000,"free_bytes":2000000000,"used_percent":98.0,"inodes_used_percent":12.5}]},
				"services":[{"name":"k3s","kind":"systemd","state":"failed","detail":{"NRestarts":"7","SubState":"failed"},"restartable":true},
				{"name":"api","kind":"docker","state":"running","detail":{"Health":"healthy"},"restartable":false}]}`)
		case strings.HasPrefix(r.URL.Path, "/v1/services/k3s/logs"):
			if r.URL.Query().Get("lines") != "300" {
				t.Errorf("lignes : %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"service":"k3s","logs":"E1124 etcd: mvcc: database space exceeded\n"}`)
		case r.URL.Path == "/v1/services/k3s/restart":
			var env struct{ Payload, Signature string }
			_ = json.NewDecoder(r.Body).Decode(&env)
			payload, _ := base64.StdEncoding.DecodeString(env.Payload)
			sig, _ := base64.StdEncoding.DecodeString(env.Signature)
			if !ed25519.Verify(orderPub, append([]byte("betorun-node-order-v1\n"), payload...), sig) {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"code":"ORDER_REFUSED","message":"ordre non signé par une clé connue"}`)
				return
			}
			var o map[string]any
			_ = json.Unmarshal(payload, &o)
			*got = append(*got, o)
			io.WriteString(w, `{"service":"k3s","restarted":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"code":"UNKNOWN_SERVICE","message":"service hors de la liste de ce nœud"}`)
		}
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvPair.der}, PrivateKey: srvPair.key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestNodesMTLSToolsAndOrders(t *testing.T) {
	dir := t.TempDir()
	operatorCA, nodeCA := issue(t, "operator CA", nil, false), issue(t, "nodes CA", nil, false)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var orders []map[string]any
	srv := fakeAgent(t, operatorCA, nodeCA, pub, &orders)

	cert, key := issue(t, "operator", operatorCA, true).write(t, dir, "operator")
	caFile := filepath.Join(dir, "nodes-ca.crt")
	_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: nodeCA.der}), 0o600)
	n, err := Parse("server-1="+srv.URL+", server-2=https://127.0.0.1:1", TLSFiles{Cert: cert, Key: key, CA: caFile}, priv)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_900_000_000, 0)
	n.SetClock(func() time.Time { return now })
	ctx := context.Background()

	out := n.Run(ctx, "node_list", nil)
	if out.IsError || !strings.Contains(out.Content, "server-1\t3.90/4\t409.6 Mio (5 %)\t/ 98.0 %\tk3s=failed") ||
		!strings.Contains(out.Content, "server-2\tinjoignable") {
		t.Fatalf("node_list :\n%s", out.Content)
	}
	out = n.Run(ctx, "node_status", json.RawMessage(`{"node":"server-1"}`))
	if !strings.Contains(out.Content, "k3s\tsystemd\tfailed\ttrue\tNRestarts=7 SubState=failed") || !strings.Contains(out.Content, "/\t98.0 %\t1.9 Gio\t12.5 %") {
		t.Fatalf("node_status :\n%s", out.Content)
	}
	out = n.Run(ctx, "node_service_logs", json.RawMessage(`{"node":"server-1","service":"k3s","lines":5000}`))
	if out.IsError || !strings.Contains(out.Content, "database space exceeded") {
		t.Fatalf("logs : %+v", out)
	}
	out = n.Run(ctx, "node_service_logs", json.RawMessage(`{"node":"server-1","service":"sshd"}`))
	if !out.IsError || !strings.Contains(out.Content, "UNKNOWN_SERVICE") {
		t.Fatalf("service inconnu : %+v", out)
	}

	// Ordre signé : validation humaine, nœud, service, durée de vie.
	if err := n.Restart(ctx, "server-1", "k3s", Approval{ApprovedBy: "awa@betorun.test", ApprovalID: "card-9", InvestigationID: "inv-1"}); err != nil {
		t.Fatal(err)
	}
	o := orders[0]
	if o["action"] != "service.restart" || o["node"] != "server-1" || o["service"] != "k3s" || o["approved_by"] != "awa@betorun.test" ||
		o["approval_id"] != "card-9" || o["expires_at"].(float64)-o["issued_at"].(float64) != 120 || len(o["nonce"].(string)) != 32 {
		t.Fatalf("ordre : %v", o)
	}
	if err := n.Restart(ctx, "server-1", "k3s", Approval{}); err == nil {
		t.Fatal("ordre sans validation accepté")
	}
	if err := New(map[string]string{"server-1": srv.URL}, nil, nil).Restart(ctx, "server-1", "k3s", Approval{ApprovedBy: "a", ApprovalID: "b"}); err != ErrNoOrderKey {
		t.Fatal(err)
	}

	// Sans certificat client : la poignée de main TLS échoue.
	pool := x509.NewCertPool()
	pool.AddCert(nodeCA.cert)
	bare := New(map[string]string{"server-1": srv.URL}, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, nil)
	if out := bare.Run(ctx, "node_status", json.RawMessage(`{"node":"server-1"}`)); !out.IsError || !strings.Contains(out.Content, "injoignable") {
		t.Fatalf("sans certificat : %+v", out)
	}
}

func TestParse(t *testing.T) {
	if n, err := Parse("", TLSFiles{}, nil); err != nil || len(n.Names()) != 0 || n.Definitions() != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"server-1=http://10.0.0.2:9443", "S1=https://x", "a=https://x,a=https://y", "a=https://u:p@x"} {
		if _, err := Parse(bad, TLSFiles{Cert: "c", Key: "k", CA: "ca"}, nil); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
	if _, err := Parse("a=https://10.0.0.2:9443", TLSFiles{}, nil); err == nil {
		t.Error("nœuds sans certificats acceptés")
	}
}

func TestNodeActions(t *testing.T) {
	dir := t.TempDir()
	operatorCA, nodeCA := issue(t, "operator CA", nil, false), issue(t, "nodes CA", nil, false)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var orders []map[string]any
	srv := fakeAgent(t, operatorCA, nodeCA, pub, &orders)
	cert, key := issue(t, "operator", operatorCA, true).write(t, dir, "operator")
	caFile := filepath.Join(dir, "nodes-ca.crt")
	_ = os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: nodeCA.der}), 0o600)
	n, err := Parse("server-1="+srv.URL, TLSFiles{Cert: cert, Key: key, CA: caFile}, priv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if len(n.Actions()) != 1 || len(New(map[string]string{"server-1": srv.URL}, nil, nil).Actions()) != 0 {
		t.Fatal("actions : une avec clé d'ordres, aucune sans")
	}

	p, err := n.Prepare(ctx, kindServiceRestart, json.RawMessage(`{"node":"server-1","service":"k3s"}`))
	if err != nil || p.Risk != "high" || p.Confirm != "server-1" || p.Target != "server-1 / k3s" || !strings.Contains(p.State, "failed NRestarts=7") {
		t.Fatalf("k3s : %+v %v", p, err)
	}
	for _, bad := range []string{`{"node":"server-1","service":"api"}`, `{"node":"server-1","service":"sshd"}`, `{"node":"server-9","service":"k3s"}`} {
		if _, err := n.Prepare(ctx, kindServiceRestart, json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepté", bad)
		}
	}
	out, err := n.Execute(ctx, kindServiceRestart, p.Params, action.Approval{CardID: "c-1", InvestigationID: "inv-1", ApprovedBy: "awa@betorun.test"})
	if err != nil || !strings.Contains(out, "État relu : failed") {
		t.Fatalf("exécution : %q %v", out, err)
	}
	if len(orders) != 1 || orders[0]["approval_id"] != "c-1" || orders[0]["approved_by"] != "awa@betorun.test" {
		t.Fatalf("ordre : %v", orders)
	}
}

// Un nœud qui ne répond plus ne fait pas passer les autres pour injoignables.
func TestNodeListOneHangs(t *testing.T) {
	old := targetTimeout
	targetTimeout = 200 * time.Millisecond
	t.Cleanup(func() { targetTimeout = old })
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"node":"b","status":{"load":[0.1,0,0],"cpus":2,"memory_total_bytes":100,"memory_available_bytes":50},"services":[]}`)
	}))
	defer ok.Close()
	n := New(map[string]string{"a": hang.URL, "b": ok.URL}, nil, nil)
	start := time.Now()
	out := n.Run(context.Background(), "node_list", nil)
	if !strings.Contains(out.Content, "a\tinjoignable") || !strings.Contains(out.Content, "b\t0.10/2") {
		t.Fatalf("%s", out.Content)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("délais additionnés")
	}
}
