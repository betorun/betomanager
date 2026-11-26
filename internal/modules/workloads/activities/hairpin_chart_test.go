package activities

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// networkPolicyOf : la NetworkPolicy rendue par le chart pour cette entrée.
func networkPolicyOf(t *testing.T, in InstallWorkloadInput) networkingv1.NetworkPolicy {
	t.Helper()
	for doc := range strings.SplitSeq(renderKind(t, in), "\n---\n") {
		if !strings.Contains(doc, "kind: NetworkPolicy") {
			continue
		}
		var np networkingv1.NetworkPolicy
		require.NoError(t, yaml.Unmarshal([]byte(doc), &np))
		return np
	}
	t.Fatal("aucune NetworkPolicy rendue")
	return networkingv1.NetworkPolicy{}
}

// ingressRule : la règle de sortie vers le contrôleur d'entrée, s'il y en a une.
func ingressRule(np networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule {
	for i, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.NamespaceSelector != nil && p.PodSelector != nil && p.PodSelector.MatchLabels["app.kubernetes.io/name"] != "" &&
				p.PodSelector.MatchLabels["k8s-app"] == "" {
				return &np.Spec.Egress[i]
			}
		}
	}
	return nil
}

func ports(r *networkingv1.NetworkPolicyEgressRule) []int {
	var out []int
	for _, p := range r.Ports {
		out = append(out, p.Port.IntValue())
	}
	return out
}

// Un conteneur qui appelle l'adresse publique d'un autre conteneur du même
// cluster (le site appelé par SYK) doit joindre le contrôleur d'entrée, sur
// ses seuls ports d'entrée : ni tableau de bord (8080) ni métriques (9100).
func TestChart_EgressReachesIngressControllerForHairpin(t *testing.T) {
	in := baseInput("web")
	in.IngressClass, in.IngressNamespace = "traefik", "kube-system"
	r := ingressRule(networkPolicyOf(t, in))
	require.NotNil(t, r, "règle hairpin absente")
	peer := r.To[0]
	assert.Equal(t, "kube-system", peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "traefik"}, peer.PodSelector.MatchLabels)
	assert.ElementsMatch(t, []int{8000, 8443}, ports(r))

	// Même chose pour un worker : il appelle, il n'est pas appelé.
	w := baseInput("worker")
	w.IngressClass, w.IngressNamespace = "traefik", "kube-system"
	assert.NotNil(t, ingressRule(networkPolicyOf(t, w)))

	n := ingressRule(networkPolicyOf(t, baseInput("web")))
	require.NotNil(t, n)
	assert.Equal(t, "ingress-nginx", n.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "controller", n.To[0].PodSelector.MatchLabels["app.kubernetes.io/component"])
	assert.ElementsMatch(t, []int{80, 443}, ports(n))
}

// Classe d'entrée inconnue : aucune règle, rien n'est ouvert au hasard.
func TestChart_UnknownIngressClassOpensNothing(t *testing.T) {
	in := baseInput("web")
	in.IngressClass = "haproxy"
	assert.Nil(t, ingressRule(networkPolicyOf(t, in)))
}
