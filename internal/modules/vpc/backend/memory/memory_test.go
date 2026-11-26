package memory

import (
	"testing"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
	"github.com/thekrauss/kubemanager/internal/modules/vpc/vpctest"
)

// Les deux formes d'une mise en œuvre : avec inscription (tunnel) et sans
// (réseau natif) passent le même contrat.
func TestContract_WithEnrollment(t *testing.T) {
	vpctest.RunBackendContract(t, func(*testing.T) domain.VPCBackend { return &Backend{Enrollment: true} })
}

func TestContract_Native(t *testing.T) {
	vpctest.RunBackendContract(t, func(*testing.T) domain.VPCBackend { return &Backend{} })
}

func TestContract_Firewall(t *testing.T) {
	vpctest.RunFirewallContract(t, func(*testing.T) domain.FirewallProvider { return &Firewall{MaxRules: 4} })
}
