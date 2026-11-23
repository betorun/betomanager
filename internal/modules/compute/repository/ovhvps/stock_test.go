package ovhvps

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

func TestStock(t *testing.T) {
	p, _ := fakeOrder(t, map[string]string{
		"GET /vps/order/rule/datacenter?ovhSubsidiary=FR&planCode=vps-2025-model1": `{"datacenters":[
			{"datacenter":"GRA","code":"gra-1","status":"available","linuxStatus":"available","daysBeforeDelivery":0},
			{"datacenter":"RBX","code":"rbx","status":"available","linuxStatus":"out-of-stock","daysBeforeDelivery":0},
			{"datacenter":"SBG","code":"sbg","status":"out-of-stock-preorder-allowed","linuxStatus":"out-of-stock-preorder-allowed","daysBeforeDelivery":5},
			{"datacenter":"BHS","code":"bhs","status":"available","linuxStatus":"available","daysBeforeDelivery":0}]}`,
	})
	rows, err := p.Stock(context.Background(), "vps-2025-model1")
	require.NoError(t, err)
	assert.Equal(t, []service.ProviderStock{
		{Location: domain.LocationGRA, Status: domain.StockAvailable},
		{Location: domain.LocationRBX, Status: domain.StockOutOfStock}, // statut Linux, pas le statut général
		{Location: domain.LocationSBG, Status: domain.StockPreorder, Days: 5},
	}, rows, "emplacements non vendus ignorés")
}
