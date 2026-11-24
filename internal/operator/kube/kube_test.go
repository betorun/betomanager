package kube

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

var t0 = time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)

func meta(ns, name string, ago time.Duration) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(t0.Add(-ago))}
}

func fixture() *Clusters {
	two := int32(2)
	cs := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: meta("", "n1", 30*24*time.Hour), Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.31.2+k3s1"}}},
		&corev1.Node{ObjectMeta: meta("", "n2", time.Hour), Spec: corev1.NodeSpec{Unschedulable: true}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}, {Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}}}},
		&corev1.Pod{ObjectMeta: meta("km-web", "web-1", 2*time.Hour), Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}}},
		&corev1.Pod{ObjectMeta: meta("km-api", "api-1", time.Hour), Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "api"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api", Ready: true}}}},
		&corev1.Event{ObjectMeta: meta("km-web", "e1", time.Hour), Type: corev1.EventTypeWarning, Reason: "BackOff", Count: 12,
			LastTimestamp: metav1.NewTime(t0.Add(-time.Minute)), Message: "Back-off restarting\n failed container",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "km-web", Name: "web-1"}},
		&corev1.Event{ObjectMeta: meta("km-web", "e2", time.Hour), Type: corev1.EventTypeNormal, Reason: "Pulled",
			LastTimestamp: metav1.NewTime(t0.Add(-2 * time.Minute)), InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "km-web", Name: "web-1"}},
		&appsv1.Deployment{ObjectMeta: meta("km-web", "web", 24*time.Hour), Spec: appsv1.DeploymentSpec{Replicas: &two},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 2, AvailableReplicas: 1, Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable", Message: "Deployment does not have minimum availability."}}}},
	)
	c := New(map[string]kubernetes.Interface{"prod-1": cs})
	c.now = func() time.Time { return t0 }
	return c
}

func run(t *testing.T, c *Clusters, name, args string) (string, bool) {
	t.Helper()
	r := c.Run(context.Background(), name, json.RawMessage(args))
	return r.Content, r.IsError
}

func TestTools(t *testing.T) {
	c := fixture()
	if len(c.Definitions()) != 6 || !Has("k8s_pod_logs") || Has("k8s_secrets") {
		t.Fatal("catalogue")
	}
	for _, d := range c.Definitions() {
		var s map[string]any
		if err := json.Unmarshal(d.Parameters, &s); err != nil || s["type"] != "object" {
			t.Fatalf("schéma de %s : %v", d.Name, err)
		}
	}
	checks := []struct {
		tool, args string
		want       []string
	}{
		{"k8s_clusters", `{}`, []string{"prod-1\t1/2"}},
		{"k8s_nodes", `{"cluster":"prod-1"}`, []string{"n1\ttrue\tfalse\t-\tv1.31.2+k3s1\t30d", "n2\tfalse\ttrue\tMemoryPressure"}},
		{"k8s_pods", `{"cluster":"prod-1","problems_only":true}`, []string{"km-web\tweb-1\tRunning\t0/1\t3\tCrashLoopBackOff (dernier arrêt : OOMKilled)\tn1\t2h"}},
		{"k8s_events", `{"cluster":"prod-1","namespace":"km-web"}`, []string{"1m\tWarning\tBackOff\tkm-web/pod/web-1\t12\tBack-off restarting failed container"}},
		{"k8s_deployments", `{"cluster":"prod-1"}`, []string{"km-web\tweb\t1/2\t2\t1\t24h\tAvailable=False (MinimumReplicasUnavailable)"}},
	}
	for _, ck := range checks {
		out, isErr := run(t, c, ck.tool, ck.args)
		if isErr {
			t.Fatalf("%s : %s", ck.tool, out)
		}
		for _, w := range ck.want {
			if !strings.Contains(out, w) {
				t.Fatalf("%s : %q absent de\n%s", ck.tool, w, out)
			}
		}
	}
	if out, _ := run(t, c, "k8s_pods", `{"cluster":"prod-1","problems_only":true}`); strings.Contains(out, "api-1") {
		t.Fatal("pod sain listé avec problems_only")
	}
	if out, _ := run(t, c, "k8s_events", `{"cluster":"prod-1","namespace":"km-web"}`); strings.Contains(out, "Pulled") {
		t.Fatal("événement normal listé sans all")
	}
	if out, _ := run(t, c, "k8s_events", `{"cluster":"prod-1","all":true,"object":"web-1"}`); !strings.Contains(out, "Pulled") {
		t.Fatal("all=true ignoré")
	}
	if out, _ := run(t, c, "k8s_pod_logs", `{"cluster":"prod-1","namespace":"km-web","pod":"web-1","lines":5000}`); out == "" {
		t.Fatal("journal")
	}
	errs := map[string]string{
		"k8s_nodes":    `{"cluster":"inconnu"}`,
		"k8s_pods":     `{"cluster":1}`,
		"k8s_pod_logs": `{"cluster":"prod-1"}`,
		"k8s_secrets":  `{}`,
	}
	for tool, args := range errs {
		if out, isErr := run(t, c, tool, args); !isErr {
			t.Fatalf("%s %s : erreur attendue, reçu %q", tool, args, out)
		}
	}
	if out, _ := run(t, c, "k8s_nodes", `{"cluster":"inconnu"}`); !strings.Contains(out, "prod-1") {
		t.Fatal("l'erreur doit lister les clusters connus")
	}
}

func TestCut(t *testing.T) {
	if got := cut("ab\ncdé", 6); got != "ab" {
		t.Fatalf("%q", got)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	cfg := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:6443"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`
	for _, f := range []string{"prod-1.yaml", "staging.kubeconfig"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignoré"), 0o600)
	c, err := LoadDir(dir)
	if err != nil || strings.Join(c.Names(), ",") != "prod-1,staging" {
		t.Fatalf("%v %v", c, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "Prod_2.yaml"), []byte(cfg), 0o600)
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("nom invalide accepté")
	}
	if _, err := LoadDir(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("dossier absent accepté")
	}
}
