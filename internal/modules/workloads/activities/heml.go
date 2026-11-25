package activities

import (
	"context"
	"errors"
	"fmt"
	"github.com/thekrauss/kubemanager/internal/infrastructure/heartbeat"
	"github.com/thekrauss/kubemanager/internal/infrastructure/helm"
	"github.com/thekrauss/kubemanager/internal/infrastructure/helm/charts"
	netDomain "github.com/thekrauss/kubemanager/internal/modules/network/domain"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"helm.sh/helm/v3/pkg/action"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	idempotency "github.com/thekrauss/kubemanager/internal/infrastructure/idempotence"
	k8sprovider "github.com/thekrauss/kubemanager/internal/infrastructure/kubernetes"
	"github.com/thekrauss/kubemanager/internal/modules/registries/pull"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/repository"
)

// WorkloadActivities : activities Temporal pour déployer des workloads via Helm.
// Idempotency (optionnel) déduplique InstallChart / EnsureSecret sur retry.
type WorkloadActivities struct {
	// Kube : clients du cluster de chaque projet (les entrées des activités
	// ne changent pas : le namespace ou le projet suffit à le retrouver).
	Kube k8sprovider.Resolver
	// Pull : Secret de pull des projets (registre de la plateforme et
	// registres externes).
	Pull        *pull.Secrets
	Idempotency idempotency.AtomicManager
	// Variables : valeurs chiffrées des variables des conteneurs.
	Variables repository.VariableStore
}

// WorkloadIdentity : le workload s'exécute comme un compte de service ; ses
// pods reçoivent un jeton Kubernetes projeté (audience = STS) à échanger.
type WorkloadIdentity struct {
	ServiceAccount string // "serviceAccount:<uuid>"
	Audience       string // émetteur du STS
	TokenURI       string
}

// ProcessSpec : sorte du conteneur et processus lancé (vide : web, commande
// de l'image). Ajouté aux entrées des workflows : les historiques en cours,
// qui ne le portent pas, restent des conteneurs web.
type ProcessSpec struct {
	Kind    string   `json:",omitempty"` // web | worker | cron | service
	Command []string `json:",omitempty"`
	Args    []string `json:",omitempty"`
	// Cron
	Schedule           string `json:",omitempty"`
	TimeZone           string `json:",omitempty"`
	MaxDurationSeconds int    `json:",omitempty"`
	// Suspend : cron arrêté (statut STOPPED) ; rendu à chaque déploiement
	// pour qu'une mise à jour ne le réactive pas.
	Suspend bool `json:",omitempty"`
	// WaitFor : adresses nom:port attendues par un conteneur d'initialisation
	// avant le démarrage (réseau du projet pas encore ouvert pour un nouveau pod).
	WaitFor []string `json:",omitempty"`
	// HealthCommand : sonde par commande d'un worker ou d'un service (vide :
	// aucune sonde pour un worker, sonde TCP sur le port pour un service).
	HealthCommand []string `json:",omitempty"`
	// ExtraPorts : ports internes supplémentaires (Service du conteneur et
	// containerPort), jamais publiés.
	ExtraPorts []NamedPort `json:",omitempty"`
}

// NamedPort : port interne supplémentaire (TCP).
type NamedPort struct {
	Name string
	Port int
}

// IsWeb : conteneur web (y compris sorte absente des entrées antérieures).
func (p ProcessSpec) IsWeb() bool { return p.Kind == "" || p.Kind == "web" }

