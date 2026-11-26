// Package tunnel : mise en œuvre « tunnel » du VPC (ADR 0019, § 4), pour un
// fournisseur de VPS sans réseau privé. Une passerelle par projet, dans
// l'espace du projet : les VPS s'y relient (clé générée sur le VPS), elle
// relaie leur trafic vers les conteneurs et bases du projet, sous la
// politique réseau du projet. Jamais nommée au client.
//
// Ressources posées dans l'espace du projet (toutes « vpc-gateway ») :
// Secret (clé privée de la passerelle, générée ici, jamais rendue),
// ConfigMap (plage, pairs : clés publiques et adresses), Deployment,
// Service NodePort (UDP, source préservée) et NetworkPolicy (UDP admis
// des seules IP publiques des VPS rattachés).
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
	"github.com/thekrauss/kubemanager/internal/vpcgateway"
)

// Name : identifiant de la mise en œuvre (interne).
const Name = "tunnel"

// ResourceName : nom de toutes les ressources de la passerelle (et valeur de
// son étiquette). Plus long que le nom d'un conteneur du client (30 au plus,
// nom de sa release) : jamais en collision avec ses ressources ni ses pods.
const ResourceName = "betorun-private-network-gateway"

// Keepalive : relance du tunnel par le VPS (traverse les NAT, garde l'état
// de connexion du filtrage de la passerelle).
const Keepalive = 25

// specVersion : version de la spécification du Deployment, à monter à
// chaque changement de deployment() pour qu'il soit réappliqué.
const (
	specAnnotation = "betorun.dev/gateway-spec"
	specVersion    = "1"
)

// ConnectedWithin : poignée de main plus récente : VPS relié.
const ConnectedWithin = 3 * time.Minute

// DefaultRoutes : plages des pods et services des clusters k3s.
var DefaultRoutes = []string{"10.42.0.0/16", "10.43.0.0/16"}

// Backend : VPCBackend « tunnel ».
type Backend struct {
	// Kube : client du cluster du VPC.
	Kube func(ctx context.Context, clusterID string) (kubernetes.Interface, error)
	// Namespace : espace Kubernetes du projet.
	Namespace func(ctx context.Context, project uuid.UUID) (string, error)
	// Endpoint : adresse publique (IPv4 ou nom) du point d'entrée du cluster.
	Endpoint func(ctx context.Context, clusterID string) (string, error)
	// Image : image de la passerelle (cmd/vpc-gateway), figée.
	Image string
	// Routes : plages joignables depuis les VPS (défaut DefaultRoutes).
	Routes []string
	// VPSProviders : hébergeurs de VPS servis (défaut : ovh).
	VPSProviders []string
	// PeerStatus : état des pairs vu par la passerelle (défaut : GET /status
	// du pod par le mandataire de l'API Kubernetes).
	PeerStatus func(ctx context.Context, kube kubernetes.Interface, ns string) ([]vpcgateway.PeerStatus, error)
	Now        func() time.Time
}

var _ domain.VPCBackend = (*Backend)(nil)

func (b *Backend) Name() string { return Name }

func (b *Backend) Handles(p domain.Placement) bool {
	providers := b.VPSProviders
	if len(providers) == 0 {
		providers = []string{"ovh"}
	}
	for _, v := range providers {
		if strings.EqualFold(v, p.VPSProvider) {
			return b.Image != ""
		}
	}
	return false
}

func (b *Backend) Capabilities() domain.Capabilities {
	return domain.Capabilities{NeedsEnrollment: true}
}

func (b *Backend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

func (b *Backend) routes() []string {
	if len(b.Routes) > 0 {
		return b.Routes
	}
	return DefaultRoutes
}

// target : client et espace du VPC.
func (b *Backend) target(ctx context.Context, v domain.VPC) (kubernetes.Interface, string, error) {
	kube, err := b.Kube(ctx, v.ClusterID)
	if err != nil {
		return nil, "", err
	}
	ns, err := b.Namespace(ctx, v.ProjectID)
	if err != nil {
		return nil, "", err
	}
	if ns == "" {
		return nil, "", errors.New("project has no namespace")
	}
	return kube, ns, nil
}

func labels(v domain.VPC) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       ResourceName,
		"app.kubernetes.io/managed-by": "betorun",
		// Membre du projet : les NetworkPolicy du projet le laissent joindre
		// les conteneurs et bases (et rien d'un autre projet).
		"app.kubernetes.io/part-of": "kubemanager",
		"km-project-id":             v.ProjectID.String(),
	}
}

