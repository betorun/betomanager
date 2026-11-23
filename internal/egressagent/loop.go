package egressagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// Agent : un nœud, ses adresses voulues, son statut.
type Agent struct {
	Kube kubernetes.Interface
	Node string
	// Interface : interface de sortie ("" : celle de la route par défaut).
	Interface string
	Run       Runner
	Every     time.Duration
	Logger    *slog.Logger
	Now       func() time.Time
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

// Once : un passage (lire, aligner, publier).
func (a *Agent) Once(ctx context.Context) Status {
	st := Status{At: a.now(), Addresses: []string{}}
	iface := a.Interface
	if iface == "" {
		var err error
		if iface, err = DefaultInterface(ctx, a.Run); err != nil {
			st.Error = err.Error()
			a.publish(ctx, st)
			return st
		}
	}
	st.Interface = iface
	want, err := a.desired(ctx)
	if err != nil {
		// Consigne illisible : on ne retire rien sur une erreur de lecture.
		st.Error = err.Error()
		if have, oerr := Owned(ctx, a.Run, iface); oerr == nil {
			st.Addresses = have
		}
		a.publish(ctx, st)
		return st
	}
	have, err := Reconcile(ctx, a.Run, iface, want)
	if have != nil {
		st.Addresses = have
	}
	if err != nil {
		st.Error = err.Error()
	}
	a.publish(ctx, st)
	return st
}

// desired : adresses de ce nœud (consigne absente : aucune).
func (a *Agent) desired(ctx context.Context) ([]string, error) {
	cm, err := a.Kube.CoreV1().ConfigMaps(Namespace).Get(ctx, DesiredConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := ParseDesired(cm.Data[DesiredKey])
	if err != nil {
		return nil, err
	}
	return d[a.Node], nil
}

// publish : clé <nœud> de StatusConfigMap (créé au besoin).
func (a *Agent) publish(ctx context.Context, st Status) {
	raw, _ := json.Marshal(st)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cms := a.Kube.CoreV1().ConfigMaps(Namespace)
		cm, err := cms.Get(ctx, StatusConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StatusConfigMap, Namespace: Namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "betorun"}},
				Data: map[string]string{a.Node: string(raw)}}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[a.Node] = string(raw)
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil && a.Logger != nil {
		a.Logger.Warn("egress status not published", "error", err)
	}
}

// Loop : passages réguliers jusqu'à l'arrêt.
func (a *Agent) Loop(ctx context.Context) {
	every := a.Every
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		st := a.Once(ctx)
		if a.Logger != nil {
			if st.Error != "" {
				a.Logger.Warn("egress reconcile", "interface", st.Interface, "addresses", st.Addresses, "error", st.Error)
			} else {
				a.Logger.Debug("egress reconcile", "interface", st.Interface, "addresses", st.Addresses)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ParseStatus : statuts publiés par les agents, par nœud.
func ParseStatus(data map[string]string) map[string]Status {
	out := map[string]Status{}
	for node, raw := range data {
		var s Status
		if json.Unmarshal([]byte(raw), &s) == nil {
			out[node] = s
		}
	}
	return out
}