type InstallWorkloadInput struct {
	Identity *WorkloadIdentity
	Process  ProcessSpec

	ProjectID    string
	WorkloadID   string
	ReleaseName  string
	Namespace    string
	ImageRepo    string
	ImageTag     string
	PullSecrets  []string
	ExternalURL  string
	IngressClass string // vide : nginx
	// IngressNamespace : espace du contrôleur d'ingress (vide : ingress-nginx).
	IngressNamespace string
	ServiceType      string // "ClusterIP" ou "LoadBalancer"

	// EnvRevision : change les pods quand les variables changent (annotation
	// du Deployment), sans rien révéler de leurs valeurs.
	EnvRevision int64

	// CustomDomains : un Ingress par nom (voir le chart).
	CustomDomains []string
	// CustomRoutes : nom + chemin (prioritaire sur CustomDomains).
	CustomRoutes    []DomainRoute
	CustomDomainTLS bool

	PersistenceEnabled bool
	StorageSize        string
	StorageClass       string
	MountPath          string

	Replicas   int
	TargetPort int

	// Resources
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string

	// Security overrides (0 = chart defaults 1000).
	RunAsUser  int64
	RunAsGroup int64

	// ReadOnlyRootFS : opt-in. Si true, tmpfs montés sur /tmp et /var/run.
	ReadOnlyRootFS bool

	// Autoscaling
	AutoscalingEnabled      bool
	AutoscalingMin          int32
	AutoscalingMax          int32
	AutoscalingTargetCPU    int32
	AutoscalingTargetMemory int32

	// Disruption budget
	PDBEnabled bool

	// TLS
	TLSEnabled bool

	// Probes
	HealthPath    string
	ReadinessPath string
	// StartupTimeoutSeconds : délai de démarrage toléré (0 : 150 s).
	StartupTimeoutSeconds int
}

// forProject : cluster du projet (namespace en repli si l'identifiant est
// mal formé).
func (a *WorkloadActivities) forProject(ctx context.Context, projectID, namespace string) (*k8sprovider.ProviderK8s, error) {
	var (
		kube *k8sprovider.ProviderK8s
		err  error
	)
	if id, perr := uuid.Parse(projectID); perr == nil {
		kube, err = a.Kube.ForProject(ctx, id)
	} else {
		kube, err = a.Kube.ForNamespace(ctx, namespace)
	}
	if err != nil {
		return nil, fmt.Errorf("project cluster: %w", err)
	}
	return kube, nil
}

func helmLog(format string, v ...interface{}) {
	fmt.Printf(format, v...)
}

func (a *WorkloadActivities) InstallChart(ctx context.Context, input InstallWorkloadInput) error {
	start := time.Now()
	wait := HelmWaitTimeout(input.StartupTimeoutSeconds)
	_, _, err := idempotency.ExecuteActivity(ctx, a.Idempotency, input,
		idempotency.ActivityOptions{ProcessingTTL: InstallProcessingTTL(input.StartupTimeoutSeconds), RetakeOnRetry: true},
		func() (struct{}, error) {
			kube, err := a.forProject(ctx, input.ProjectID, input.Namespace)
			if err != nil {
				return struct{}{}, err
			}
			actionConfig, err := helm.NewActionConfig(kube.Config, input.Namespace, helmLog)
			if err != nil {
				return struct{}{}, err
			}

			// Chart embarqué dans le binaire (internal/infrastructure/helm/charts).
			chart, err := charts.StandardApp()
			if err != nil {
				return struct{}{}, fmt.Errorf("failed to load chart: %w", err)
			}
			vals := buildHelmValues(input)

			// Une tentative suivante ne démarre qu'une fois la précédente tenue
			// pour morte par Temporal (délai de heartbeat ou d'activité).
			retried := activity.GetInfo(ctx).Attempt > 1
			if err := heartbeat.While(ctx, installHeartbeatEvery, func() error {
				return upgradeOrInstall(ctx, actionConfig, input.Namespace, input.ReleaseName, chart, vals, retried, wait)
			}); err != nil {
				return struct{}{}, fmt.Errorf("helm release failed: %w", err)
			}
			return struct{}{}, nil
		},
	)
	return classifyInstallError(ctx, err, time.Since(start), wait)
}

// classifyInstallError : application qui ne démarre pas dans le délai de Helm :
// une nouvelle attente n'y changerait rien (avant : environ 15 min en STARTING).
// Toute autre erreur reste relancée par Temporal : infrastructure, ou délai plus
// court qu'une attente complète (appel à l'API du cluster). L'erreur reste la plus
// externe : Temporal ne lit que le type de celle-ci.
func classifyInstallError(ctx context.Context, err error, elapsed, wait time.Duration) error {
	if err == nil || ctx.Err() != nil || elapsed < wait || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), errTypeWorkloadNotReady, err)
}

