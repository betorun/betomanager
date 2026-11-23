package egressagent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestAgent_Once(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DesiredConfigMap, Namespace: Namespace},
		Data: map[string]string{DesiredKey: `{"gw-1":["203.0.113.7"],"gw-2":["203.0.113.8"]}`}})
	f := &fakeIP{route: "default via 1.2.3.1 dev eth0\n", addrs: map[string]string{"51.75.141.233": "eth0"}}
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	a := &Agent{Kube: kube, Node: "gw-1", Run: f.run, Now: func() time.Time { return now }}

	st := a.Once(ctx)
	assert.Empty(t, st.Error)
	assert.Equal(t, "eth0", st.Interface)
	assert.Equal(t, []string{"203.0.113.7"}, st.Addresses, "seulement les adresses de ce nœud")

	cm, err := kube.CoreV1().ConfigMaps(Namespace).Get(ctx, StatusConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	got := ParseStatus(cm.Data)["gw-1"]
	assert.Equal(t, []string{"203.0.113.7"}, got.Addresses)
	assert.True(t, got.At.Equal(now))

	// Un second nœud publie sans écraser le premier.
	b := &Agent{Kube: kube, Node: "gw-2", Run: (&fakeIP{route: "default via 1.2.3.1 dev eth0\n", addrs: map[string]string{}}).run, Now: a.Now}
	b.Once(ctx)
	cm, _ = kube.CoreV1().ConfigMaps(Namespace).Get(ctx, StatusConfigMap, metav1.GetOptions{})
	all := ParseStatus(cm.Data)
	assert.Len(t, all, 2)

	// Consigne illisible : rien n'est retiré.
	des, _ := kube.CoreV1().ConfigMaps(Namespace).Get(ctx, DesiredConfigMap, metav1.GetOptions{})
	des.Data[DesiredKey] = "{oops"
	_, _ = kube.CoreV1().ConfigMaps(Namespace).Update(ctx, des, metav1.UpdateOptions{})
	st = a.Once(ctx)
	assert.NotEmpty(t, st.Error)
	assert.Equal(t, []string{"203.0.113.7"}, st.Addresses)
	assert.Equal(t, "eth0:bt", f.addrs["203.0.113.7"])

	// Consigne retirée : plus rien à porter.
	require.NoError(t, kube.CoreV1().ConfigMaps(Namespace).Delete(ctx, DesiredConfigMap, metav1.DeleteOptions{}))
	st = a.Once(ctx)
	assert.Empty(t, st.Error)
	assert.Empty(t, st.Addresses)
}
