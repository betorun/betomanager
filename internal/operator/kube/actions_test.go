package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/operator/action"
)

func actionFixture() (*Clusters, *fake.Clientset) {
	one, three := int32(1), int32(3)
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	ctrl := true
	cs := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: ready},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Status: ready},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "km-web", Name: "web-1",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7d9", Controller: &ctrl}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "km-web", Name: "seul"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "km-web", Name: "web"}, Spec: appsv1.DeploymentSpec{Replicas: &three},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 3}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "km-api", Name: "api"}, Spec: appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "coredns"}, Spec: appsv1.DeploymentSpec{Replicas: &three},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 3}},
	)
	c := New(map[string]kubernetes.Interface{"c1": cs})
	c.SetClock(func() time.Time { return t0 })
	return c, cs
}

func TestActionsPrepare(t *testing.T) {
	c, _ := actionFixture()
	ctx := context.Background()
	cases := []struct {
		kind, params string
		risk         action.Risk
		confirm      string
		inSummary    string
	}{
		{kindPodDel, `{"cluster":"c1","namespace":"km-web","name":"web-1"}`, action.RiskLow, "", "ReplicaSet web-7d9"},
		{kindRestart, `{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web"}`, action.RiskLow, "", "Redémarrage progressif"},
		{kindRestart, `{"cluster":"c1","namespace":"km-api","kind":"deployment","name":"api"}`, action.RiskMedium, "", "Un seul réplica"},
		{kindRestart, `{"cluster":"c1","namespace":"kube-system","kind":"deployment","name":"coredns"}`, action.RiskMedium, "", "Namespace système"},
		{kindScale, `{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web","replicas":5}`, action.RiskMedium, "", "de 3 à 5"},
		{kindScale, `{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web","replicas":0}`, action.RiskHigh, "web", "s'arrête"},
		{kindScale, `{"cluster":"c1","namespace":"kube-system","kind":"deployment","name":"coredns","replicas":2}`, action.RiskHigh, "coredns", "Namespace système"},
		{kindCordon, `{"cluster":"c1","node":"n1"}`, action.RiskMedium, "", "Isoler"},
	}
	for _, tc := range cases {
		p, err := c.Prepare(ctx, tc.kind, json.RawMessage(tc.params))
		if err != nil {
			t.Fatalf("%s %s : %v", tc.kind, tc.params, err)
		}
		if p.Risk != tc.risk || p.Confirm != tc.confirm || !strings.Contains(p.Summary, tc.inSummary) {
			t.Errorf("%s %s : %+v", tc.kind, tc.params, p)
		}
	}
	for _, bad := range []struct{ kind, params string }{
		{kindPodDel, `{"cluster":"c1","namespace":"km-web","name":"seul"}`},                     // sans contrôleur
		{kindPodDel, `{"cluster":"c1","namespace":"km-web","name":"absent"}`},                   // inexistant
		{kindRestart, `{"cluster":"c9","namespace":"km-web","kind":"deployment","name":"web"}`}, // cluster inconnu
		{kindScale, `{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web","replicas":500}`},
		{kindScale, `{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web"}`},
		{kindRestart, `{"cluster":"c1","namespace":"km-web","kind":"cronjob","name":"web"}`},
		{kindUncordon, `{"cluster":"c1","node":"n1"}`}, // pas isolé
		{kindRestart, `{"cluster":"c1","namespace":"../x","kind":"deployment","name":"web"}`},
		{"k8s.secret.read", `{}`},
	} {
		if _, err := c.Prepare(ctx, bad.kind, json.RawMessage(bad.params)); err == nil {
			t.Errorf("%s %s accepté", bad.kind, bad.params)
		}
	}
	if _, err := c.Prepare(ctx, kindPodDel, json.RawMessage(`{"cluster":"c1","namespace":"km-web","name":"seul"}`)); !errors.Is(err, action.ErrRefused) {
		t.Errorf("pod sans contrôleur : %v", err)
	}
}

func TestActionsExecute(t *testing.T) {
	c, cs := actionFixture()
	ctx := context.Background()
	a := action.Approval{CardID: "card-1", ApprovedBy: "awa@betorun.test"}

	if _, err := c.Execute(ctx, kindRestart, json.RawMessage(`{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web"}`), a); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("km-web").Get(ctx, "web", metav1.GetOptions{})
	ann := d.Spec.Template.Annotations
	if ann[restartedAt] != "2030-03-01T12:00:00Z" || ann[approvedBy] != "awa@betorun.test" || ann[cardAnnotation] != "card-1" {
		t.Fatalf("annotations : %v", ann)
	}

	if _, err := c.Execute(ctx, kindScale, json.RawMessage(`{"cluster":"c1","namespace":"km-web","kind":"deployment","name":"web","replicas":5}`), a); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("km-web").Get(ctx, "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 5 {
		t.Fatalf("réplicas : %d", *d.Spec.Replicas)
	}

	if _, err := c.Execute(ctx, kindPodDel, json.RawMessage(`{"cluster":"c1","namespace":"km-web","name":"web-1"}`), a); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Pods("km-web").Get(ctx, "web-1", metav1.GetOptions{}); err == nil {
		t.Fatal("pod encore là")
	}

	if _, err := c.Execute(ctx, kindCordon, json.RawMessage(`{"cluster":"c1","node":"n1"}`), a); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "n1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("nœud non isolé")
	}
	// Dernier nœud ouvert : isoler n2 devient un risque élevé.
	p, err := c.Prepare(ctx, kindCordon, json.RawMessage(`{"cluster":"c1","node":"n2"}`))
	if err != nil || p.Risk != action.RiskHigh || p.Confirm != "n2" {
		t.Fatalf("dernier nœud : %+v %v", p, err)
	}
	if _, err := c.Prepare(ctx, kindUncordon, json.RawMessage(`{"cluster":"c1","node":"n1"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestActionsCatalog(t *testing.T) {
	c, _ := actionFixture()
	if _, err := action.NewRegistry(c); err != nil {
		t.Fatal(err)
	}
	if len(New(nil).Actions()) != 0 {
		t.Fatal("actions sans cluster")
	}
	for _, s := range c.Actions() {
		if !json.Valid(s.Parameters) || s.Capability != action.CapInfrastructure {
			t.Errorf("%s : schéma ou droit", s.Kind)
		}
	}
}