// upgradeOrInstall : équivalent de `helm upgrade --install`. Dans le SDK,
// Upgrade.Install n'a aucun effet (la logique est dans la commande helm) :
// sans ce choix explicite, le premier déploiement d'un conteneur échouait
// toujours (« has no deployed releases »).
func upgradeOrInstall(ctx context.Context, cfg *action.Configuration, namespace, name string, ch *chart.Chart, vals map[string]any, retried bool, wait time.Duration) error {
	history := action.NewHistory(cfg)
	history.Max = 1
	releases, err := history.Run(name)
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound) || (err == nil && len(releases) == 0):
		return install(ctx, cfg, namespace, name, ch, vals, false, wait)
	case err != nil:
		return err
	}
	if err := releasePendingLock(cfg, name, retried); err != nil {
		return err
	}
	upgrade := action.NewUpgrade(cfg)
	upgrade.Namespace = namespace
	upgrade.Wait = true
	upgrade.Timeout = wait
	_, err = upgrade.RunWithContext(ctx, name, ch, vals)
	if err != nil && strings.Contains(err.Error(), helmErrNoDeployedReleases) {
		// Seulement des tentatives en échec : on réinstalle par-dessus.
		return install(ctx, cfg, namespace, name, ch, vals, true, wait)
	}
	return err
}

// releasePendingLock : une release restée pending-* (worker mort en pleine
// opération) refuse toute mise à jour (« another operation is in progress »).
// Elle passe en échec si plus rien ne peut tourner : soit l'état est trop ancien,
// soit c'est une nouvelle tentative de cette même opération. Un retour arrière
// récent (lancé par l'API, hors Temporal) reste protégé.
func releasePendingLock(cfg *action.Configuration, name string, retried bool) error {
	last, err := cfg.Releases.Last(name)
	if err != nil {
		return err
	}
	if !last.Info.Status.IsPending() {
		return nil
	}
	stale := time.Since(last.Info.LastDeployed.Time) > helmPendingStaleAfter
	if !stale && (!retried || last.Info.Status == release.StatusPendingRollback) {
		return nil
	}
	last.SetStatus(release.StatusFailed, "interrupted before completion")
	return cfg.Releases.Update(last)
}

func install(ctx context.Context, cfg *action.Configuration, namespace, name string, ch *chart.Chart, vals map[string]any, replace bool, wait time.Duration) error {
	in := action.NewInstall(cfg)
	in.Namespace = namespace
	in.ReleaseName = name
	in.Wait = true
	in.Timeout = wait
	in.Replace = replace
	_, err := in.RunWithContext(ctx, ch, vals)
	return err
}

// UninstallChart : helm uninstall de la release (Saga Delete).
// Idempotent : si la release n'existe pas, on retourne nil (déjà uninstall).
func (a *WorkloadActivities) UninstallChart(ctx context.Context, namespace, releaseName string) error {
	payload := struct{ NS, Release string }{namespace, releaseName}
	_, _, err := idempotency.ExecuteActivity(ctx, a.Idempotency, payload,
		idempotency.ActivityOptions{ProcessingTTL: uninstallChartProcessingTTL},
		func() (struct{}, error) {
			kube, err := a.Kube.ForNamespace(ctx, namespace)
			if err != nil {
				return struct{}{}, fmt.Errorf("project cluster: %w", err)
			}
			actionConfig, err := helm.NewActionConfig(kube.Config, namespace, helmLog)
			if err != nil {
				return struct{}{}, err
			}
			u := action.NewUninstall(actionConfig)
			u.Wait = true
			u.Timeout = helmUninstallTimeout
			// Uninstall n'accepte pas de contexte (Helm 3.22) : borné par son
			// délai, lui-même inférieur à celui de l'activité.
			if _, err := u.Run(releaseName); err != nil {
				// "release: not found" → considéré idempotent (déjà supprimée).
				if strings.Contains(err.Error(), helmErrReleaseNotFound) {
					return struct{}{}, nil
				}
				return struct{}{}, fmt.Errorf("helm uninstall: %w", err)
			}
			return struct{}{}, nil
		},
	)
	return err
}

