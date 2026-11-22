package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/databases/service"
)

// Ressources CloudNativePG et plugin Barman Cloud.
var (
	ClusterGVR         = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	ScheduledBackupGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "scheduledbackups"}
	BackupGVR          = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
	ObjectStoreGVR     = schema.GroupVersionResource{Group: "barmancloud.cnpg.io", Version: "v1", Resource: "objectstores"}
)

// barmanPlugin : nom du plugin de sauvegarde (CNPG-I). L'archivage natif
// (barmanObjectStore) est déprécié depuis 1.26 et retiré en 1.32.
const barmanPlugin = "barman-cloud.cloudnative-pg.io"

// operatorNamespace : espace de l'opérateur (manifeste officiel).
const operatorNamespace = "cnpg-system"

// CNPG : Clusters sur un vrai cluster (client dynamique pour la CRD, client
// typé pour la politique réseau).
type CNPG struct {
	Dynamic      dynamic.Interface
	Kube         kubernetes.Interface
	StorageClass string // vide : classe par défaut du cluster
	// Backup : nil = bases sans sauvegarde (aucun stockage configuré).
	Backup *service.BackupTarget
}

// Conteneur annexe du plugin dans le pod de la base : demandes et limites
// obligatoires, comme Postgres (quota du projet).
const (
	sidecarCPURequest = "50m"
	sidecarMemRequest = "64Mi"
	sidecarCPULimit   = "200m"
	sidecarMemLimit   = "256Mi"
)

func storeName(cluster string) string    { return cluster + "-store" }
func secretName(cluster string) string   { return cluster + "-backup-s3" }
func scheduleName(cluster string) string { return cluster + "-daily" }

// Ressources d'une instance : demandes et limites obligatoires (le projet a
// un quota CPU et mémoire ; sans elles, le pod serait refusé).
const (
	cpuRequest = "100m"
	memRequest = "256Mi"
	cpuLimit   = "500m"
	memLimit   = "512Mi"
)

func labels(spec service.ClusterSpec) map[string]any {
	return map[string]any{
		"app.kubernetes.io/managed-by": "betorun",
		"betorun.dev/database-id":      spec.DatabaseID,
	}
}

// clusterObject : le Cluster CloudNativePG d'une base.
func (c *CNPG) clusterObject(ns string, spec service.ClusterSpec) *unstructured.Unstructured {
	storage := map[string]any{"size": spec.Storage}
	if c.StorageClass != "" {
		storage["storageClass"] = c.StorageClass
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": spec.Name, "namespace": ns, "labels": labels(spec)},
		"spec": map[string]any{
			"instances": int64(1),
			"storage":   storage,
			"resources": map[string]any{
				"requests": map[string]any{"cpu": cpuRequest, "memory": memRequest},
				"limits":   map[string]any{"cpu": cpuLimit, "memory": memLimit},
			},
			// Base « app », propriétaire « app » ; pas d'accès superutilisateur.
			"bootstrap":             map[string]any{"initdb": map[string]any{"database": "app", "owner": "app"}},
			"enableSuperuserAccess": false,
		},
	}}
	if spec.Restore != nil {
		// Restauration : base et propriétaire « app » recréés (nouveau mot de
		// passe), données lues dans l'ObjectStore de la base source.
		recovery := map[string]any{"source": "origin", "database": "app", "owner": "app"}
		if t := spec.Restore.TargetTime; t != nil {
			recovery["recoveryTarget"] = map[string]any{"targetTime": t.UTC().Format(time.RFC3339)}
		}
		_ = unstructured.SetNestedMap(obj.Object, map[string]any{"recovery": recovery}, "spec", "bootstrap")
		_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{
			"name": "origin",
			"plugin": map[string]any{"name": barmanPlugin, "parameters": map[string]any{
				"barmanObjectName": storeName(spec.Restore.Cluster), "serverName": spec.Restore.Cluster,
			}},
		}}, "spec", "externalClusters")
	}
	if c.Backup != nil {
		// Archivage continu du journal (WAL) : restauration à un instant donné.
		_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{
			"name": barmanPlugin, "isWALArchiver": true,
			"parameters": map[string]any{"barmanObjectName": storeName(spec.Name)},
		}}, "spec", "plugins")
	}
	return obj
}

