package ovhvps

// Essai réel, en lecture seule, contre le compte OVH des VPS. Ignoré sans
// OVH_LIVE=1 et les variables KUBEMANAGER_COMPUTE_OVH_* :
//
//	set -a; . ./.env; set +a; OVH_LIVE=1 go test ./internal/modules/compute/repository/ovhvps -run TestLive -v

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
)

func TestLiveList(t *testing.T) {
	if os.Getenv("OVH_LIVE") == "" {
		t.Skip("OVH_LIVE not set")
	}
	p, err := New(ovh.Credentials{
		Endpoint:          os.Getenv("KUBEMANAGER_COMPUTE_OVH_ENDPOINT"),
		ApplicationKey:    os.Getenv("KUBEMANAGER_COMPUTE_OVH_APPLICATION_KEY"),
		ApplicationSecret: os.Getenv("KUBEMANAGER_COMPUTE_OVH_APPLICATION_SECRET"),
		ConsumerKey:       os.Getenv("KUBEMANAGER_COMPUTE_OVH_CONSUMER_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	list, err := p.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range list {
		t.Logf("%s : %s (%s) busy=%v plan=%s zone=%s v4=%s v6=%s", vm.ProviderID, vm.State, vm.RawState, vm.Busy, vm.Plan, vm.Zone, vm.IPv4, vm.IPv6)
	}
	if len(list) == 0 {
		t.Log("aucun VPS sur le compte (lot 0 : commander un VPS-1 de test)")
	}
}
