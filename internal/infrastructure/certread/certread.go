// Package certread : lecture des Certificate de cert-manager dans le cluster
// (état certstatus.Status).
package certread

import (
	"context"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/thekrauss/kubemanager/internal/core/certstatus"
)

// ReadTimeout : lecture du cluster bornée ; au-delà, rien n'est affiché.
const ReadTimeout = 3 * time.Second

// GVR : Certificate de cert-manager.
var GVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

// List : certificats d'un namespace, par nom (nom du Certificate = nom du
// secret TLS de l'Ingress, posé par cert-manager).
func List(ctx context.Context, dyn dynamic.Interface, namespace string) (map[string]*certstatus.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	list, err := dyn.Resource(GVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*certstatus.Status, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].GetName()] = FromObject(&list.Items[i])
	}
	return out, nil
}

// FromObject : état d'un Certificate.
//   - Ready=True : actif (même pendant un renouvellement) ;
//   - Issuing=True : délivrance en cours, y compris une nouvelle tentative ;
//   - un échec enregistré (lastFailureTime) sans tentative en cours : échec ;
//   - sinon : en cours.
func FromObject(u *unstructured.Unstructured) *certstatus.Status {
	ready, _ := condition(u, "Ready")
	issuing, issuingMsg := condition(u, "Issuing")
	_, readyMsg := condition(u, "Ready")
	if ready == "True" {
		s := &certstatus.Status{State: certstatus.StateActive}
		s.NotAfter = timeField(u, "notAfter")
		s.RenewalAt = timeField(u, "renewalTime")
		return s
	}
	if issuing == "True" {
		return certstatus.Pending()
	}
	if failed, _, _ := unstructured.NestedString(u.Object, "status", "lastFailureTime"); failed != "" {
		return &certstatus.Status{State: certstatus.StateFailed, Reason: reason(issuingMsg + " " + readyMsg)}
	}
	return certstatus.Pending()
}

func condition(u *unstructured.Unstructured, kind string) (status, message string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != kind {
			continue
		}
		status, _ = m["status"].(string)
		message, _ = m["message"].(string)
		return status, message
	}
	return "", ""
}

func timeField(u *unstructured.Unstructured, field string) *time.Time {
	raw, _, _ := unstructured.NestedString(u.Object, "status", field)
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &t
}

// reason : catégorie d'un message d'échec de cert-manager / ACME.
func reason(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "ratelimited") || strings.Contains(m, "rate limit") || strings.Contains(m, "too many"):
		return certstatus.ReasonRateLimited
	case strings.Contains(m, "challenge") || strings.Contains(m, "acme:error:unauthorized") ||
		strings.Contains(m, "acme:error:connection") || strings.Contains(m, "acme:error:dns") ||
		strings.Contains(m, "nxdomain") || strings.Contains(m, "no valid a records"):
		return certstatus.ReasonChallenge
	}
	return certstatus.ReasonUnknown
}