// backupObjects : secret des clés S3, ObjectStore (destination, rétention,
// compression, ressources du conteneur annexe) et sauvegarde planifiée.
func (c *CNPG) backupObjects(ns string, spec service.ClusterSpec) (*corev1.Secret, *unstructured.Unstructured, *unstructured.Unstructured) {
	b := c.Backup
	lbl := map[string]string{"app.kubernetes.io/managed-by": "betorun", "betorun.dev/database-id": spec.DatabaseID}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(spec.Name), Namespace: ns, Labels: lbl},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"ACCESS_KEY_ID": b.AccessKey, "SECRET_ACCESS_KEY": b.SecretKey, "REGION": b.Region},
	}
	ref := func(key string) map[string]any { return map[string]any{"name": secretName(spec.Name), "key": key} }
	creds := map[string]any{"accessKeyId": ref("ACCESS_KEY_ID"), "secretAccessKey": ref("SECRET_ACCESS_KEY")}
	if b.Region != "" {
		creds["region"] = ref("REGION")
	}
	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "barmancloud.cnpg.io/v1",
		"kind":       "ObjectStore",
		"metadata":   map[string]any{"name": storeName(spec.Name), "namespace": ns, "labels": labels(spec)},
		"spec": map[string]any{
			"configuration": map[string]any{
				// Un dossier par projet et par base : jamais deux bases au même endroit.
				"destinationPath": "s3://" + b.Bucket + "/" + spec.ProjectID + "/" + spec.DatabaseID,
				"endpointURL":     b.Endpoint,
				"s3Credentials":   creds,
				"wal":             map[string]any{"compression": "gzip"},
				"data":            map[string]any{"compression": "gzip"},
			},
			"retentionPolicy": b.Retention,
			"instanceSidecarConfiguration": map[string]any{"resources": map[string]any{
				"requests": map[string]any{"cpu": sidecarCPURequest, "memory": sidecarMemRequest},
				"limits":   map[string]any{"cpu": sidecarCPULimit, "memory": sidecarMemLimit},
			}},
		},
	}}
	sched := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "ScheduledBackup",
		"metadata":   map[string]any{"name": scheduleName(spec.Name), "namespace": ns, "labels": labels(spec)},
		"spec": map[string]any{
			"schedule":             b.Schedule,
			"immediate":            true, // une première sauvegarde complète dès la création
			"backupOwnerReference": "self",
			"cluster":              map[string]any{"name": spec.Name},
			"method":               "plugin",
			"pluginConfiguration":  map[string]any{"name": barmanPlugin},
		},
	}}
	return secret, store, sched
}

// networkPolicy : seuls les pods du projet (même espace) et l'opérateur
// joignent l'instance. Sans elle, n'importe quel pod du cluster le pourrait.
func networkPolicy(ns string, spec service.ClusterSpec) *networkingv1.NetworkPolicy {
	pg := intstr.FromInt32(5432)
	tcp := corev1.ProtocolTCP
	lbl := map[string]string{"app.kubernetes.io/managed-by": "betorun", "betorun.dev/database-id": spec.DatabaseID}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: ns, Labels: lbl},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"cnpg.io/cluster": spec.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				// Conteneurs du projet : Postgres seulement.
				{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &pg}}},
				// Opérateur CloudNativePG : supervision et pilotage de l'instance.
				{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace}}}}},
			},
		},
	}
}

func (c *CNPG) Create(ctx context.Context, ns string, spec service.ClusterSpec) error {
	if spec.Restore != nil && c.Backup == nil {
		return domain.ErrBackupsDisabled
	}
	if _, err := c.Kube.NetworkingV1().NetworkPolicies(ns).Create(ctx, networkPolicy(ns, spec), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("network policy: %w", err)
	}
	var sched *unstructured.Unstructured
	if c.Backup != nil {
		// La destination existe avant le Cluster : l'archivage du journal
		// démarre avec la base.
		secret, store, s := c.backupObjects(ns, spec)
		sched = s
		if _, err := c.Kube.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("backup secret: %w", err)
		}
		_, err := c.Dynamic.Resource(ObjectStoreGVR).Namespace(ns).Create(ctx, store, metav1.CreateOptions{})
		if apierrors.IsNotFound(err) {
			return domain.ErrBackupPluginMissing
		}
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("object store: %w", err)
		}
	}
	_, err := c.Dynamic.Resource(ClusterGVR).Namespace(ns).Create(ctx, c.clusterObject(ns, spec), metav1.CreateOptions{})
	if apierrors.IsNotFound(err) {
		// CRD absente : l'opérateur n'est pas installé sur ce cluster.
		return domain.ErrOperatorMissing
	}
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("cluster: %w", err)
	}
	if sched != nil {
		if _, err := c.Dynamic.Resource(ScheduledBackupGVR).Namespace(ns).Create(ctx, sched, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("scheduled backup: %w", err)
		}
	}
	return nil
}

