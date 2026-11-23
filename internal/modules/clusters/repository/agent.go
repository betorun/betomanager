package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

// Installer : pose le compte plateforme et l'agent sur un cluster (lot 3).
// Création ou mise à jour : le rejouer ne casse rien.
type Installer struct {
	Client kubernetes.Interface
	// TokenWait, Poll : attente du jeton du compte plateforme.
	TokenWait time.Duration
	Poll      time.Duration
}

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "betorun"
	keyMountDir    = "/var/run/betorun"
	agentPort      = 8080
	// keyHashAnnotation : empreinte du fichier de clé (jamais la clé).
	keyHashAnnotation = "betorun.dev/key-sha"
)

func shortHash(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func labels(name string) map[string]string {
	return map[string]string{managedByLabel: managedBy, "app.kubernetes.io/name": name}
}

// PlatformRules : droits du compte plateforme. Même contenu que
// deploy/k8s/rbac.yaml (vérifié par un test) : aucun droit sur le RBAC, les
// CRD ni exec/port-forward.
func PlatformRules() []rbacv1.PolicyRule {
	crud := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	read := []string{"get", "list", "watch"}
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"namespaces", "resourcequotas", "secrets", "services", "serviceaccounts", "persistentvolumeclaims"}, Verbs: crud},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"pods/log", "events"}, Verbs: read},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: crud},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, Verbs: []string{"get", "update", "patch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: read},
		{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: crud},
		{APIGroups: []string{"batch"}, Resources: []string{"cronjobs"}, Verbs: crud},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses", "networkpolicies"}, Verbs: crud},
		{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: crud},
		{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: crud},
		{APIGroups: []string{"traefik.io"}, Resources: []string{"middlewares"}, Verbs: crud},
		{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters", "scheduledbackups", "backups"}, Verbs: crud},
		{APIGroups: []string{"barmancloud.cnpg.io"}, Resources: []string{"objectstores"}, Verbs: crud},
	}
}

// AgentRules : droits du watcher (ceux du chart du dépôt watcher).
func AgentRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods", "namespaces", "events"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch"}},
		{NonResourceURLs: []string{"/.well-known/openid-configuration", "/openid/v1/jwks"}, Verbs: []string{"get"}},
	}
}

