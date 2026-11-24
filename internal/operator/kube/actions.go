package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/thekrauss/kubemanager/internal/operator/action"
)

// Actions sur les clusters, jouées avec l'identité de dépannage de
// l'Opérateur. Son rôle n'ouvre que ces verbes : patch des déploiements,
// statefulsets et daemonsets (redémarrage, réplicas) et des nœuds ; delete des
// pods. Ni secrets, ni exec, ni création, ni suppression d'un contrôleur.

const (
	kindRestart  = "k8s.workload.restart"
	kindPodDel   = "k8s.pod.delete"
	kindScale    = "k8s.workload.scale"
	kindCordon   = "k8s.node.cordon"
	kindUncordon = "k8s.node.uncordon"

	maxReplicas = 50
	// restartedAt : l'annotation de « kubectl rollout restart ».
	restartedAt = "kubectl.kubernetes.io/restartedAt"
)

var objName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// system : namespaces de la plateforme elle-même ; y agir touche tout le
// cluster (DNS, réseau, ingress) : risque relevé d'un cran, nom retapé.
var system = map[string]bool{"kube-system": true, "kube-public": true, "kube-node-lease": true, "cilium": true, "traefik": true}

type workloadParams struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name"`
	Replicas  *int32 `json:"replicas,omitempty"`
}

type nodeParams struct {
	Cluster string `json:"cluster"`
	Node    string `json:"node"`
}

const (
	pKind = `"kind":{"type":"string","enum":["deployment","statefulset","daemonset"]}`
	pNs   = `"namespace":{"type":"string"}`
	pName = `"name":{"type":"string"}`
)

// Actions : action.Provider.
func (c *Clusters) Actions() []action.Spec {
	if c == nil || len(c.names) == 0 {
		return nil
	}
	return []action.Spec{
		{Kind: kindRestart, Capability: action.CapInfrastructure,
			Description: "Redémarrage progressif d'une charge de travail (comme kubectl rollout restart) : les pods sont recréés un à un.",
			Parameters:  schema(pCluster+","+pNs+","+pKind+","+pName, "cluster", "namespace", "kind", "name")},
		{Kind: kindPodDel, Capability: action.CapInfrastructure,
			Description: "Supprimer un pod géré par un contrôleur (il est recréé aussitôt). Refusé pour un pod sans contrôleur, qui serait perdu.",
			Parameters:  schema(pCluster+","+pNs+","+pName, "cluster", "namespace", "name")},
		{Kind: kindScale, Capability: action.CapInfrastructure,
			Description: fmt.Sprintf("Changer le nombre de réplicas d'un déploiement ou d'un statefulset (0 à %d ; 0 arrête le service).", maxReplicas),
			Parameters: schema(pCluster+","+pNs+`,"kind":{"type":"string","enum":["deployment","statefulset"]},`+pName+
				fmt.Sprintf(`,"replicas":{"type":"integer","minimum":0,"maximum":%d}`, maxReplicas), "cluster", "namespace", "kind", "name", "replicas")},
		{Kind: kindCordon, Capability: action.CapInfrastructure,
			Description: "Isoler un nœud (cordon) : plus aucun nouveau pod n'y est placé ; ceux qui y tournent restent.",
			Parameters:  schema(pCluster+`,"node":{"type":"string"}`, "cluster", "node")},
		{Kind: kindUncordon, Capability: action.CapInfrastructure,
			Description: "Réintégrer un nœud isolé (uncordon).",
			Parameters:  schema(pCluster+`,"node":{"type":"string"}`, "cluster", "node")},
	}
}

func bump(r action.Risk) action.Risk {
	if r == action.RiskLow {
		return action.RiskMedium
	}
	return action.RiskHigh
}

