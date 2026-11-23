package repository

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/egressagent"
)

// Nodes : consigne des agents réseau (IP à porter par nœud) et ce qu'ils
// portent vraiment, lus et écrits dans le cluster (ConfigMap de betorun-system).
type Nodes struct {
	Kube func(ctx context.Context, clusterID string) (kubernetes.Interface, error)
	Now  func() time.Time
}

func (n *Nodes) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now().UTC()
}

// Publish : la consigne du cluster (toutes les IP de sa réserve, par nœud).
func (n *Nodes) Publish(ctx context.Context, clusterID string, d egressagent.Desired) error {
	kube, err := n.Kube(ctx, clusterID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	cms := kube.CoreV1().ConfigMaps(egressagent.Namespace)
	cm, err := cms.Get(ctx, egressagent.DesiredConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: egressagent.DesiredConfigMap,
			Namespace: egressagent.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "betorun"}},
			Data: map[string]string{egressagent.DesiredKey: string(raw)}}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if cm.Data[egressagent.DesiredKey] == string(raw) {
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[egressagent.DesiredKey] = string(raw)
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// Mounted : par nœud, les IP qu'un agent récent porte ; agent false : aucun
// agent n'a jamais rendu compte sur ce cluster (montage à la main).
func (n *Nodes) Mounted(ctx context.Context, clusterID string) (map[string]map[string]bool, bool, error) {
	kube, err := n.Kube(ctx, clusterID)
	if err != nil {
		return nil, false, err
	}
	cm, err := kube.CoreV1().ConfigMaps(egressagent.Namespace).Get(ctx, egressagent.StatusConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := map[string]map[string]bool{}
	now := n.now()
	for node, st := range egressagent.ParseStatus(cm.Data) {
		set := map[string]bool{}
		for _, a := range st.Mounted(now) {
			set[a] = true
		}
		out[node] = set
	}
	return out, true, nil
}
