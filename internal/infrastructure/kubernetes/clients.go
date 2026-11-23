package kubernetes

import (
	"context"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
)

// Clients typés par projet (lot 2b) : les adaptateurs dont les méthodes ne
// connaissent que le namespace (ou le projet) prennent l'une de ces
// fonctions ; les tests y passent un faux client (fake.NewSimpleClientset).

// NamespaceClient : client du cluster d'un namespace de projet.
type NamespaceClient func(ctx context.Context, namespace string) (kubernetes.Interface, error)

// ProjectClient : client du cluster d'un projet.
type ProjectClient func(ctx context.Context, project uuid.UUID) (kubernetes.Interface, error)

// ByNamespace : NamespaceClient d'un résolveur.
func ByNamespace(r Resolver) NamespaceClient {
	return func(ctx context.Context, namespace string) (kubernetes.Interface, error) {
		p, err := r.ForNamespace(ctx, namespace)
		if err != nil {
			return nil, err
		}
		return p.Client, nil
	}
}

// ByProject : ProjectClient d'un résolveur.
func ByProject(r Resolver) ProjectClient {
	return func(ctx context.Context, project uuid.UUID) (kubernetes.Interface, error) {
		p, err := r.ForProject(ctx, project)
		if err != nil {
			return nil, err
		}
		return p.Client, nil
	}
}

// Fixed : un seul client pour tous les namespaces (un cluster, tests).
func Fixed(c kubernetes.Interface) NamespaceClient {
	return func(context.Context, string) (kubernetes.Interface, error) { return c, nil }
}
