package databases

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// ClusterGVR : Cluster CloudNativePG.
var ClusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}

// operatorNamespace : espace de l'opérateur (manifeste officiel).
const operatorNamespace = "cnpg-system"

// ClusterState : état d'un Cluster, lu dans son status.
type ClusterState struct {
	Exists        bool
	Phase         string // texte de CloudNativePG, ex. « Cluster in healthy state »
	ReadyInstance bool
	Image         string
}

// Clusters : ce que le service demande au cluster Kubernetes.
type Clusters interface {
	Create(ctx context.Context, ns string, spec ClusterSpec) error
	State(ctx context.Context, ns, name string) (ClusterState, error)
	Delete(ctx context.Context, ns, name string) error
}

// ClusterSpec : une base à créer.
type ClusterSpec struct {
	Name       string // nom du Cluster (db-<nom>)
	DatabaseID string
	Storage    string
}

// CNPG : Clusters sur un vrai cluster (client dynamique pour la CRD, client
// typé pour la politique réseau).
type CNPG struct {
	Dynamic      dynamic.Interface
	Kube         kubernetes.Interface
	StorageClass string // vide : classe par défaut du cluster
}

// Ressources d'une instance : demandes et limites obligatoires (le projet a
// un quota CPU et mémoire ; sans elles, le pod serait refusé).
const (
	cpuRequest = "100m"
	memRequest = "256Mi"
	cpuLimit   = "500m"
	memLimit   = "512Mi"
)

func labels(spec ClusterSpec) map[string]any {
	return map[string]any{
		"app.kubernetes.io/managed-by": "betorun",
		"betorun.dev/database-id":      spec.DatabaseID,
	}
}

// clusterObject : le Cluster CloudNativePG d'une base.
func (c *CNPG) clusterObject(ns string, spec ClusterSpec) *unstructured.Unstructured {
	storage := map[string]any{"size": spec.Storage}
	if c.StorageClass != "" {
		storage["storageClass"] = c.StorageClass
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": spec.Name, "namespace": ns, "labels": labels(spec)},
		"spec": map[string]any{
			"instances": int64(1),
			"storage":   storage,
			"resources": map[string]any{
				"requests": map[string]any{"cpu": cpuRequest, "memory": memRequest},
				"limits":   map[string]any{"cpu": cpuLimit, "memory": memLimit},
			},
			// Base « app », propriétaire « app » ; pas d'accès superutilisateur.
			"bootstrap":             map[string]any{"initdb": map[string]any{"database": "app", "owner": "app"}},
			"enableSuperuserAccess": false,
		},
	}}
}

// networkPolicy : seuls les pods du projet (même espace) et l'opérateur
// joignent l'instance. Sans elle, n'importe quel pod du cluster le pourrait.
func networkPolicy(ns string, spec ClusterSpec) *networkingv1.NetworkPolicy {
	pg := intstr.FromInt32(5432)
	tcp := corev1.ProtocolTCP
	lbl := map[string]string{"app.kubernetes.io/managed-by": "betorun", "betorun.dev/database-id": spec.DatabaseID}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: ns, Labels: lbl},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"cnpg.io/cluster": spec.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				// Conteneurs du projet : Postgres seulement.
				{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &pg}}},
				// Opérateur CloudNativePG : supervision et pilotage de l'instance.
				{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace}}}}},
			},
		},
	}
}

func (c *CNPG) Create(ctx context.Context, ns string, spec ClusterSpec) error {
	if _, err := c.Kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, networkPolicy(ns, spec), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("network policy: %w", err)
	}
	_, err := c.Dynamic.Resource(ClusterGVR).Namespace(ns).Create(ctx, c.clusterObject(ns, spec), metav1.CreateOptions{})
	if apierrors.IsNotFound(err) {
		// CRD absente : l'opérateur n'est pas installé sur ce cluster.
		return ErrOperatorMissing
	}
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("cluster: %w", err)
	}
	return nil
}

// ErrOperatorMissing : CloudNativePG n'est pas installé.
var ErrOperatorMissing = errors.New("CloudNativePG is not installed on the cluster")

func (c *CNPG) State(ctx context.Context, ns, name string) (ClusterState, error) {
	obj, err := c.Dynamic.Resource(ClusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ClusterState{}, nil
	}
	if err != nil {
		return ClusterState{}, err
	}
	st := ClusterState{Exists: true}
	st.Phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
	st.Image, _, _ = unstructured.NestedString(obj.Object, "status", "image")
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyInstances")
	st.ReadyInstance = ready >= 1
	return st, nil
}

// Delete : le Cluster (ses disques suivent : CloudNativePG en est
// propriétaire) et sa politique réseau. Absent : rien à faire.
func (c *CNPG) Delete(ctx context.Context, ns, name string) error {
	err := c.Dynamic.Resource(ClusterGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cluster: %w", err)
	}
	err = c.Kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("network policy: %w", err)
	}
	return nil
}
