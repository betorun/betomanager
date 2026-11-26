package activities

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Marge pour le pod supplémentaire des mises à jour progressives.
func TestWithRollingMargin(t *testing.T) {
	for in, want := range map[string]string{"2": "4", "500m": "1", "4Gi": "8Gi", "1536Mi": "3Gi"} {
		got := withRollingMargin(resource.MustParse(in))
		assert.Zero(t, got.Cmp(resource.MustParse(want)), "%s → %s", in, got.String())
	}
}

// La passerelle du réseau privé est en plus de la limite du client.
func TestWithPlatformReserve(t *testing.T) {
	cpu, mem := withPlatformReserve(resource.MustParse("1"), resource.MustParse("1Gi"))
	assert.Zero(t, cpu.Cmp(resource.MustParse("1200m")), cpu.String())
	assert.Zero(t, mem.Cmp(resource.MustParse("1088Mi")), mem.String())
}