// Platform : namespace, compte plateforme, son jeton et son rôle ; rend le
// jeton et le certificat d'autorité du cluster. Exige un administrateur.
func (i *Installer) Platform(ctx context.Context) (token, ca []byte, err error) {
	if err := i.namespace(ctx); err != nil {
		return nil, nil, err
	}
	if err := i.serviceAccount(ctx, domain.PlatformAccount, false); err != nil {
		return nil, nil, err
	}
	if err := i.clusterRole(ctx, domain.PlatformAccount, PlatformRules()); err != nil {
		return nil, nil, err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: domain.PlatformTokenSecret, Namespace: domain.AgentNamespace,
		Labels: labels(domain.PlatformAccount), Annotations: map[string]string{corev1.ServiceAccountNameKey: domain.PlatformAccount}},
		Type: corev1.SecretTypeServiceAccountToken}
	if _, err := i.Client.CoreV1().Secrets(domain.AgentNamespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, nil, admin(fmt.Errorf("platform token secret: %w", err))
	}
	wait, poll := i.TokenWait, i.Poll
	if wait == 0 {
		wait = 30 * time.Second
	}
	if poll == 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(wait)
	for {
		s, err := i.Client.CoreV1().Secrets(domain.AgentNamespace).Get(ctx, domain.PlatformTokenSecret, metav1.GetOptions{})
		if err != nil {
			return nil, nil, admin(err)
		}
		if len(s.Data[corev1.ServiceAccountTokenKey]) > 0 && len(s.Data[corev1.ServiceAccountRootCAKey]) > 0 {
			return s.Data[corev1.ServiceAccountTokenKey], s.Data[corev1.ServiceAccountRootCAKey], nil
		}
		if time.Now().After(deadline) {
			return nil, nil, domain.ErrPlatformTokenPending
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// AgentRBAC : rôle de cluster du watcher et sa liaison (administrateur).
func (i *Installer) AgentRBAC(ctx context.Context) error {
	if err := i.namespace(ctx); err != nil {
		return err
	}
	return i.clusterRole(ctx, domain.AgentName, AgentRules())
}

// Agent : compte, fichier de clé et Deployment du watcher. Possible avec le
// compte plateforme (aucun objet RBAC touché) ; KeyFile vide : clé en place gardée.
func (i *Installer) Agent(ctx context.Context, spec domain.AgentSpec) error {
	if err := i.namespace(ctx); err != nil {
		return err
	}
	if err := i.serviceAccount(ctx, domain.AgentName, true); err != nil {
		return err
	}
	if len(spec.KeyFile) > 0 {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: domain.AgentKeySecret, Namespace: domain.AgentNamespace,
			Labels: labels(domain.AgentName)}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"key.json": spec.KeyFile}}
		if err := upsert(ctx, i.Client.CoreV1().Secrets(domain.AgentNamespace), sec, func(cur *corev1.Secret) {
			cur.Data, cur.Labels = sec.Data, sec.Labels
		}); err != nil {
			return fmt.Errorf("agent key secret: %w", err)
		}
	}
	dep := agentDeployment(spec)
	return upsert(ctx, i.Client.AppsV1().Deployments(domain.AgentNamespace), dep, func(cur *appsv1.Deployment) {
		// Clé gardée : son empreinte aussi (pas de redémarrage pour rien).
		if len(spec.KeyFile) == 0 {
			dep.Spec.Template.Annotations[keyHashAnnotation] = cur.Spec.Template.Annotations[keyHashAnnotation]
		}
		cur.Labels = dep.Labels
		cur.Spec.Replicas = dep.Spec.Replicas
		// Empreinte de la clé dans le modèle : nouvelle clé → nouveaux pods
		// (le watcher ne relit pas son fichier).
		cur.Spec.Template = dep.Spec.Template
	})
}

// State : Deployment de l'agent (installé par la plateforme ou non).
func (i *Installer) State(ctx context.Context) (*domain.AgentState, error) {
	d, err := i.Client.AppsV1().Deployments(domain.AgentNamespace).Get(ctx, domain.AgentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &domain.AgentState{}, nil
	}
	if err != nil {
		return nil, denied(err)
	}
	st := &domain.AgentState{Installed: true, Ready: d.Status.ReadyReplicas, ManagedByPlatform: d.Labels[managedByLabel] == managedBy}
	if d.Spec.Replicas != nil {
		st.Desired = *d.Spec.Replicas
	}
	if len(d.Spec.Template.Spec.Containers) > 0 {
		st.Image = d.Spec.Template.Spec.Containers[0].Image
	}
	if st.Ready < st.Desired {
		pods, err := i.Client.CoreV1().Pods(domain.AgentNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + domain.AgentName})
		if err == nil {
			for _, p := range pods.Items {
				for _, cs := range p.Status.ContainerStatuses {
					if w := cs.State.Waiting; w != nil && w.Reason != "" {
						st.Reason = w.Reason
					}
				}
			}
		}
	}
	return st, nil
}

// RestrictedKubeconfig : kubeconfig du compte plateforme (jeton et autorité
// inclus), pour le serveur du kubeconfig de préparation.
func RestrictedKubeconfig(clusterID, server string, token, ca []byte) ([]byte, error) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterID] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: ca}
	cfg.AuthInfos[domain.PlatformAccount] = &clientcmdapi.AuthInfo{Token: string(token)}
	cfg.Contexts[clusterID] = &clientcmdapi.Context{Cluster: clusterID, AuthInfo: domain.PlatformAccount}
	cfg.CurrentContext = clusterID
	return clientcmd.Write(*cfg)
}

