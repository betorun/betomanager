package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/thekrauss/kubemanager/internal/operator/fanout"
)

type clusterArgs struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
}

func clustersTool(ctx context.Context, c *Clusters, _ json.RawMessage) (string, error) {
	if len(c.names) == 0 {
		return "aucun cluster configuré", nil
	}
	var b strings.Builder
	b.WriteString("CLUSTER\tNŒUDS PRÊTS\n")
	// Tous les clusters à la fois, chacun avec son délai.
	lines := fanout.Each(ctx, c.names, targetTimeout, func(ctx context.Context, n string) string {
		nodes, err := c.clients[n].CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Sprintf("%s\tinjoignable : %v\n", n, err)
		}
		ready := 0
		for _, node := range nodes.Items {
			if nodeReady(node) {
				ready++
			}
		}
		return fmt.Sprintf("%s\t%d/%d\n", n, ready, len(nodes.Items))
	})
	for _, l := range lines {
		b.WriteString(l)
	}
	return b.String(), nil
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodesTool(ctx context.Context, c *Clusters, raw json.RawMessage) (string, error) {
	var a clusterArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	cs, err := c.client(a.Cluster)
	if err != nil {
		return "", err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	now := c.now()
	var b strings.Builder
	b.WriteString("NŒUD\tPRÊT\tISOLÉ\tPRESSIONS\tVERSION\tÂGE\n")
	for _, n := range nodes.Items {
		var pressures []string
		for _, cond := range n.Status.Conditions {
			if cond.Type != corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				pressures = append(pressures, string(cond.Type))
			}
		}
		fmt.Fprintf(&b, "%s\t%t\t%t\t%s\t%s\t%s\n", n.Name, nodeReady(n), n.Spec.Unschedulable,
			dash(strings.Join(pressures, ",")), n.Status.NodeInfo.KubeletVersion, age(now, n.CreationTimestamp.Time))
	}
	return b.String(), nil
}

type podsArgs struct {
	clusterArgs
	ProblemsOnly bool `json:"problems_only"`
}

func podsTool(ctx context.Context, c *Clusters, raw json.RawMessage) (string, error) {
	var a podsArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	cs, err := c.client(a.Cluster)
	if err != nil {
		return "", err
	}
	pods, err := cs.CoreV1().Pods(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	now := c.now()
	var b strings.Builder
	b.WriteString("NAMESPACE\tPOD\tPHASE\tPRÊTS\tREDÉMARRAGES\tRAISON\tNŒUD\tÂGE\n")
	rows, skipped := 0, 0
	for _, p := range pods.Items {
		ready, total, restarts, reason := podState(p)
		problem := p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodUnknown ||
			(p.Status.Phase == corev1.PodRunning && ready < total) || reason != ""
		if a.ProblemsOnly && !problem {
			continue
		}
		if rows >= maxRows {
			skipped++
			continue
		}
		rows++
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d/%d\t%d\t%s\t%s\t%s\n", p.Namespace, p.Name, p.Status.Phase, ready, total, restarts,
			dash(reason), dash(p.Spec.NodeName), age(now, p.CreationTimestamp.Time))
	}
	if rows == 0 {
		return "aucun pod" + map[bool]string{true: " en difficulté", false: ""}[a.ProblemsOnly], nil
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "… %d pods de plus non affichés : préciser le namespace\n", skipped)
	}
	return b.String(), nil
}

// podState : conteneurs prêts, total, redémarrages, et la raison la plus
// parlante (attente en cours, sinon dernière fin anormale).
func podState(p corev1.Pod) (ready, total int, restarts int32, reason string) {
	total = len(p.Spec.Containers)
	var last string
	for _, s := range p.Status.ContainerStatuses {
		if s.Ready {
			ready++
		}
		restarts += s.RestartCount
		if w := s.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			reason = w.Reason
		}
		if t := s.LastTerminationState.Terminated; t != nil && t.Reason != "" && t.Reason != "Completed" {
			last = "dernier arrêt : " + t.Reason
		}
		if t := s.State.Terminated; t != nil && t.Reason != "" && t.Reason != "Completed" && reason == "" {
			reason = t.Reason
		}
	}
	for _, s := range p.Status.InitContainerStatuses {
		if w := s.State.Waiting; w != nil && w.Reason != "" && reason == "" && w.Reason != "PodInitializing" {
			reason = "init : " + w.Reason
		}
	}
	if reason == "" && p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	switch {
	case reason == "":
		reason = last
	case last != "" && !strings.Contains(reason, "dernier arrêt"):
		// CrashLoopBackOff seul ne dit pas pourquoi : la fin précédente
		// (OOMKilled, Error…) est l'indice.
		reason += " (" + last + ")"
	}
	return
}

