package activities

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type hostList []string

func (h *hostList) ServedDomains(context.Context) ([]string, error) { return *h, nil }

func TestSyncEdgeDomains(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewSimpleClientset()
	hosts := &hostList{"www.sunupay.sn", "boutique.example"}
	a := &EdgeDomainActivities{K8s: k8s, Hosts: hosts, Logger: zap.NewNop().Sugar(),
		Edge: EdgeIngress{Namespace: "betorun-edge", Service: "static-edge", Port: 80, Issuer: "letsencrypt-prod", IngressClass: "traefik"}}

	require.NoError(t, a.SyncEdgeDomains(ctx))
	list, err := k8s.NetworkingV1().Ingresses("betorun-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 2)
	ing, err := k8s.NetworkingV1().Ingresses("betorun-edge").Get(ctx, edgeObjectName("www.sunupay.sn"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "www.sunupay.sn", ing.Spec.Rules[0].Host)
	assert.Equal(t, "letsencrypt-prod", ing.Annotations[certIssuerAnnotation])
	assert.Equal(t, ing.Name, ing.Spec.TLS[0].SecretName)
	assert.Equal(t, "traefik", *ing.Spec.IngressClassName)
	assert.Equal(t, "betorun-edge-https-redirect@kubernetescrd", ing.Annotations[traefikMiddlewaresAnnotation])

	// Un Ingress étranger (sans le repère) n'est jamais touché.
	_, err = k8s.NetworkingV1().Ingresses("betorun-edge").Create(ctx, ingressNamed("autre"), metav1.CreateOptions{})
	require.NoError(t, err)
	// Certificat émis pour le domaine qui va partir.
	gone := edgeObjectName("boutique.example")
	_, err = k8s.CoreV1().Secrets("betorun-edge").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: gone}}, metav1.CreateOptions{})
	require.NoError(t, err)

	*hosts = hostList{"www.sunupay.sn"}
	require.NoError(t, a.SyncEdgeDomains(ctx))
	require.NoError(t, a.SyncEdgeDomains(ctx), "rejouable")
	list, err = k8s.NetworkingV1().Ingresses("betorun-edge").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	names := []string{}
	for _, i := range list.Items {
		names = append(names, i.Name)
	}
	assert.ElementsMatch(t, []string{edgeObjectName("www.sunupay.sn"), "autre"}, names)
	_, err = k8s.CoreV1().Secrets("betorun-edge").Get(ctx, gone, metav1.GetOptions{})
	assert.Error(t, err, "secret du certificat retiré avec le domaine")
}

func ingressNamed(name string) *networkingv1.Ingress {
	return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name}}
}
