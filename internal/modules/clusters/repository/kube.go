package repository

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/thekrauss/kubemanager/internal/modules/clusters/domain"
)

// Erreurs d'un kubeconfig refusé (déposé dans le back-office).
var (
	ErrKubeconfigInvalid = errors.New("invalid kubeconfig")
	// ErrKubeconfigExec : un greffon « exec » ou « auth-provider » lancerait une
	// commande sur nos serveurs : refusé (jeton ou certificat inclus seulement).
	ErrKubeconfigExec = errors.New("kubeconfig must not use exec or auth-provider plugins: embed a token or a client certificate")
	// ErrKubeconfigFiles : chemins de fichiers locaux (certificats, jeton) :
	// refusés, tout doit être inclus dans le fichier.
	ErrKubeconfigFiles = errors.New("kubeconfig must embed its certificates and token, not reference local files")
	ErrKubeconfigHTTPS = errors.New("kubeconfig server must be an https URL")
	// ErrKubeconfigNoContext : pas de contexte courant (ou son cluster ou son
	// utilisateur manque).
	ErrKubeconfigNoContext = errors.New("kubeconfig has no usable current context")
	// ErrKubeconfigNoCredentials : ni jeton ni certificat pour l'utilisateur.
	ErrKubeconfigNoCredentials = errors.New("kubeconfig has no token or client certificate for the current user")
)

// requestTimeout : chaque appel à l'API d'un cluster.
const requestTimeout = 10 * time.Second

// ParseKubeconfig : configuration du contexte courant, après contrôle :
// aucune commande, aucun fichier local, serveur en https. Rend l'adresse du
// serveur (affichée au back-office, jamais les secrets).
func ParseKubeconfig(raw []byte) (*rest.Config, string, error) {
	if len(raw) == 0 || len(raw) > domain.KubeconfigMax {
		return nil, "", ErrKubeconfigInvalid
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, "", ErrKubeconfigInvalid
	}
	ctxName := cfg.CurrentContext
	if ctxName == "" && len(cfg.Contexts) == 1 {
		for name := range cfg.Contexts {
			ctxName = name
		}
	}
	kctx, ok := cfg.Contexts[ctxName]
	if !ok {
		return nil, "", ErrKubeconfigNoContext
	}
	cl, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return nil, "", ErrKubeconfigNoContext
	}
	user, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return nil, "", ErrKubeconfigNoContext
	}
	// Tout le fichier est contrôlé (pas seulement le contexte courant) : un
	// changement de contexte ne doit jamais rouvrir une commande ou un fichier.
	for _, u := range cfg.AuthInfos {
		if u.Exec != nil || u.AuthProvider != nil {
			return nil, "", ErrKubeconfigExec
		}
		if u.ClientCertificate != "" || u.ClientKey != "" || u.TokenFile != "" {
			return nil, "", ErrKubeconfigFiles
		}
	}
	for _, c := range cfg.Clusters {
		if c.CertificateAuthority != "" {
			return nil, "", ErrKubeconfigFiles
		}
	}
	u, err := url.Parse(cl.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, "", ErrKubeconfigHTTPS
	}
	if user.Token == "" && len(user.ClientCertificateData) == 0 && user.Username == "" {
		return nil, "", ErrKubeconfigNoCredentials
	}
	rc, err := clientcmd.NewNonInteractiveClientConfig(*cfg, ctxName, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, "", ErrKubeconfigInvalid
	}
	rc.Timeout = requestTimeout
	rc.QPS, rc.Burst = 10, 20
	return rc, u.Scheme + "://" + u.Host, nil
}

// Kube : lecture d'un cluster par son API.
type Kube struct{ Client kubernetes.Interface }

// NewKube : client d'un rest.Config.
func NewKube(rc *rest.Config) (*Kube, error) {
	c, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &Kube{Client: c}, nil
}

// podPages : pages de pods lues au plus par relevé (500 par page).
const podPages = 40

// denied : refus de droits (403, 401) rendu en domain.ErrAccessDenied.
func denied(err error) error {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return fmt.Errorf("%w (%v)", domain.ErrAccessDenied, err)
	}
	return err
}

// Snapshot : version, nœuds (allouable et demandes), pods.
func (k *Kube) Snapshot(ctx context.Context) (*domain.Snapshot, error) {
	snap, err := k.snapshot(ctx)
	return snap, denied(err)
}

func (k *Kube) snapshot(ctx context.Context) (*domain.Snapshot, error) {
	v, err := k.Client.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}
	nodes, err := k.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	snap := &domain.Snapshot{Version: v.GitVersion, Nodes: nodes}
	byNode := map[string]int{}
	for i, n := range nodes {
		byNode[n.Name] = i
	}
	cont := ""
	for page := 0; page < podPages; page++ {
		pods, err := k.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return nil, err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if failing(p) {
				snap.PodsFailing++
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			if p.Status.Phase == corev1.PodRunning {
				snap.PodsRunning++
			}
			idx, ok := byNode[p.Spec.NodeName]
			if !ok {
				continue
			}
			cpu, mem := requests(p)
			snap.Nodes[idx].CPUReqMilli += cpu
			snap.Nodes[idx].MemReqBytes += mem
			snap.Nodes[idx].Pods++
		}
		cont = pods.Continue
		if cont == "" {
			break
		}
	}
	return snap, nil
}

// Nodes : nœuds, prêts ou non, avec leur allouable.
func (k *Kube) Nodes(ctx context.Context) ([]domain.Node, error) {
	list, err := k.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, denied(err)
	}
	out := make([]domain.Node, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		node := domain.Node{Name: n.Name, Unschedulable: n.Spec.Unschedulable, KubeletVer: n.Status.NodeInfo.KubeletVersion,
			CPUAllocMilli: n.Status.Allocatable.Cpu().MilliValue(), MemAllocBytes: n.Status.Allocatable.Memory().Value(), Roles: []string{}}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				node.Ready = c.Status == corev1.ConditionTrue
			}
		}
		for l := range n.Labels {
			if r, ok := strings.CutPrefix(l, "node-role.kubernetes.io/"); ok && r != "" {
				node.Roles = append(node.Roles, r)
			}
		}
		sort.Strings(node.Roles)
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// requests : demandes du pod (conteneurs ; conteneurs d'initialisation non comptés).
func requests(p *corev1.Pod) (cpu, mem int64) {
	for _, c := range p.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	return cpu, mem
}

// failing : un conteneur boucle ou ne trouve pas son image (les pods finis
// en échec, tâches ou évictions anciennes, ne comptent pas : ils restent).
func failing(p *corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
				return true
			}
		}
	}
	return false
}
