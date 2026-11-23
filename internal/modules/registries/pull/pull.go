// Package pull : Secret de pull d'un namespace de projet. Un seul Secret par
// projet, avec le registre de la plateforme et les registres externes du
// projet ; les valeurs sont lues ici (worker ou API), jamais transmises à
// Temporal.
package pull

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/modules/builds/registry"
	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// SecretName : nom du Secret (inchangé : les déploiements existants le citent).
const SecretName = registry.PullSecretName

// Platform : registre de la plateforme (nil si les builds sont désactivés).
type Platform struct {
	Host  string
	Creds registry.Credentials
}

// Auths : identifiants externes d'un projet (repository.Store).
type Auths interface {
	Auths(ctx context.Context, project uuid.UUID, host string) ([]domain.Auth, error)
}

// Secrets : écrit le Secret de pull d'un projet.
type Secrets struct {
	// K8s : client du cluster de chaque projet.
	K8s      k8sprovider.ProjectClient
	Platform *Platform
	External Auths
}

// ErrNothingToPull : ni registre de la plateforme ni accès externe.
var ErrNothingToPull = errors.New("no registry access for this project")

// Config : contenu .dockerconfigjson. Vide s'il n'y a aucune entrée.
func (s *Secrets) Config(ctx context.Context, project uuid.UUID) ([]byte, error) {
	var auths []domain.Auth
	if s.Platform != nil && s.Platform.Host != "" {
		user, password := s.Platform.Creds.ForPull(project)
		auths = append(auths, domain.Auth{Host: s.Platform.Host, Username: user, Password: password})
	}
	if s.External != nil {
		external, err := s.External.Auths(ctx, project, "")
		if err != nil {
			return nil, err
		}
		auths = append(auths, external...)
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return dockerConfig(auths)
}

func dockerConfig(auths []domain.Auth) ([]byte, error) {
	entries := make(map[string]map[string]string, len(auths))
	for _, a := range auths {
		entries[a.ConfigKey()] = map[string]string{
			"username": a.Username, "password": a.Password,
			"auth": base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password)),
		}
	}
	return json.Marshal(map[string]any{"auths": entries})
}

// Sync : écrit le Secret, ou le supprime s'il n'y a plus aucune entrée.
// ErrNothingToPull dans ce dernier cas.
func (s *Secrets) Sync(ctx context.Context, namespace string, project uuid.UUID) error {
	raw, err := s.Config(ctx, project)
	if err != nil {
		return err
	}
	kube, err := s.K8s(ctx, project)
	if err != nil {
		return err
	}
	api := kube.CoreV1().Secrets(namespace)
	if raw == nil {
		if err := api.Delete(ctx, SecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return ErrNothingToPull
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: namespace,
			Labels: map[string]string{utils.LabelKmProjectID: project.String()}},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
	if _, err := api.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err = api.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	}
	return nil
}

// Refresh : réécrit le Secret s'il existe déjà (un accès a changé). Un
// namespace sans Secret n'a encore rien déployé : rien à faire.
func (s *Secrets) Refresh(ctx context.Context, namespace string, project uuid.UUID) error {
	kube, err := s.K8s(ctx, project)
	if err != nil {
		return err
	}
	if _, err := kube.CoreV1().Secrets(namespace).Get(ctx, SecretName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := s.Sync(ctx, namespace, project); err != nil && !errors.Is(err, ErrNothingToPull) {
		return err
	}
	return nil
}