// buildHelmValues construit l'arbre `values` envoyé à Helm upgrade.
// Centralisé ici pour que InstallChart reste lisible + pour pouvoir tester
// le mapping Go → Helm values (unit-testable sans K8s).
//
// Règles de mapping :
//   - commonLabels.projectID/workloadID → labels K8s km-project-id/km-workload-id
//   - RunAsUser/Group = 0 → laisse les defaults du chart (1000)
//   - Autoscaling désactivé → HPA pas déployé, replicas fixe dans Deployment
//   - PDB activé uniquement si replicas >= 2 (sinon chart no-op)
func buildHelmValues(input InstallWorkloadInput) map[string]any {
	// Resources avec fallbacks sur les defaults restrictifs.
	resources := map[string]any{
		"requests": map[string]any{
			"cpu":    requestWithin(input.CPURequest, defaultCPURequest, input.CPULimit),
			"memory": requestWithin(input.MemoryRequest, defaultMemoryRequest, input.MemoryLimit),
		},
		"limits": map[string]any{
			"cpu":    fallback(input.CPULimit, DefaultCPULimit),
			"memory": fallback(input.MemoryLimit, DefaultMemoryLimit),
		},
	}

	// podSecurityContext : override RunAsUser/Group si fournis (images non-1000).
	podSec := map[string]any{
		"runAsNonRoot": true,
		"seccompProfile": map[string]any{
			"type": seccompProfileType,
		},
	}
	containerSec := map[string]any{
		"allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem":   input.ReadOnlyRootFS,
		"runAsNonRoot":             true,
		"capabilities": map[string]any{
			"drop": []string{dropAllCapability},
		},
	}
	if input.RunAsUser > 0 {
		podSec["runAsUser"] = input.RunAsUser
		containerSec["runAsUser"] = input.RunAsUser
	}
	if input.RunAsGroup > 0 {
		podSec["runAsGroup"] = input.RunAsGroup
		podSec["fsGroup"] = input.RunAsGroup
	}

	// Probes : HealthPath sert pour liveness + startup par défaut.
	healthPath := fallback(input.HealthPath, defaultHealthPath)
	readinessPath := fallback(input.ReadinessPath, healthPath)

	// Sorte : worker et cron n'ont ni port, ni Service, ni ingress, ni sondes
	// HTTP (sans port à interroger, les sondes tueraient le pod en boucle).
	proc := input.Process
	web := proc.IsWeb()
	externalURL := input.ExternalURL
	if !web {
		externalURL = ""
	}

	vals := map[string]any{
		"kind":         defaultIfEmptyKind(proc.Kind),
		"replicaCount": input.Replicas,
		"commonLabels": map[string]any{
			"projectID":  input.ProjectID,
			"workloadID": input.WorkloadID,
		},
		"image": map[string]any{
			"repository":  input.ImageRepo,
			"tag":         input.ImageTag,
			"pullSecrets": input.PullSecrets,
		},
		"service": map[string]any{
			"type":       fallback(input.ServiceType, defaultServiceType),
			"port":       defaultServicePort,
			"targetPort": input.TargetPort,
			"protocol":   defaultServiceProtocol,
		},
		"ingress": map[string]any{
			"enabled":   externalURL != "", // sans URL publique : pas d'ingress
			"className": fallback(input.IngressClass, defaultIngressClass),
			"host":      externalURL,
			"tls": map[string]any{
				"enabled":       input.TLSEnabled,
				"clusterIssuer": defaultClusterIssuer,
			},
		},
		"customDomains":            customDomainValues(input.CustomRoutes, input.CustomDomains),
		"customDomainTLS":          input.CustomDomainTLS,
		"customDomainIssuer":       defaultClusterIssuer,
		"resources":                resources,
		"podSecurityContext":       podSec,
		"containerSecurityContext": containerSec,
		"serviceAccount": map[string]any{
			"create":                       true,
			"automountServiceAccountToken": false,
			// Nom fixe : le STS retrouve le workload par
			// system:serviceaccount:<namespace>:<release>.
			"name": input.ReleaseName,
		},
		"betorunIdentity": identityValues(input.Identity),
		"persistence": map[string]any{
			"enabled":      input.PersistenceEnabled,
			"size":         input.StorageSize,
			"storageClass": input.StorageClass,
			"mountPath":    input.MountPath,
		},
		"autoscaling": map[string]any{
			"enabled":                           input.AutoscalingEnabled,
			"minReplicas":                       fallbackInt32(input.AutoscalingMin, defaultAutoscalingMinReplicas),
			"maxReplicas":                       fallbackInt32(input.AutoscalingMax, defaultAutoscalingMaxReplicas),
			"targetCPUUtilizationPercentage":    fallbackInt32(input.AutoscalingTargetCPU, defaultAutoscalingTargetCPU),
			"targetMemoryUtilizationPercentage": input.AutoscalingTargetMemory,
		},
		"pdb": map[string]any{
			"enabled":      input.PDBEnabled,
			"minAvailable": defaultPDBMinAvailable,
		},
		"probes": map[string]any{
			"startup": map[string]any{
				"enabled":          web,
				"httpGet":          map[string]any{"path": healthPath, "port": containerPortName},
				"periodSeconds":    startupProbePeriodSeconds,
				"failureThreshold": startupFailureThreshold(input.StartupTimeoutSeconds),
			},
			"readiness": map[string]any{
				"enabled":             web,
				"httpGet":             map[string]any{"path": readinessPath, "port": containerPortName},
				"initialDelaySeconds": readinessProbeInitialDelaySeconds,
				"periodSeconds":       readinessProbePeriodSeconds,
				"timeoutSeconds":      readinessProbeTimeoutSeconds,
				"failureThreshold":    readinessProbeFailureThreshold,
			},
			"liveness": map[string]any{
				"enabled":             web,
				"httpGet":             map[string]any{"path": healthPath, "port": containerPortName},
				"initialDelaySeconds": livenessProbeInitialDelaySeconds,
				"periodSeconds":       livenessProbePeriodSeconds,
				"timeoutSeconds":      livenessProbeTimeoutSeconds,
				"failureThreshold":    livenessProbeFailureThreshold,
			},
		},
		"networkPolicy": map[string]any{
			"enabled": true,
			// Réseau privé : les conteneurs du projet se joignent entre eux.
			"projectNetwork":      true,
			"ingressNSLabelKey":   namespaceNameLabel,
			"ingressNSLabelValue": fallback(input.IngressNamespace, defaultIngressNamespace),
			"egress": map[string]any{
				"enabled":     true,
				"allowDNS":    true,
				"publicPorts": defaultEgressPublicPorts,
			},
		},
		// tmpfs uniquement si rootfs read-only : sinon un emptyDir sur /var/run
		// masquerait le contenu de l'image sans rien apporter.
		"tmpfs": map[string]any{
			"enabled": input.ReadOnlyRootFS,
			"mounts": []map[string]any{
				{"name": tmpfsTmpName, "path": tmpfsTmpPath, "sizeLimit": tmpfsTmpSizeLimit},
				{"name": tmpfsRunName, "path": tmpfsRunPath, "sizeLimit": tmpfsRunSizeLimit},
			},
		},
		// Variables d'environnement : uniquement par le Secret <nom>-env
		// (envFrom). Avant, elles étaient aussi recopiées en clair dans la
		// spec du Deployment, lisible par qui peut lire les déploiements.
		"envSecretName": utils.EnvSecretName(input.ReleaseName),
		// Empreinte du contenu : change → nouveaux pods (le Secret seul ne
		// relance rien, et le nom du Secret, lui, ne change jamais).
		"envChecksum": strconv.FormatInt(input.EnvRevision, 10),
	}
	if len(proc.Command) > 0 {
		vals["command"] = proc.Command
	}
	if len(proc.Args) > 0 {
		vals["args"] = proc.Args
	}
	if len(proc.WaitFor) > 0 {
		vals["waitFor"] = map[string]any{"targets": strings.Join(proc.WaitFor, " "), "image": waitForImage}
	}
	if len(proc.ExtraPorts) > 0 && proc.Kind != "cron" {
		// Ports internes supplémentaires : ajoutés au Service du conteneur
		// (même nom DNS) et au conteneur ; jamais à un Ingress.
		ports := make([]map[string]any, 0, len(proc.ExtraPorts))
		for _, p := range proc.ExtraPorts {
			ports = append(ports, map[string]any{"name": p.Name, "port": p.Port})
		}
		vals["extraPorts"] = ports
	}
	if !web {
		vals["customDomains"] = []any{}
	}
	if proc.Kind == "service" {
		// Service interne : une instance (état sur disque), sondes sur le port
		// (TCP) ou par commande (ex. valkey-cli ping). Pas de readiness par
		// commande : le Service n'envoie du trafic qu'à un pod prêt, et le
		// port ouvert suffit à le dire.
		vals["replicaCount"] = 1
		vals["autoscaling"].(map[string]any)["enabled"] = false
		vals["pdb"].(map[string]any)["enabled"] = false
		vals["probes"] = serviceProbes(proc.HealthCommand, input.StartupTimeoutSeconds)
	}
	if proc.Kind == "worker" && len(proc.HealthCommand) > 0 {
		// Worker : pas de port, la sonde lance une commande. Démarrage toléré
		// comme pour un web (startup), puis vivacité ; pas de readiness (aucun
		// Service ne lui envoie de trafic).
		exec := map[string]any{"command": proc.HealthCommand}
		probes := vals["probes"].(map[string]any)
		// httpGet: nil retire la sonde HTTP par défaut du chart (values.yaml) :
		// Helm fusionne, et Kubernetes refuse deux types de sonde à la fois.
		probes["startup"] = map[string]any{"enabled": true, "exec": exec, "httpGet": nil, "periodSeconds": startupProbePeriodSeconds,
			"timeoutSeconds": livenessProbeTimeoutSeconds, "failureThreshold": startupFailureThreshold(input.StartupTimeoutSeconds)}
		probes["liveness"] = map[string]any{"enabled": true, "exec": exec, "httpGet": nil, "periodSeconds": livenessProbePeriodSeconds,
			"timeoutSeconds": livenessProbeTimeoutSeconds, "failureThreshold": livenessProbeFailureThreshold}
	}
	if proc.Kind == "cron" {
		maxDuration := proc.MaxDurationSeconds
		if maxDuration <= 0 {
			maxDuration = defaultCronMaxDurationSeconds
		}
		vals["cron"] = map[string]any{
			"schedule":              proc.Schedule,
			"timeZone":              proc.TimeZone,
			"suspend":               proc.Suspend,
			"activeDeadlineSeconds": maxDuration,
		}
		// Une exécution à la fois : ni autoscaling, ni PDB, une réplique.
		vals["replicaCount"] = 1
		vals["autoscaling"].(map[string]any)["enabled"] = false
		vals["pdb"].(map[string]any)["enabled"] = false
	}
	return vals
}