func selector() map[string]string { return map[string]string{"app.kubernetes.io/name": ResourceName} }

// ── Configuration (ConfigMap) ──────────────────────────────────────────

func (b *Backend) readConfig(ctx context.Context, kube kubernetes.Interface, ns string) (*vpcgateway.Config, *corev1.ConfigMap, error) {
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	var c vpcgateway.Config
	if raw := cm.Data[vpcgateway.ConfigKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, nil, err
		}
	}
	return &c, cm, nil
}

// edit : lecture, modification et écriture du ConfigMap, rejouées en cas
// d'écriture concurrente (deux inscriptions en même temps).
func (b *Backend) edit(ctx context.Context, kube kubernetes.Interface, ns string, v domain.VPC, f func(c *vpcgateway.Config) error) (*vpcgateway.Config, error) {
	var out *vpcgateway.Config
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c, cm, err := b.readConfig(ctx, kube, ns)
		if err != nil {
			return err
		}
		c.CIDR, c.Gateway, c.Routes = v.CIDR.Masked().String(), v.Gateway.String(), b.routes()
		if err := f(c); err != nil {
			return err
		}
		sort.Slice(c.Peers, func(i, j int) bool { return c.Peers[i].Address < c.Peers[j].Address })
		if err := c.Validate(); err != nil {
			return err
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		out = c
		if cm.Data[vpcgateway.ConfigKey] == string(raw) {
			return nil // rien de changé : pas d'écriture
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[vpcgateway.ConfigKey] = string(raw)
		if _, err := kube.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, b.applyPolicy(ctx, kube, ns, v, out)
}

func peerOf(m domain.Member) vpcgateway.Peer {
	return vpcgateway.Peer{VPSID: m.VPSID.String(), Name: m.Name, Address: m.Address.String(), PublicKey: m.PublicKey, PublicIPv4: m.PublicIPv4}
}

func upsert(c *vpcgateway.Config, p vpcgateway.Peer) {
	for i := range c.Peers {
		if c.Peers[i].VPSID == p.VPSID {
			c.Peers[i] = p
			return
		}
	}
	c.Peers = append(c.Peers, p)
}

// ── Contrat ────────────────────────────────────────────────────────────

// Ensure : passerelle posée (idempotent) ; ces membres y sont à jour (les
// autres ne sont pas touchés : un retrait passe par Detach).
func (b *Backend) Ensure(ctx context.Context, v domain.VPC, members []domain.Member) error {
	kube, ns, err := b.target(ctx, v)
	if err != nil {
		return err
	}
	if err := b.ensureObjects(ctx, kube, ns, v); err != nil {
		return err
	}
	_, err = b.edit(ctx, kube, ns, v, func(c *vpcgateway.Config) error {
		for _, m := range members {
			p := peerOf(m)
			for _, old := range c.Peers {
				if old.VPSID == p.VPSID && p.PublicIPv4 == "" {
					p.PublicIPv4 = old.PublicIPv4 // relevée à l'inscription, pas encore chez le fournisseur
				}
			}
			upsert(c, p)
		}
		return nil
	})
	return err
}

// Attach : le VPS a sa place (adresse) ; il deviendra pair à son inscription.
func (b *Backend) Attach(ctx context.Context, v domain.VPC, m domain.Member) (*domain.AttachPlan, error) {
	kube, ns, err := b.target(ctx, v)
	if err != nil {
		return nil, err
	}
	if err := b.ensureObjects(ctx, kube, ns, v); err != nil {
		return nil, err
	}
	if _, err := b.edit(ctx, kube, ns, v, func(c *vpcgateway.Config) error {
		p := peerOf(m)
		for _, old := range c.Peers {
			if old.VPSID == p.VPSID && p.PublicKey == "" {
				p.PublicKey = old.PublicKey // déjà inscrit : rien ne change
			}
		}
		upsert(c, p)
		return nil
	}); err != nil {
		return nil, err
	}
	return &domain.AttachPlan{NeedsEnrollment: true}, nil
}

// Enroll : clé publique du VPS enregistrée ; configuration rendue (point
// d'entrée, clé publique de la passerelle, plages), sans aucun secret.
func (b *Backend) Enroll(ctx context.Context, req domain.EnrollRequest) (*domain.EnrollResult, error) {
	if !vpcgateway.ValidKey(req.PublicKey) {
		return nil, domain.ErrInvalidPublicKey
	}
	kube, ns, err := b.target(ctx, req.VPC)
	if err != nil {
		return nil, err
	}
	found := false
	if _, err := b.edit(ctx, kube, ns, req.VPC, func(c *vpcgateway.Config) error {
		for i := range c.Peers {
			if c.Peers[i].VPSID == req.Member.VPSID.String() {
				found = true
				p := peerOf(req.Member)
				p.PublicKey = req.PublicKey
				c.Peers[i] = p
			}
		}
		if !found {
			return domain.ErrNotAttached
		}
		return nil
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, domain.ErrNotAttached
		}
		return nil, err
	}
	pub, err := b.publicKey(ctx, kube, ns)
	if err != nil {
		return nil, err
	}
	port, err := b.nodePort(ctx, kube, ns)
	if err != nil {
		return nil, err
	}
	host, err := b.Endpoint(ctx, req.VPC.ClusterID)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, domain.ErrUnavailable
	}
	allowed := append([]string{req.VPC.CIDR.Masked().String()}, b.routes()...)
	return &domain.EnrollResult{Config: map[string]string{
		"endpoint":        hostPort(host, port),
		"peer_public_key": pub,
		"allowed_ips":     strings.Join(allowed, ","),
		"keepalive":       strconv.Itoa(Keepalive),
	}}, nil
}

func hostPort(host string, port int32) string {
	if a, err := netip.ParseAddr(host); err == nil && a.Is6() {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Detach : le VPS n'est plus pair, tout de suite (même injoignable).
func (b *Backend) Detach(ctx context.Context, v domain.VPC, id uuid.UUID) error {
	kube, ns, err := b.target(ctx, v)
	if err != nil {
		return err
	}
	_, err = b.edit(ctx, kube, ns, v, func(c *vpcgateway.Config) error {
		kept := c.Peers[:0]
		for _, p := range c.Peers {
			if p.VPSID != id.String() {
				kept = append(kept, p)
			}
		}
		c.Peers = kept
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil // pas de passerelle : rien à retirer
	}
	return err
}

// Status : membres connus de la passerelle ; relié si la dernière poignée
// de main a moins de ConnectedWithin.
func (b *Backend) Status(ctx context.Context, v domain.VPC) ([]domain.MemberStatus, error) {
	kube, ns, err := b.target(ctx, v)
	if err != nil {
		return nil, err
	}
	c, _, err := b.readConfig(ctx, kube, ns)
	if apierrors.IsNotFound(err) {
		return []domain.MemberStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	live := map[string]vpcgateway.PeerStatus{}
	if len(c.Peers) > 0 {
		fetch := b.PeerStatus
		if fetch == nil {
			fetch = podStatus
		}
		list, err := fetch(ctx, kube, ns)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			live[s.PublicKey] = s
		}
	}
	now := b.now()
	out := make([]domain.MemberStatus, 0, len(c.Peers))
	for _, p := range c.Peers {
		id, err := uuid.Parse(p.VPSID)
		if err != nil {
			continue
		}
		st := domain.MemberStatus{VPSID: id}
		if s, ok := live[p.PublicKey]; ok && p.PublicKey != "" && s.LatestHandshake > 0 {
			at := time.Unix(s.LatestHandshake, 0).UTC()
			st.LastContact = &at
			st.Connected = now.Sub(at) < ConnectedWithin
		}
		out = append(out, st)
	}
	return out, nil
}

// Teardown : passerelle et sa configuration retirées (idempotent).
func (b *Backend) Teardown(ctx context.Context, v domain.VPC) error {
	kube, ns, err := b.target(ctx, v)
	if err != nil {
		return err
	}
	del := metav1.DeleteOptions{}
	errs := []error{
		kube.AppsV1().Deployments(ns).Delete(ctx, ResourceName, del),
		kube.CoreV1().Services(ns).Delete(ctx, ResourceName, del),
		kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, ResourceName, del),
		kube.CoreV1().ConfigMaps(ns).Delete(ctx, ResourceName, del),
		kube.CoreV1().Secrets(ns).Delete(ctx, ResourceName, del),
	}
	for _, e := range errs {
		if e != nil && !apierrors.IsNotFound(e) {
			return e
		}
	}
	return nil
}

// ── Ressources ─────────────────────────────────────────────────────────

func (b *Backend) publicKey(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	s, err := kube.CoreV1().Secrets(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return vpcgateway.PublicKey(strings.TrimSpace(string(s.Data[vpcgateway.PrivateKeyKey])))
}

func (b *Backend) nodePort(ctx context.Context, kube kubernetes.Interface, ns string) (int32, error) {
	s, err := kube.CoreV1().Services(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	for _, p := range s.Spec.Ports {
		if p.NodePort > 0 {
			return p.NodePort, nil
		}
	}
	return 0, errors.New("gateway port not allocated yet")
}

// ensureObjects : Secret, ConfigMap, Deployment, Service et NetworkPolicy.
func (b *Backend) ensureObjects(ctx context.Context, kube kubernetes.Interface, ns string, v domain.VPC) error {
	lbl := labels(v)
	if sec, err := kube.CoreV1().Secrets(ns).Get(ctx, ResourceName, metav1.GetOptions{}); err == nil && len(sec.Data[vpcgateway.StatusTokenKey]) == 0 {
		// Secret plus ancien que le jeton d'état : complété, clé gardée.
		token, err := vpcgateway.NewStatusToken()
		if err != nil {
			return err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[vpcgateway.StatusTokenKey] = []byte(token)
		if _, err := kube.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return err
		}
	} else if apierrors.IsNotFound(err) {
		priv, _, err := vpcgateway.GenerateKey()
		if err != nil {
			return err
		}
		token, err := vpcgateway.NewStatusToken()
		if err != nil {
			return err
		}
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ResourceName, Namespace: ns, Labels: lbl},
			Type: corev1.SecretTypeOpaque, Data: map[string][]byte{
				vpcgateway.PrivateKeyKey: []byte(priv), vpcgateway.StatusTokenKey: []byte(token)}}
		if _, err := kube.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, ResourceName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		raw, err := json.Marshal(vpcgateway.Config{CIDR: v.CIDR.Masked().String(), Gateway: v.Gateway.String(), Routes: b.routes(), Peers: []vpcgateway.Peer{}})
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ResourceName, Namespace: ns, Labels: lbl},
			Data: map[string]string{vpcgateway.ConfigKey: string(raw)}}
		if _, err := kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := b.applyDeployment(ctx, kube, ns, v); err != nil {
		return err
	}
	return b.applyService(ctx, kube, ns, v)
}

func (b *Backend) applyDeployment(ctx context.Context, kube kubernetes.Interface, ns string, v domain.VPC) error {
	d := b.deployment(ns, v)
	api := kube.AppsV1().Deployments(ns)
	cur, err := api.Get(ctx, ResourceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, d, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if len(cur.Spec.Template.Spec.Containers) == 1 && cur.Spec.Template.Spec.Containers[0].Image == b.Image &&
		cur.Annotations[specAnnotation] == specVersion && equality.Semantic.DeepEqual(cur.Labels, d.Labels) {
		return nil // à jour (le cluster complète la spécification : comparée par sa version)
	}
	cur.Labels, cur.Annotations, cur.Spec = d.Labels, d.Annotations, d.Spec
	_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

func (b *Backend) deployment(ns string, v domain.VPC) *appsv1.Deployment {
	lbl := labels(v)
	one := int32(1)
	no, yes := false, true
	root := int64(0)
	keyMode := int32(0o400)
	mem := resource.MustParse("1Mi")
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ResourceName, Namespace: ns, Labels: lbl,
			Annotations: map[string]string{specAnnotation: specVersion}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: selector()},
			// Jamais deux passerelles avec la même clé.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &no,
					EnableServiceLinks:           &no,
					SecurityContext:              &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name:  "gateway",
						Image: b.Image,
						Ports: []corev1.ContainerPort{
							{Name: "tunnel", ContainerPort: vpcgateway.ListenPort, Protocol: corev1.ProtocolUDP},
							{Name: "status", ContainerPort: vpcgateway.StatusPort, Protocol: corev1.ProtocolTCP},
						},
						// root dans le conteneur, seulement NET_ADMIN, dans
						// l'espace réseau du pod.
						SecurityContext: &corev1.SecurityContext{
							RunAsUser: &root, RunAsNonRoot: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"NET_ADMIN"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz", Port: intstr.FromInt32(vpcgateway.StatusPort)}}, PeriodSeconds: 10, TimeoutSeconds: 3},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: vpcgateway.ConfigDir, ReadOnly: true},
							{Name: "key", MountPath: vpcgateway.KeyDir, ReadOnly: true},
							{Name: "run", MountPath: "/run"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: ResourceName}}}},
						{Name: "key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: ResourceName, DefaultMode: &keyMode}}},
						{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium: corev1.StorageMediumMemory, SizeLimit: &mem}}},
					},
				},
			},
		},
	}
}

