package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.uber.org/zap"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Repère des Ingress gérés ici : seuls ceux-là sont créés ou supprimés.
const (
	edgeDomainLabel      = "betorun.io/edge-domain"
	edgeHostAnnotation   = "betorun.io/hostname"
	certIssuerAnnotation = "cert-manager.io/cluster-issuer"
	// Redirection HTTPS sous Traefik : Middleware « https-redirect » de
	// deploy/k8s/static-edge.yaml. Le solveur HTTP-01 n'est pas touché (chemin
	// plus long, routeur prioritaire).
	traefikMiddlewaresAnnotation = "traefik.ingress.kubernetes.io/router.middlewares"
	traefikClass                 = "traefik"
)

// EdgeIngress : où trouver static-edge dans le cluster.
type EdgeIngress struct {
	Namespace    string
	Service      string
	Port         int32
	Issuer       string
	IngressClass string
}

// HostLister : noms à servir (domaines vérifiés des sites non supprimés).
type HostLister interface {
	ServedDomains(ctx context.Context) ([]string, error)
}

// EdgeDomainActivities : un Ingress (et son certificat HTTP-01, émis par
// cert-manager) par domaine personnalisé vérifié. Les sous-domaines
// *.<base_domain> passent par le certificat joker, hors de cette activité.
type EdgeDomainActivities struct {
	K8s    kubernetes.Interface
	Hosts  HostLister
	Edge   EdgeIngress
	Logger *zap.SugaredLogger
}

// SyncEdgeDomains : aligne les Ingress sur la base. Idempotente : la rejouer
// ne change rien si tout est déjà en place.
func (a *EdgeDomainActivities) SyncEdgeDomains(ctx context.Context) error {
	hosts, err := a.Hosts.ServedDomains(ctx)
	if err != nil {
		return err
	}
	want := make(map[string]string, len(hosts)) // nom de l'objet → hôte
	for _, h := range hosts {
		want[edgeObjectName(h)] = h
	}
	ingresses := a.K8s.NetworkingV1().Ingresses(a.Edge.Namespace)
	current, err := ingresses.List(ctx, metav1.ListOptions{LabelSelector: edgeDomainLabel + "=true"})
	if err != nil {
		return fmt.Errorf("list edge ingresses: %w", err)
	}
	have := map[string]bool{}
	for _, ing := range current.Items {
		if _, ok := want[ing.Name]; ok {
			have[ing.Name] = true
			continue
		}
		// Domaine retiré : l'Ingress, puis le secret du certificat (le
		// Certificate de cert-manager suit l'Ingress, pas son secret).
		if err := ingresses.Delete(ctx, ing.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete edge ingress %s: %w", ing.Name, err)
		}
		if err := a.K8s.CoreV1().Secrets(a.Edge.Namespace).Delete(ctx, ing.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete edge certificate %s: %w", ing.Name, err)
		}
		a.Logger.Infow("edge domain removed", "hostname", ing.Annotations[edgeHostAnnotation])
	}
	for name, host := range want {
		if have[name] {
			continue
		}
		if _, err := ingresses.Create(ctx, a.ingress(name, host), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create edge ingress for %s: %w", host, err)
		}
		a.Logger.Infow("edge domain added", "hostname", host)
	}
	return nil
}

func (a *EdgeDomainActivities) ingress(name, host string) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	var class *string
	if a.Edge.IngressClass != "" {
		class = &a.Edge.IngressClass
	}
	annotations := map[string]string{edgeHostAnnotation: host, certIssuerAnnotation: a.Edge.Issuer}
	if a.Edge.IngressClass == traefikClass {
		annotations[traefikMiddlewaresAnnotation] = a.Edge.Namespace + "-https-redirect@kubernetescrd"
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{edgeDomainLabel: "true"},
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: class,
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: name}},
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: a.Edge.Service, Port: networkingv1.ServiceBackendPort{Number: a.Edge.Port},
						}},
					}},
				}},
			}},
		},
	}
}

// EdgeObjectName : nom de l'Ingress et du certificat d'un domaine de site.
func EdgeObjectName(host string) string { return edgeObjectName(host) }

// edgeObjectName : nom d'objet stable et valide pour un hôte (un nom de
// domaine peut dépasser 63 caractères).
func edgeObjectName(host string) string {
	sum := sha256.Sum256([]byte(host))
	return "domain-" + hex.EncodeToString(sum[:])[:20]
}
