package certread

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/thekrauss/kubemanager/internal/core/certstatus"
)

func cert(name string, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": name, "namespace": "km-p"},
	}}
	if status != nil {
		u.Object["status"] = status
	}
	return u
}

func cond(kind, status, msg string) map[string]any {
	return map[string]any{"type": kind, "status": status, "message": msg}
}

func TestFromObject(t *testing.T) {
	active := FromObject(cert("a", map[string]any{
		"conditions":  []any{cond("Ready", "True", "Certificate is up to date and has not expired")},
		"notAfter":    "2027-02-22T10:00:00Z",
		"renewalTime": "2027-01-23T10:00:00Z",
	}))
	assert.Equal(t, certstatus.StateActive, active.State)
	require.NotNil(t, active.NotAfter)
	assert.Equal(t, time.Date(2027, 2, 22, 10, 0, 0, 0, time.UTC), active.NotAfter.UTC())
	require.NotNil(t, active.RenewalAt)

	renewing := FromObject(cert("r", map[string]any{"conditions": []any{
		cond("Ready", "True", "ok"), cond("Issuing", "True", "Renewing certificate as renewal was scheduled")}}))
	assert.Equal(t, certstatus.StateActive, renewing.State, "un renouvellement en cours reste actif")

	assert.Equal(t, &certstatus.Status{State: certstatus.StatePending}, FromObject(cert("n", nil)), "sans état : en cours")
	assert.Equal(t, certstatus.StatePending, FromObject(cert("i", map[string]any{"conditions": []any{
		cond("Ready", "False", "Issuing certificate as Secret does not exist"), cond("Issuing", "True", "Issuing")},
		"lastFailureTime": "2026-11-24T10:00:00Z"})).State, "nouvelle tentative en cours")

	limited := FromObject(cert("l", map[string]any{"conditions": []any{
		cond("Ready", "False", "Issuing certificate as Secret does not exist"),
		cond("Issuing", "False", `The certificate request has failed to complete and will be retried: 429 urn:ietf:params:acme:error:rateLimited: too many certificates`)},
		"lastFailureTime": "2026-11-24T10:00:00Z"}))
	assert.Equal(t, &certstatus.Status{State: certstatus.StateFailed, Reason: certstatus.ReasonRateLimited}, limited)

	challenge := FromObject(cert("c", map[string]any{"conditions": []any{
		cond("Ready", "False", "x"), cond("Issuing", "False", "Failed to wait for order resource: order is in errored state: challenge failed")},
		"lastFailureTime": "2026-11-24T10:00:00Z"}))
	assert.Equal(t, certstatus.ReasonChallenge, challenge.Reason)

	other := FromObject(cert("o", map[string]any{"conditions": []any{cond("Issuing", "False", "boom")},
		"lastFailureTime": "2026-11-24T10:00:00Z"}))
	assert.Equal(t, &certstatus.Status{State: certstatus.StateFailed, Reason: certstatus.ReasonUnknown}, other, "jamais le texte brut")
}

func TestList(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{GVR: "CertificateList"},
		cert("web-d-1234567890", map[string]any{"conditions": []any{cond("Ready", "True", "ok")}}))
	got, err := List(context.Background(), dyn, "km-p")
	require.NoError(t, err)
	require.Contains(t, got, "web-d-1234567890")
	assert.Equal(t, certstatus.StateActive, got["web-d-1234567890"].State)

	none, err := List(context.Background(), dyn, "autre")
	require.NoError(t, err)
	assert.Empty(t, none)
}
