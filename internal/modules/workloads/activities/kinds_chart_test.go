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