func (c *CNPG) State(ctx context.Context, ns, name string) (service.ClusterState, error) {
	obj, err := c.Dynamic.Resource(ClusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return service.ClusterState{}, nil
	}
	if err != nil {
		return service.ClusterState{}, err
	}
	st := service.ClusterState{Exists: true, Hibernated: obj.GetAnnotations()[hibernationAnnotation] == "on"}
	st.Phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
	st.Image, _, _ = unstructured.NestedString(obj.Object, "status", "image")
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyInstances")
	st.ReadyInstance = ready >= 1
	plugins, _, _ := unstructured.NestedSlice(obj.Object, "spec", "plugins")
	for _, p := range plugins {
		if m, ok := p.(map[string]any); ok && m["name"] == barmanPlugin {
			st.BackupsEnabled = true
		}
	}
	if st.BackupsEnabled {
		st.LastBackup = c.lastBackup(ctx, ns, name)
	}
	return st, nil
}

// lastBackup : la sauvegarde complète la plus récente (au mieux : une erreur
// de lecture ne fait pas échouer l'état de la base).
func (c *CNPG) lastBackup(ctx context.Context, ns, cluster string) *service.BackupState {
	list, err := c.Dynamic.Resource(BackupGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + cluster})
	if err != nil || len(list.Items) == 0 {
		return nil
	}
	var best *service.BackupState
	var bestAt time.Time
	for _, item := range list.Items {
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		stopped, _, _ := unstructured.NestedString(item.Object, "status", "stoppedAt")
		msg, _, _ := unstructured.NestedString(item.Object, "status", "error")
		at := item.GetCreationTimestamp().Time
		b := &service.BackupState{Phase: phase, Error: msg}
		if t, err := time.Parse(time.RFC3339, stopped); err == nil {
			b.StoppedAt = t
		}
		if best == nil || at.After(bestAt) {
			best, bestAt = b, at
		}
	}
	return best
}

// hibernationAnnotation : hibernation déclarative de CloudNativePG. « on » :
// les pods sont supprimés, les disques gardés ; « off » : l'instance repart
// sur le même disque.
const hibernationAnnotation = "cnpg.io/hibernation"

func (c *CNPG) Hibernate(ctx context.Context, ns, name string, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{hibernationAnnotation: value}}})
	if err != nil {
		return err
	}
	_, err = c.Dynamic.Resource(ClusterGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return errClusterMissing
	}
	return err
}

var errClusterMissing = errors.New("database cluster not found")

// Delete : le Cluster (ses disques suivent : CloudNativePG en est
// propriétaire) et sa politique réseau. Absent : rien à faire.
func (c *CNPG) Delete(ctx context.Context, ns, name string) error {
	err := c.Dynamic.Resource(ClusterGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cluster: %w", err)
	}
	err = c.Kube.NetworkingV1().NetworkPolicies(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("network policy: %w", err)
	}
	// Sauvegarde planifiée, destination et clés S3. Les fichiers déjà
	// sauvegardés restent dans le bucket (filet de sécurité ; ménage par
	// règle de cycle de vie du bucket). CRD absentes : rien à supprimer.
	for _, gvr := range []schema.GroupVersionResource{ScheduledBackupGVR, ObjectStoreGVR} {
		n := scheduleName(name)
		if gvr == ObjectStoreGVR {
			n = storeName(name)
		}
		if err := c.Dynamic.Resource(gvr).Namespace(ns).Delete(ctx, n, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("%s: %w", gvr.Resource, err)
		}
	}
	if err := c.Kube.CoreV1().Secrets(ns).Delete(ctx, secretName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("backup secret: %w", err)
	}
	return nil
}

func (c *CNPG) AppURI(ctx context.Context, ns, name string) (string, error) {
	sec, err := c.Kube.CoreV1().Secrets(ns).Get(ctx, name+"-app", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", domain.ErrAppSecretMissing
	}
	if err != nil {
		return "", err
	}
	uri := string(sec.Data["uri"])
	if uri == "" {
		return "", domain.ErrAppSecretMissing
	}
	return uri, nil
}
