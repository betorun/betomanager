package activities

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/thekrauss/kubemanager/internal/infrastructure/helm/charts"
)

// renderKind : valeurs posées par l'API (buildHelmValues) rendues par le
// chart embarqué ; rend tout le YAML produit, concaténé.
func renderKind(t *testing.T, in InstallWorkloadInput) string {
	t.Helper()
	c, err := charts.StandardApp()
	if err != nil {
		t.Fatal(err)
	}
	vals, err := chartutil.ToRenderValues(c, buildHelmValues(in),
		chartutil.ReleaseOptions{Name: in.ReleaseName, Namespace: "proj-1", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	out, err := engine.Render(c, vals)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for name, body := range out {
		if strings.HasSuffix(name, ".yaml") && strings.TrimSpace(body) != "" {
			b.WriteString("---\n# " + name + "\n" + body + "\n")
		}
	}
	return b.String()
}

func baseInput(kind string) InstallWorkloadInput {
	return InstallWorkloadInput{
		ReleaseName: "job", ImageRepo: "registry.example.org/p/job", ImageTag: "1", TargetPort: 8080,
		ExternalURL: "job.apps.example.org", Replicas: 1,
		Process: ProcessSpec{Kind: kind, Command: []string{"python"}, Args: []string{"worker.py"}},
	}
}

func TestChart_WebRendersServiceIngressAndProbes(t *testing.T) {
	y := renderKind(t, baseInput("web"))
	for _, want := range []string{"kind: Deployment", "kind: Service", "kind: Ingress", "readinessProbe", "containerPort: 8080"} {
		if !strings.Contains(y, want) {
			t.Fatalf("web : %q absent\n%s", want, y)
		}
	}
	if strings.Contains(y, "kind: CronJob") {
		t.Fatal("web : CronJob rendu")
	}
}

func TestChart_WorkerHasNoPortServiceOrProbe(t *testing.T) {
	y := renderKind(t, baseInput("worker"))
	if !strings.Contains(y, "kind: Deployment") {
		t.Fatalf("worker sans Deployment\n%s", y)
	}
	for _, banned := range []string{"kind: Service\n", "kind: Ingress", "containerPort", "readinessProbe", "livenessProbe", "kind: CronJob"} {
		if strings.Contains(y, banned) {
			t.Fatalf("worker : %q rendu\n%s", banned, y)
		}
	}
	if !strings.Contains(y, "- python") || !strings.Contains(y, "- worker.py") {
		t.Fatalf("worker : commande absente\n%s", y)
	}
}

func TestChart_CronRendersCronJobOnly(t *testing.T) {
	in := baseInput("cron")
	in.Process.Schedule, in.Process.TimeZone, in.Process.Suspend = "0 2 * * *", "Africa/Dakar", true
	y := renderKind(t, in)
	for _, want := range []string{
		"kind: CronJob", `schedule: "0 2 * * *"`, `timeZone: "Africa/Dakar"`, "suspend: true",
		"concurrencyPolicy: Forbid", "restartPolicy: Never", "activeDeadlineSeconds: 3600", "- python",
	} {
		if !strings.Contains(y, want) {
			t.Fatalf("cron : %q absent\n%s", want, y)
		}
	}
	for _, banned := range []string{"kind: Deployment", "kind: Service\n", "kind: Ingress", "HorizontalPodAutoscaler", "PodDisruptionBudget", "containerPort", "readinessProbe"} {
		if strings.Contains(y, banned) {
			t.Fatalf("cron : %q rendu\n%s", banned, y)
		}
	}
}

func TestChart_CronEnvChecksumOnPodTemplate(t *testing.T) {
	in := baseInput("cron")
	in.Process.Schedule = "@daily"
	y := renderKind(t, in)
	// revisionBehind lit l'annotation sur le gabarit du pod du CronJob.
	if !strings.Contains(y, "checksum/env") {
		t.Fatalf("cron : annotation checksum/env absente\n%s", y)
	}
}

// Domaine à chemin : Ingress propre à la route ; la route « / » garde le nom
// d'avant les chemins (pas de nouveau certificat à la mise à jour).
func TestChart_CustomDomainPaths(t *testing.T) {
	in := baseInput("web")
	in.CustomRoutes = []DomainRoute{{Host: "backoffice.exemple.sn", Path: "/"}, {Host: "backoffice.exemple.sn", Path: "/auth"}}
	in.CustomDomainTLS = true
	y := renderKind(t, in)
	if strings.Count(y, "# standard-app/templates/custom-domains.yaml") != 1 || strings.Count(y, "host: \"backoffice.exemple.sn\"") != 2 {
		t.Fatalf("deux routes attendues\n%s", y)
	}
	if !strings.Contains(y, `path: "/auth"`) || !strings.Contains(y, `path: "/"`) {
		t.Fatalf("chemins absents\n%s", y)
	}
	legacy := baseInput("web")
	legacy.CustomDomains = []string{"backoffice.exemple.sn"}
	old := renderKind(t, legacy)
	nameOf := func(doc string) string {
		i := strings.Index(doc, "name: job-d-")
		return doc[i : i+len("name: job-d-")+10]
	}
	if nameOf(old) != nameOf(y) {
		t.Fatalf("la route « / » a changé de nom : %s ≠ %s", nameOf(old), nameOf(y))
	}
}

// Réseau privé : entrée ET sortie ouvertes entre pods du même projet, pour
// toutes les sortes (un worker appelle l'API, un site appelle Keycloak).
func TestChart_ProjectPrivateNetwork(t *testing.T) {
	for _, kind := range []string{"web", "worker", "cron"} {
		in := baseInput(kind)
		in.ProjectID = "11111111-1111-1111-1111-111111111111"
		if kind == "cron" {
			in.Process.Schedule = "@daily"
		}
		y := renderKind(t, in)
		i := strings.Index(y, "kind: NetworkPolicy")
		if i < 0 {
			t.Fatalf("%s : pas de NetworkPolicy", kind)
		}
		np := y[i:]
		if j := strings.Index(np, "\n---"); j > 0 {
			np = np[:j]
		}
		np = np[strings.Index(np, "spec:"):] // hors labels de la politique elle-même
		sel := `km-project-id: "11111111-1111-1111-1111-111111111111"`
		if strings.Count(np, sel) != 2 {
			t.Fatalf("%s : règle d'entrée et de sortie du projet attendues\n%s", kind, np)
		}
		if !strings.Contains(np, "app.kubernetes.io/part-of: kubemanager") {
			t.Fatalf("%s : sélecteur sans part-of\n%s", kind, np)
		}
	}
	// Sans projet connu : aucune règle « projet » (un label vide viserait tout).
	y := renderKind(t, baseInput("web"))
	if strings.Contains(y, "Réseau privé du projet") {
		t.Fatal("règle projet rendue sans projet")
	}
}