func agentDeployment(spec domain.AgentSpec) *appsv1.Deployment {
	one := int32(1)
	no, yes := false, true
	uid := int64(65532)
	mode := int32(0o440)
	lbl := labels(domain.AgentName)
	probe := func(path string, delay int32) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("http")}},
			InitialDelaySeconds: delay, PeriodSeconds: 15}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: domain.AgentName, Namespace: domain.AgentNamespace, Labels: lbl},
		Spec: appsv1.DeploymentSpec{
			// Une instance : le watcher élit un leader (Lease) ; un seul
			// pod suffit, il est recréé s'il tombe.
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": domain.AgentName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl, Annotations: map[string]string{keyHashAnnotation: shortHash(spec.KeyFile)}},
				Spec: corev1.PodSpec{
					ServiceAccountName: domain.AgentName,
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name:            "watcher",
						Image:           spec.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args: []string{
							"--restate-url=" + spec.APIURL,
							"--cluster-id=" + spec.ClusterID,
							"--restate-timeout=5s",
							"--watch-namespace-mode=label",
							"--managed-ns-label=" + utils.LabelManaged,
							fmt.Sprintf("--http-listen=:%d", agentPort),
						},
						Env:            []corev1.EnvVar{{Name: "WATCHER_KEY_FILE", Value: keyMountDir + "/key.json"}},
						VolumeMounts:   []corev1.VolumeMount{{Name: "betorun-key", MountPath: keyMountDir, ReadOnly: true}},
						Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: agentPort, Protocol: corev1.ProtocolTCP}},
						LivenessProbe:  probe("/healthz", 10),
						ReadinessProbe: probe("/readyz", 5),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
						},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, RunAsNonRoot: &yes,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					}},
					Volumes: []corev1.Volume{{Name: "betorun-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: domain.AgentKeySecret, DefaultMode: &mode,
						Items: []corev1.KeyToPath{{Key: "key.json", Path: "key.json"}}}}}},
				},
			},
		},
	}
}

func (i *Installer) namespace(ctx context.Context) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: domain.AgentNamespace, Labels: map[string]string{managedByLabel: managedBy}}}
	if _, err := i.Client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("namespace: %w", denied(err))
	}
	return nil
}

func (i *Installer) serviceAccount(ctx context.Context, name string, mount bool) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: domain.AgentNamespace, Labels: labels(name)},
		AutomountServiceAccountToken: &mount}
	if _, err := i.Client.CoreV1().ServiceAccounts(domain.AgentNamespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("service account %s: %w", name, denied(err))
	}
	return nil
}

// clusterRole : rôle et liaison au compte du même nom.
func (i *Installer) clusterRole(ctx context.Context, name string, rules []rbacv1.PolicyRule) error {
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels(name)}, Rules: rules}
	if err := upsert(ctx, i.Client.RbacV1().ClusterRoles(), role, func(cur *rbacv1.ClusterRole) {
		cur.Rules, cur.Labels = rules, role.Labels
	}); err != nil {
		return admin(fmt.Errorf("cluster role %s: %w", name, err))
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels(name)},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: domain.AgentNamespace}}}
	if err := upsert(ctx, i.Client.RbacV1().ClusterRoleBindings(), binding, func(cur *rbacv1.ClusterRoleBinding) {
		cur.Subjects, cur.Labels = binding.Subjects, binding.Labels
	}); err != nil {
		return admin(fmt.Errorf("cluster role binding %s: %w", name, err))
	}
	return nil
}

// admin : un refus de droits pendant la préparation veut dire « pas administrateur ».
func admin(err error) error {
	if errors.Is(err, domain.ErrAccessDenied) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return fmt.Errorf("%w (%v)", domain.ErrAdminRequired, err)
	}
	return err
}

type object interface{ metav1.Object }

type client[T object] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Create(ctx context.Context, obj T, opts metav1.CreateOptions) (T, error)
	Update(ctx context.Context, obj T, opts metav1.UpdateOptions) (T, error)
}

// upsert : crée, ou met à jour l'existant avec apply.
func upsert[T object](ctx context.Context, c client[T], obj T, apply func(T)) error {
	cur, err := c.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.Create(ctx, obj, metav1.CreateOptions{})
		return denied(err)
	}
	if err != nil {
		return denied(err)
	}
	apply(cur)
	_, err = c.Update(ctx, cur, metav1.UpdateOptions{})
	return denied(err)
}
