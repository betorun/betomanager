package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// IP de sortie dédiée : prix mensuel de la grille, à l'heure, plafonné au
// mois ; jamais dans le code (0 : gratuite).
func TestPrice_EgressIP(t *testing.T) {
	hour := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	c := Catalog{Version: "v1", Currency: "XAF", EgressIP: FromUnits(7300)}
	r := Reservation{Kind: KindEgressIP, OrganizationID: uuid.New(), ProjectID: uuid.New(), ResourceID: uuid.New(), ResourceName: "203.0.113.7"}
	lines := c.Price(r, hour)
	if len(lines) != 1 || lines[0].Meter != MeterEgressIP || lines[0].Quantity != 1 || lines[0].UnitPrice != PerHour(FromUnits(7300)) {
		t.Fatalf("lignes : %+v", lines)
	}
	if lines[0].ResourceName != "203.0.113.7" {
		t.Fatalf("ressource : %q", lines[0].ResourceName)
	}
	if !MeterEgressIP.Hourly() {
		t.Fatal("compteur horaire")
	}
	u, err := CatalogFromUpdate(CatalogUpdate{Currency: "XAF", Sizes: []SizeView{{ID: "mini", CPUMilli: 250, MemMi: 256, Monthly: 1000}}, EgressIP: 7300})
	if err != nil || u.EgressIP != FromUnits(7300) || u.Update().EgressIP != 7300 || u.View().EgressIP != 7300 {
		t.Fatalf("grille : %+v %v", u, err)
	}
	if _, err := CatalogFromUpdate(CatalogUpdate{Currency: "XAF", Sizes: []SizeView{{ID: "mini", CPUMilli: 250, MemMi: 256, Monthly: 1000}}, EgressIP: -1}); err == nil {
		t.Fatal("prix négatif refusé")
	}
}