// serviceProbes : sondes d'un conteneur « service ». Démarrage et
// disponibilité : le port répond ; vivacité : la commande donnée, sinon le
// port aussi.
func serviceProbes(health []string, startupTimeout int) map[string]any {
	tcp := map[string]any{"port": serviceContainerPortName}
	// httpGet: nil : retire la sonde HTTP par défaut du chart (voir worker).
	live := map[string]any{"enabled": true, "tcpSocket": tcp, "httpGet": nil}
	if len(health) > 0 {
		live = map[string]any{"enabled": true, "exec": map[string]any{"command": health}, "httpGet": nil}
	}
	live["periodSeconds"] = livenessProbePeriodSeconds
	live["timeoutSeconds"] = livenessProbeTimeoutSeconds
	live["failureThreshold"] = livenessProbeFailureThreshold
	return map[string]any{
		"startup": map[string]any{"enabled": true, "tcpSocket": tcp, "httpGet": nil, "periodSeconds": startupProbePeriodSeconds,
			"failureThreshold": startupFailureThreshold(startupTimeout)},
		"readiness": map[string]any{"enabled": true, "tcpSocket": tcp, "httpGet": nil, "periodSeconds": readinessProbePeriodSeconds,
			"timeoutSeconds": readinessProbeTimeoutSeconds, "failureThreshold": readinessProbeFailureThreshold},
		"liveness": live,
	}
}

