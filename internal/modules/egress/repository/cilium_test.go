package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
)

func TestCilium_ApplyUpdateRemove(t *testing.T) {
	ctx := context.Background()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{EgressPolicyGVR: "CiliumEgressGatewayPolicyList"})
	c := &Cilium{Dynamic: func(context.Context, string) (dynamic.Interface, error) { return dyn, nil }}
	b := domain.Binding{ClusterID: "gra-1", Namespace: "km-a", Address: "203.0.113.7", GatewayNode: "gw-1"}

	require.NoError(t, c.Apply(ctx, b))
	obj, err := dyn.Resource(EgressPolicyGVR).Get(ctx, "betorun-egress-km-a", metav1.GetOptions{})
	require.NoError(t, err)
	sel, _, _ := unstructured.NestedSlice(obj.Object, "spec", "selectors")
	require.Len(t, sel, 1)
	lbl, _, _ := unstructured.NestedString(sel[0].(map[string]any), "podSelector", "matchLabels", "io.kubernetes.pod.namespace")
	assert.Equal(t, "km-a", lbl)
	ip, _, _ := unstructured.NestedString(obj.Object, "spec", "egressGateway", "egressIP")
	assert.Equal(t, "203.0.113.7", ip)
	node, _, _ := unstructured.NestedString(obj.Object, "spec", "egressGateway", "nodeSelector", "matchLabels", "kubernetes.io/hostname")
	assert.Equal(t, "gw-1", node)
	ex, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "excludedCIDRs")
	assert.Contains(t, ex, "10.0.0.0/8")

	b.Address = "203.0.113.8"
	require.NoError(t, c.Apply(ctx, b), "rejouée : mise à jour")
	obj, _ = dyn.Resource(EgressPolicyGVR).Get(ctx, "betorun-egress-km-a", metav1.GetOptions{})
	ip, _, _ = unstructured.NestedString(obj.Object, "spec", "egressGateway", "egressIP")
	assert.Equal(t, "203.0.113.8", ip)

	require.NoError(t, c.Remove(ctx, "gra-1", "km-a"))
	require.NoError(t, c.Remove(ctx, "gra-1", "km-a"), "absente : rien à faire")
}

func TestIsNoKind(t *testing.T) {
	gr := schema.GroupResource{Group: "cilium.io", Resource: "ciliumegressgatewaypolicies"}
	assert.False(t, isNoKind(apierrors.NewNotFound(gr, "betorun-egress-km-a")), "objet absent")
	noKind := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound,
		Message: "the server could not find the requested resource"}}
	assert.True(t, isNoKind(noKind), "ressource inconnue : Cilium absent")
	assert.ErrorIs(t, missing(noKind), ErrGatewayMissing)
}

func TestNormalizeAddress(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7": "203.0.113.7", " 51.75.141.233 ": "51.75.141.233",
		"10.1.2.3": "", "192.168.1.1": "", "100.64.0.1": "", "127.0.0.1": "", "0.0.0.0": "",
		"2001:db8::1": "", "nope": "", "": "",
	} {
		assert.Equal(t, want, domain.NormalizeAddress(in), in)
	}
}