// Prepare : action.Provider. Lit l'objet visé : il doit exister ; le risque
// tient compte de son état (réplicas prêts, namespace système).
func (c *Clusters) Prepare(ctx context.Context, kind string, raw json.RawMessage) (action.Prepared, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	switch kind {
	case kindCordon, kindUncordon:
		return c.prepareNode(ctx, kind, raw)
	case kindRestart, kindPodDel, kindScale:
	default:
		return action.Prepared{}, action.ErrUnknown
	}
	var p workloadParams
	if err := decode(raw, &p); err != nil {
		return action.Prepared{}, err
	}
	if !objName.MatchString(p.Namespace) || !objName.MatchString(p.Name) {
		return action.Prepared{}, fmt.Errorf("%w : namespace et nom attendus", action.ErrRefused)
	}
	cs, err := c.client(p.Cluster)
	if err != nil {
		return action.Prepared{}, err
	}
	where := fmt.Sprintf("%s/%s/%s", p.Cluster, p.Namespace, p.Name)
	var out action.Prepared

	switch kind {
	case kindPodDel:
		p.Kind, p.Replicas = "", nil
		pod, err := cs.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return action.Prepared{}, fmt.Errorf("pod %s : %v", where, err)
		}
		ctrl := metav1.GetControllerOf(pod)
		if ctrl == nil {
			return action.Prepared{}, fmt.Errorf("%w : le pod %s n'a pas de contrôleur : supprimé, il ne reviendrait pas", action.ErrRefused, where)
		}
		ready, total, restarts, reason := podState(*pod)
		out = action.Prepared{Target: where, Risk: action.RiskLow,
			Summary: fmt.Sprintf("Supprimer le pod %s ; son contrôleur (%s %s) en recrée un.", where, ctrl.Kind, ctrl.Name),
			State:   fmt.Sprintf("%d/%d prêts, %d redémarrages, %s, nœud %s", ready, total, restarts, dash(reason), dash(pod.Spec.NodeName))}

	case kindRestart, kindScale:
		if kind == kindRestart {
			p.Replicas = nil
		} else if p.Replicas == nil || *p.Replicas < 0 || *p.Replicas > maxReplicas {
			return action.Prepared{}, fmt.Errorf("%w : replicas de 0 à %d", action.ErrRefused, maxReplicas)
		}
		desired, ready, err := workloadReplicas(ctx, c, p)
		if err != nil {
			return action.Prepared{}, err
		}
		out = action.Prepared{Target: where, State: fmt.Sprintf("%s : %d/%d prêts", p.Kind, ready, desired)}
		if kind == kindRestart {
			out.Summary = fmt.Sprintf("Redémarrage progressif du %s %s.", p.Kind, where)
			out.Risk = action.RiskLow
			if ready < 2 && p.Kind != "daemonset" {
				out.Risk = action.RiskMedium
				out.Summary += " Un seul réplica prêt : le service est coupé le temps du redémarrage."
			}
		} else {
			out.Summary = fmt.Sprintf("Passer le %s %s de %d à %d réplicas.", p.Kind, where, desired, *p.Replicas)
			out.Risk = action.RiskMedium
			if *p.Replicas == 0 {
				out.Risk = action.RiskHigh
				out.Summary += " Le service s'arrête."
			}
		}
	}
	if system[p.Namespace] {
		out.Risk = bump(out.Risk)
		out.Summary += " Namespace système : tout le cluster peut être touché."
	}
	if out.Risk == action.RiskHigh {
		out.Confirm = p.Name
	}
	out.Params, _ = json.Marshal(p)
	return out, nil
}