// waitForImage : conteneur d'initialisation qui attend les adresses WaitFor
// (nc -z de busybox). Étiquette figée.
const waitForImage = "busybox:1.37.0"

// serviceContainerPortName : nom du port d'un conteneur « service » (chart).
const serviceContainerPortName = "tcp"

// defaultCronMaxDurationSeconds : durée maximale d'une exécution (1 h).
const defaultCronMaxDurationSeconds = 3600

func defaultIfEmptyKind(k string) string {
	if k == "" {
		return "web"
	}
	return k
}

// defaultEgressPublicPorts : ports TCP sortants autorisés vers Internet
// (subnets privés exclus par le chart) : ceux de la politique réseau des
// projets (network/domain.EgressPorts), affichée au client.
var defaultEgressPublicPorts = netDomain.EgressPorts

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func fallbackInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// requestWithin : réservation demandée, sinon le défaut ramené à la limite.
// Kubernetes refuse une réservation supérieure à la limite : une limite basse
// (légitime) échouait en plein déploiement à cause de la valeur par défaut.
func requestWithin(requested, def, limit string) string {
	if requested != "" {
		return requested
	}
	d, err := resource.ParseQuantity(def)
	l, lerr := resource.ParseQuantity(limit)
	if err == nil && lerr == nil && l.Cmp(d) < 0 {
		return limit
	}
	return def
}