type eventsArgs struct {
	clusterArgs
	Object string `json:"object"`
	All    bool   `json:"all"`
}

func eventsTool(ctx context.Context, c *Clusters, raw json.RawMessage) (string, error) {
	var a eventsArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	cs, err := c.client(a.Cluster)
	if err != nil {
		return "", err
	}
	list, err := cs.CoreV1().Events(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	evs := list.Items[:0]
	for _, e := range list.Items {
		if (!a.All && e.Type != corev1.EventTypeWarning) || (a.Object != "" && e.InvolvedObject.Name != a.Object) {
			continue
		}
		evs = append(evs, e)
	}
	sort.Slice(evs, func(i, j int) bool { return lastSeen(evs[i]).After(lastSeen(evs[j])) })
	if len(evs) == 0 {
		return "aucun événement", nil
	}
	now := c.now()
	var b strings.Builder
	b.WriteString("IL Y A\tTYPE\tRAISON\tOBJET\tFOIS\tMESSAGE\n")
	for i, e := range evs {
		if i == 100 {
			fmt.Fprintf(&b, "… %d événements plus anciens non affichés\n", len(evs)-i)
			break
		}
		count := e.Count
		if count == 0 {
			count = 1
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s/%s/%s\t%d\t%s\n", age(now, lastSeen(e)), e.Type, e.Reason,
			e.InvolvedObject.Namespace, strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name, count, oneLine(e.Message))
	}
	return b.String(), nil
}

func lastSeen(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

type logsArgs struct {
	clusterArgs
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Previous  bool   `json:"previous"`
	Lines     int64  `json:"lines"`
}

func logsTool(ctx context.Context, c *Clusters, raw json.RawMessage) (string, error) {
	var a logsArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if a.Namespace == "" || a.Pod == "" {
		return "", fmt.Errorf("namespace et pod sont obligatoires")
	}
	cs, err := c.client(a.Cluster)
	if err != nil {
		return "", err
	}
	if a.Lines <= 0 {
		a.Lines = defaultLines
	}
	a.Lines = min(a.Lines, maxLogLines)
	limit := int64(maxLogBytes)
	rc, err := cs.CoreV1().Pods(a.Namespace).GetLogs(a.Pod, &corev1.PodLogOptions{
		Container: a.Container, Previous: a.Previous, TailLines: &a.Lines, LimitBytes: &limit,
	}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	out, err := io.ReadAll(io.LimitReader(rc, maxLogBytes))
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "journal vide", nil
	}
	return strings.ToValidUTF8(string(out), "�"), nil
}

func deploymentsTool(ctx context.Context, c *Clusters, raw json.RawMessage) (string, error) {
	var a clusterArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	cs, err := c.client(a.Cluster)
	if err != nil {
		return "", err
	}
	list, err := cs.AppsV1().Deployments(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "aucun déploiement", nil
	}
	now := c.now()
	var b strings.Builder
	b.WriteString("NAMESPACE\tDÉPLOIEMENT\tPRÊTS\tÀ JOUR\tDISPONIBLES\tÂGE\tPROBLÈME\n")
	for i, d := range list.Items {
		if i == maxRows {
			fmt.Fprintf(&b, "… %d déploiements de plus : préciser le namespace\n", len(list.Items)-i)
			break
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		fmt.Fprintf(&b, "%s\t%s\t%d/%d\t%d\t%d\t%s\t%s\n", d.Namespace, d.Name, d.Status.ReadyReplicas, want,
			d.Status.UpdatedReplicas, d.Status.AvailableReplicas, age(now, d.CreationTimestamp.Time), dash(deployProblem(d)))
	}
	return b.String(), nil
}

func deployProblem(d appsv1.Deployment) string {
	var out []string
	for _, c := range d.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			out = append(out, fmt.Sprintf("%s=%s (%s) %s", c.Type, c.Status, c.Reason, oneLine(c.Message)))
		}
	}
	return strings.Join(out, " ; ")
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "…"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