func workloadReplicas(ctx context.Context, c *Clusters, p workloadParams) (desired, ready int32, err error) {
	cs, err := c.client(p.Cluster)
	if err != nil {
		return 0, 0, err
	}
	apps := cs.AppsV1()
	switch p.Kind {
	case "deployment":
		d, err := apps.Deployments(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return 0, 0, fmt.Errorf("déploiement %s/%s : %v", p.Namespace, p.Name, err)
		}
		return replicas(d.Spec.Replicas), d.Status.ReadyReplicas, nil
	case "statefulset":
		s, err := apps.StatefulSets(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return 0, 0, fmt.Errorf("statefulset %s/%s : %v", p.Namespace, p.Name, err)
		}
		return replicas(s.Spec.Replicas), s.Status.ReadyReplicas, nil
	case "daemonset":
		d, err := apps.DaemonSets(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return 0, 0, fmt.Errorf("daemonset %s/%s : %v", p.Namespace, p.Name, err)
		}
		return d.Status.DesiredNumberScheduled, d.Status.NumberReady, nil
	}
	return 0, 0, fmt.Errorf("%w : kind attendu : deployment, statefulset ou daemonset", action.ErrRefused)
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

func (c *Clusters) prepareNode(ctx context.Context, kind string, raw json.RawMessage) (action.Prepared, error) {
	var p nodeParams
	if err := decode(raw, &p); err != nil {
		return action.Prepared{}, err
	}
	cs, err := c.client(p.Cluster)
	if err != nil {
		return action.Prepared{}, err
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, p.Node, metav1.GetOptions{})
	if err != nil {
		return action.Prepared{}, fmt.Errorf("nœud %s/%s : %v", p.Cluster, p.Node, err)
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return action.Prepared{}, err
	}
	schedulable := 0
	for _, o := range list.Items {
		if !o.Spec.Unschedulable && nodeReady(o) {
			schedulable++
		}
	}
	where := p.Cluster + "/" + p.Node
	state := fmt.Sprintf("prêt=%t, isolé=%t, %d nœud(s) acceptent des pods", nodeReady(*n), n.Spec.Unschedulable, schedulable)
	params, _ := json.Marshal(p)
	if kind == kindUncordon {
		if !n.Spec.Unschedulable {
			return action.Prepared{}, fmt.Errorf("%w : %s n'est pas isolé", action.ErrRefused, where)
		}
		return action.Prepared{Target: where, Params: params, Risk: action.RiskLow, State: state,
			Summary: fmt.Sprintf("Réintégrer le nœud %s : il accepte de nouveau des pods.", where)}, nil
	}
	if n.Spec.Unschedulable {
		return action.Prepared{}, fmt.Errorf("%w : %s est déjà isolé", action.ErrRefused, where)
	}
	out := action.Prepared{Target: where, Params: params, Risk: action.RiskMedium, State: state,
		Summary: fmt.Sprintf("Isoler le nœud %s : plus aucun nouveau pod n'y est placé.", where)}
	if !n.Spec.Unschedulable && nodeReady(*n) && schedulable <= 1 {
		out.Risk, out.Confirm = action.RiskHigh, p.Node
		out.Summary += " C'est le dernier nœud qui accepte des pods : plus rien ne pourra démarrer."
	}
	return out, nil
}

// Execute : action.Provider.
func (c *Clusters) Execute(ctx context.Context, kind string, raw json.RawMessage, a action.Approval) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	switch kind {
	case kindCordon, kindUncordon:
		var p nodeParams
		if err := decode(raw, &p); err != nil {
			return "", err
		}
		cs, err := c.client(p.Cluster)
		if err != nil {
			return "", err
		}
		patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, kind == kindCordon)
		if _, err := cs.CoreV1().Nodes().Patch(ctx, p.Node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
			return "", err
		}
		if kind == kindCordon {
			return fmt.Sprintf("Nœud %s/%s isolé.", p.Cluster, p.Node), nil
		}
		return fmt.Sprintf("Nœud %s/%s réintégré.", p.Cluster, p.Node), nil
	case kindRestart, kindPodDel, kindScale:
	default:
		return "", action.ErrUnknown
	}
	var p workloadParams
	if err := decode(raw, &p); err != nil {
		return "", err
	}
	cs, err := c.client(p.Cluster)
	if err != nil {
		return "", err
	}
	where := fmt.Sprintf("%s/%s/%s", p.Cluster, p.Namespace, p.Name)
	apps := cs.AppsV1()
	opts := metav1.PatchOptions{FieldManager: fieldManager}

	switch kind {
	case kindPodDel:
		if err := cs.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Pod %s supprimé ; son contrôleur le recrée.", where), nil

	case kindRestart:
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{restartedAt: c.now().UTC().Format(time.RFC3339), approvedBy: a.ApprovedBy, cardAnnotation: a.CardID}}}}})
		switch p.Kind {
		case "deployment":
			_, err = apps.Deployments(p.Namespace).Patch(ctx, p.Name, types.StrategicMergePatchType, patch, opts)
		case "statefulset":
			_, err = apps.StatefulSets(p.Namespace).Patch(ctx, p.Name, types.StrategicMergePatchType, patch, opts)
		case "daemonset":
			_, err = apps.DaemonSets(p.Namespace).Patch(ctx, p.Name, types.StrategicMergePatchType, patch, opts)
		default:
			err = fmt.Errorf("kind inconnu : %s", p.Kind)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Redémarrage progressif du %s %s lancé.", p.Kind, where), nil

	case kindScale:
		if p.Replicas == nil {
			return "", fmt.Errorf("replicas absent")
		}
		patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, *p.Replicas)
		switch p.Kind {
		case "deployment":
			_, err = apps.Deployments(p.Namespace).Patch(ctx, p.Name, types.MergePatchType, patch, opts)
		case "statefulset":
			_, err = apps.StatefulSets(p.Namespace).Patch(ctx, p.Name, types.MergePatchType, patch, opts)
		default:
			err = fmt.Errorf("kind inconnu : %s", p.Kind)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s passé à %d réplicas.", p.Kind, where, *p.Replicas), nil
	}
	return "", action.ErrUnknown
}

const (
	fieldManager   = "betorun-operator"
	approvedBy     = "operator.betorun.io/approved-by"
	cardAnnotation = "operator.betorun.io/card"
)