// DomainRoute : domaine personnalisé vérifié et chemin routé (« / » : tout).
type DomainRoute struct {
	Host string
	Path string
}

// customDomainValues : une entrée {host, path} par Ingress. hosts : entrée
// d'un workflow antérieur aux chemins (tout le domaine).
func customDomainValues(routes []DomainRoute, hosts []string) []any {
	out := make([]any, 0, len(routes)+len(hosts))
	if len(routes) == 0 {
		for _, h := range hosts {
			routes = append(routes, DomainRoute{Host: h, Path: "/"})
		}
	}
	for _, r := range routes {
		path := r.Path
		if path == "" {
			path = "/"
		}
		out = append(out, map[string]any{"host": r.Host, "path": path})
	}
	return out
}

// SyncEnvSecret : écrit le Secret Kubernetes depuis les valeurs chiffrées en
// base, lues ici : ni l'entrée ni le résultat de l'activité ne contiennent de
// valeur. Revision 0 : conteneur antérieur, son Secret reste tel quel.
func (a *WorkloadActivities) SyncEnvSecret(ctx context.Context, namespace, releaseName, projectID, workloadID string, revision int64) error {
	if revision == 0 {
		return nil
	}
	id, err := uuid.Parse(workloadID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid workload id", "INVALID_INPUT", err)
	}
	values, err := a.Variables.Values(ctx, id)
	if err != nil {
		return err
	}
	data := make(map[string][]byte, len(values))
	for k, v := range values {
		data[k] = []byte(v)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: utils.EnvSecretName(releaseName), Namespace: namespace,
			Labels: map[string]string{utils.LabelKmProjectID: projectID}},
		Data: data,
	}
	kube, err := a.forProject(ctx, projectID, namespace)
	if err != nil {
		return err
	}
	api := kube.Client.CoreV1().Secrets(namespace)
	if _, err := api.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err = api.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	}
	return nil
}

// DeleteEnvSecret : Secret des variables d'un conteneur supprimé.
func (a *WorkloadActivities) DeleteEnvSecret(ctx context.Context, namespace, releaseName string) error {
	kube, err := a.Kube.ForNamespace(ctx, namespace)
	if err != nil {
		return fmt.Errorf("project cluster: %w", err)
	}
	err = kube.Client.CoreV1().Secrets(namespace).Delete(ctx, utils.EnvSecretName(releaseName), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// EnsureRegistryPullSecret : Secret de pull du projet, avec le registre de la
// plateforme (limité aux images du projet) et les registres externes du
// projet. Les identifiants restent hors de l'historique Temporal : ils sont
// lus ici, côté worker.
func (a *WorkloadActivities) EnsureRegistryPullSecret(ctx context.Context, namespace, projectID string) error {
	if a.Pull == nil {
		return temporal.NewNonRetryableApplicationError("registry pull disabled", "REGISTRY_DISABLED", nil)
	}
	project, err := uuid.Parse(projectID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid project id", "INVALID_INPUT", err)
	}
	if err := a.Pull.Sync(ctx, namespace, project); err != nil {
		if errors.Is(err, pull.ErrNothingToPull) {
			// Accès externe retiré entre la demande et le déploiement.
			return temporal.NewNonRetryableApplicationError("no registry access for this project", "NO_REGISTRY_ACCESS", err)
		}
		return err
	}
	return nil
}

func identityValues(id *WorkloadIdentity) map[string]any {
	if id == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "serviceAccount": id.ServiceAccount, "audience": id.Audience,
		"tokenURI": id.TokenURI}
}

// startupFailureThreshold : échecs tolérés de la sonde de démarrage pour un
// délai donné (0 : le défaut, 150 s).
func startupFailureThreshold(timeoutSeconds int) int {
	if timeoutSeconds <= 0 {
		return startupProbeFailureThreshold
	}
	return (timeoutSeconds + startupProbePeriodSeconds - 1) / startupProbePeriodSeconds
}
