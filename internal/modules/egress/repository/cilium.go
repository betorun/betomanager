package repository

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
)

// EgressPolicyGVR : règle de sortie de Cilium (objet de cluster).
var EgressPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumegressgatewaypolicies"}

// ErrGatewayMissing : la passerelle déclarée n'est pas installée sur le cluster.
var ErrGatewayMissing = errors.New("the cluster's egress gateway is not installed")

// privateRanges : jamais par la passerelle (réseau du projet et de la
// plateforme, interdit en sortie par la politique réseau).
var privateRanges = []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

// Cilium : règles CiliumEgressGatewayPolicy, une par projet.
type Cilium struct {
	// Dynamic : client du cluster (pool multi-cluster).
	Dynamic func(ctx context.Context, clusterID string) (dynamic.Interface, error)
}

// PolicyName : nom de la règle d'un projet.
func PolicyName(namespace string) string { return "betorun-egress-" + namespace }

// Policy : tous les pods du namespace du projet sortent par l'IP, depuis le
// nœud passerelle ; destinations privées exclues.
func Policy(b domain.Binding) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumEgressGatewayPolicy",
		"metadata": map[string]any{"name": PolicyName(b.Namespace),
			"labels": map[string]any{"app.kubernetes.io/managed-by": "betorun"}},
		"spec": map[string]any{
			"selectors": []any{map[string]any{"podSelector": map[string]any{
				"matchLabels": map[string]any{"io.kubernetes.pod.namespace": b.Namespace}}}},
			"destinationCIDRs": []any{"0.0.0.0/0"},
			"excludedCIDRs":    privateRanges,
			"egressGateway": map[string]any{
				"nodeSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/hostname": b.GatewayNode}},
				"egressIP":     b.Address,
			},
		},
	}}
}

// Apply : pose ou met à jour la règle du projet.
func (c *Cilium) Apply(ctx context.Context, b domain.Binding) error {
	dyn, err := c.Dynamic(ctx, b.ClusterID)
	if err != nil {
		return err
	}
	res := dyn.Resource(EgressPolicyGVR)
	want := Policy(b)
	cur, err := res.Get(ctx, want.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = res.Create(ctx, want, metav1.CreateOptions{})
	case err == nil:
		want.SetResourceVersion(cur.GetResourceVersion())
		_, err = res.Update(ctx, want, metav1.UpdateOptions{})
	}
	return missing(err)
}

// Remove : retire la règle (absente : rien à faire).
func (c *Cilium) Remove(ctx context.Context, clusterID, namespace string) error {
	dyn, err := c.Dynamic(ctx, clusterID)
	if err != nil {
		return err
	}
	err = dyn.Resource(EgressPolicyGVR).Delete(ctx, PolicyName(namespace), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) && !isNoKind(err) {
		return nil
	}
	return missing(err)
}

// missing : CRD absente (Cilium non installé ou sans passerelle).
func missing(err error) error {
	if err != nil && isNoKind(err) {
		return fmt.Errorf("%w (%v)", ErrGatewayMissing, err)
	}
	return err
}

// isNoKind : la ressource elle-même est inconnue du serveur (404 sans nom d'objet).
func isNoKind(err error) bool {
	var st apierrors.APIStatus
	if !errors.As(err, &st) || !apierrors.IsNotFound(err) {
		return false
	}
	d := st.Status().Details
	return d == nil || d.Name == ""
}