// applyService : port UDP alloué par le cluster (NodePort, unique), source
// préservée (Local) pour que la NetworkPolicy filtre sur l'IP du VPS.
func (b *Backend) applyService(ctx context.Context, kube kubernetes.Interface, ns string, v domain.VPC) error {
	api := kube.CoreV1().Services(ns)
	_, err := api.Get(ctx, ResourceName, metav1.GetOptions{})
	if err == nil {
		return nil // port déjà alloué : gardé (les VPS l'ont en configuration)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ResourceName, Namespace: ns, Labels: labels(v)},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeNodePort,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
			Selector:              selector(),
			Ports: []corev1.ServicePort{{Name: "tunnel", Protocol: corev1.ProtocolUDP,
				Port: vpcgateway.ListenPort, TargetPort: intstr.FromInt32(vpcgateway.ListenPort)}},
		},
	}
	_, err = api.Create(ctx, s, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// applyPolicy : entrée UDP des seules IP publiques des VPS rattachés (aucune :
// fermée) ; état lisible par l'API ; sortie vers l'espace du projet et le DNS.
func (b *Backend) applyPolicy(ctx context.Context, kube kubernetes.Interface, ns string, v domain.VPC, c *vpcgateway.Config) error {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	tunnelPort, statusPort, dns := intstr.FromInt32(vpcgateway.ListenPort), intstr.FromInt32(vpcgateway.StatusPort), intstr.FromInt32(53)
	var sources []networkingv1.NetworkPolicyPeer
	seen := map[string]bool{}
	for _, p := range c.Peers {
		a, err := netip.ParseAddr(p.PublicIPv4)
		if err != nil || !a.Is4() || seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		sources = append(sources, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: a.String() + "/32"}})
	}
	ingress := []networkingv1.NetworkPolicyIngressRule{
		// État des pairs : lu par l'API via le mandataire de l'API Kubernetes,
		// avec le jeton du Secret (le port est joignable du cluster).
		{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &statusPort}}},
	}
	if len(sources) > 0 {
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{From: sources,
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &tunnelPort}}})
	}
	egress := []networkingv1.NetworkPolicyEgressRule{
		// Conteneurs, services et bases du projet (même espace), sous leur
		// propre politique.
		{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
		{To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}}},
	}
	if len(sources) > 0 {
		// Réponses et relances du tunnel vers les VPS.
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: sources})
	}
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: ResourceName, Namespace: ns, Labels: labels(v)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector()},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
	api := kube.NetworkingV1().NetworkPolicies(ns)
	cur, err := api.Get(ctx, ResourceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, pol, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(cur.Spec, pol.Spec) && equality.Semantic.DeepEqual(cur.Labels, pol.Labels) {
		return nil
	}
	cur.Labels, cur.Spec = pol.Labels, pol.Spec
	_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// podStatus : GET /status du pod de la passerelle (mandataire de l'API
// Kubernetes : rien d'exposé hors du cluster).
func podStatus(ctx context.Context, kube kubernetes.Interface, ns string) ([]vpcgateway.PeerStatus, error) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, ResourceName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	token := string(sec.Data[vpcgateway.StatusTokenKey])
	if token == "" {
		return nil, errors.New("gateway status token missing")
	}
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + ResourceName})
	if err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		raw, err := kube.CoreV1().Pods(ns).ProxyGet("http", p.Name, strconv.Itoa(vpcgateway.StatusPort), "status",
			map[string]string{"token": token}).DoRaw(ctx)
		if err != nil {
			return nil, err
		}
		var out []vpcgateway.PeerStatus
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, errors.New("gateway not running")
}
