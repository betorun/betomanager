package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/core/configs"
	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/workflows"
)

func TestPublicRoute(t *testing.T) {
	saved := configs.AppConfig
	defer func() { configs.AppConfig = saved }()
	project := uuid.MustParse("37f62891-bdce-40b1-be27-833c1fea931c")
	s := &WorkloadService{}

	// Sans IP du point d'entrée : pas d'URL (avant : hôte invalide « web-37f628..sslip.io »).
	configs.AppConfig.Vps.AdressIp = ""
	assert.Equal(t, route{}, publicRoute("web", project, false, s.configProfile()))

	configs.AppConfig.Vps.AdressIp = " 5.196.223.196 "
	configs.AppConfig.Kubernetes.IngressClass = "traefik"
	configs.AppConfig.Kubernetes.IngressNamespace = "kube-system"
	r := publicRoute("web", project, false, s.configProfile())
	assert.Equal(t, "web-37f628.5.196.223.196.sslip.io", r.Host)
	assert.Equal(t, "http://web-37f628.5.196.223.196.sslip.io", r.URL, "URL enregistrée avec le workload")
	assert.Equal(t, "traefik", r.IngressClass)
	assert.Equal(t, "kube-system", r.IngressNamespace)
	assert.Equal(t, "https://web-37f628.5.196.223.196.sslip.io", publicRoute("web", project, true, s.configProfile()).URL)
}

type fakeClusterSettings struct {
	by  map[uuid.UUID]k8sprovider.Settings
	err error
}

func (f fakeClusterSettings) SettingsForProject(_ context.Context, p uuid.UUID) (k8sprovider.Settings, error) {
	return f.by[p], f.err
}

// Lot 2c : un projet d'un autre cluster prend son ingress, son point
// d'entrée et sa classe de disques ; champ vide : celui de la configuration.
func TestProfileFor_ClusterSettings(t *testing.T) {
	saved := configs.AppConfig
	defer func() { configs.AppConfig = saved }()
	configs.AppConfig.Vps.AdressIp = "5.196.223.196"
	configs.AppConfig.Kubernetes.IngressClass = "traefik"
	configs.AppConfig.Kubernetes.IngressNamespace = "kube-system"

	onDefault, onIP, onName := uuid.New(), uuid.New(), uuid.New()
	dns := &fakeDNS{hosts: map[string][]string{"edge.gra.example": {"2001:db8::1", "198.51.100.7"}}}
	s := &WorkloadService{
		DomainCfg: &DomainSettings{Resolver: dns, Target: "5.196.223.196"},
		Clusters: fakeClusterSettings{by: map[uuid.UUID]k8sprovider.Settings{
			onDefault: {ClusterID: "main"},
			onIP:      {ClusterID: "gra", IngressClass: "nginx", IngressNamespace: "ingress-nginx", IngressTarget: "203.0.113.20", StorageClass: "csi-cinder"},
			onName:    {ClusterID: "sbg", IngressTarget: "edge.gra.example"},
		}},
	}
	ctx := context.Background()

	p, err := s.profileFor(ctx, onDefault)
	require.NoError(t, err)
	assert.Equal(t, s.configProfile(), p, "cluster sans réglage : la configuration")

	p, err = s.profileFor(ctx, onIP)
	require.NoError(t, err)
	assert.Equal(t, profile{IngressClass: "nginx", IngressNamespace: "ingress-nginx", PublicIP: "203.0.113.20",
		Target: "203.0.113.20", StorageClass: "csi-cinder"}, p)
	assert.Equal(t, "http://web-"+onIP.String()[:6]+".203.0.113.20.sslip.io",
		routeFor(domain.KindWeb, "web", onIP, false, false, p).URL, "adresse automatique sur le point d'entrée du cluster")

	p, err = s.profileFor(ctx, onName)
	require.NoError(t, err)
	assert.Equal(t, "edge.gra.example", p.Target, "cible des domaines : le nom")
	assert.Equal(t, "198.51.100.7", p.PublicIP, "sslip.io : première IPv4 du nom")
	assert.Equal(t, "traefik", p.IngressClass, "classe non réglée : la configuration")

	// Réglages illisibles : erreur, jamais ceux d'un autre cluster.
	s.Clusters = fakeClusterSettings{err: errors.New("db down")}
	_, err = s.profileFor(ctx, onIP)
	assert.Error(t, err)
}

// Création sur un autre cluster : sa classe de disques, son ingress et son
// point d'entrée ; une classe demandée explicitement reste celle du client.
func TestDeploy_UsesProjectClusterSettings(t *testing.T) {
	saved := configs.AppConfig
	defer func() { configs.AppConfig = saved }()
	configs.AppConfig.Vps.AdressIp = "5.196.223.196"
	configs.AppConfig.Kubernetes.IngressClass = "traefik"

	s, _, dist := createService(t)
	project := uuid.New()
	s.Clusters = fakeClusterSettings{by: map[uuid.UUID]k8sprovider.Settings{
		project: {ClusterID: "gra", IngressClass: "nginx", IngressTarget: "203.0.113.20", StorageClass: "csi-cinder"}}}
	req := createRequest(func(r *domain.CreateWorkloadRequest) {
		r.ProjectID, r.PersistenceEnabled, r.StorageSize = project.String(), true, "1Gi"
	})
	w, err := s.Deploy(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "csi-cinder", w.StorageClass)
	in := dist.args[0].(workflows.DeployWorkloadInput)
	assert.Equal(t, "csi-cinder", in.StorageClass)
	assert.Equal(t, "nginx", in.IngressClass)
	assert.Contains(t, in.ExternalHost, ".203.0.113.20.sslip.io")
	assert.Contains(t, w.ExternalURL, ".203.0.113.20.sslip.io")

	req = createRequest(func(r *domain.CreateWorkloadRequest) {
		r.ProjectID, r.Name, r.StorageClass = project.String(), "other", "fast-ssd"
	})
	w, err = s.Deploy(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "fast-ssd", w.StorageClass, "classe choisie par le client gardée")

	// Réglages illisibles : rien n'est créé.
	s.Clusters = fakeClusterSettings{err: errors.New("db down")}
	_, err = s.Deploy(context.Background(), createRequest(func(r *domain.CreateWorkloadRequest) { r.ProjectID, r.Name = project.String(), "third" }))
	assert.Error(t, err)
}
