package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/thekrauss/kubemanager/internal/egressagent"
)

func TestNodes_PublishAndMounted(t *testing.T) {
	ctx := context.Background()
	kube := kubefake.NewClientset()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	n := &Nodes{Kube: func(context.Context, string) (kubernetes.Interface, error) { return kube, nil }, Now: func() time.Time { return now }}

	_, agent, err := n.Mounted(ctx, "gra-1")
	require.NoError(t, err)
	assert.False(t, agent, "aucun agent n'a rendu compte")

	require.NoError(t, n.Publish(ctx, "gra-1", egressagent.Desired{"gw-1": {"203.0.113.7"}}))
	require.NoError(t, n.Publish(ctx, "gra-1", egressagent.Desired{"gw-1": {"203.0.113.7", "203.0.113.8"}}), "mise à jour")
	cm, err := kube.CoreV1().ConfigMaps(egressagent.Namespace).Get(ctx, egressagent.DesiredConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	d, err := egressagent.ParseDesired(cm.Data[egressagent.DesiredKey])
	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.7", "203.0.113.8"}, d["gw-1"])

	fresh, _ := json.Marshal(egressagent.Status{Interface: "eth0", Addresses: []string{"203.0.113.7"}, At: now.Add(-30 * time.Second)})
	stale, _ := json.Marshal(egressagent.Status{Interface: "eth0", Addresses: []string{"203.0.113.9"}, At: now.Add(-10 * time.Minute)})
	_, err = kube.CoreV1().ConfigMaps(egressagent.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: egressagent.StatusConfigMap, Namespace: egressagent.Namespace},
		Data:       map[string]string{"gw-1": string(fresh), "gw-2": string(stale)}}, metav1.CreateOptions{})
	require.NoError(t, err)
	m, agent, err := n.Mounted(ctx, "gra-1")
	require.NoError(t, err)
	assert.True(t, agent)
	assert.True(t, m["gw-1"]["203.0.113.7"])
	assert.False(t, m["gw-2"]["203.0.113.9"], "agent silencieux : rien n'est tenu pour monté")
}
