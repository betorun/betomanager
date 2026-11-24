package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Stockage objet : Go occupés (arrondis au-dessus) × prix mensuel de la
// grille, à l'heure ; jamais dans le code, absent : gratuit ; vide : rien.
func TestPrice_Storage(t *testing.T) {
	hour := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	c := Catalog{Version: "v1", Currency: "XAF", StorageGBMonthly: FromUnits(730)}
	r := Reservation{Kind: KindBucket, OrganizationID: uuid.New(), ProjectID: uuid.New(), ResourceID: uuid.New(), ResourceName: "uploads", DiskGi: 3}
	lines := c.Price(r, hour)
	if len(lines) != 1 || lines[0].Meter != MeterStorage || lines[0].Quantity != 3 || lines[0].UnitPrice != PerHour(FromUnits(730)) ||
		lines[0].Amount != 3*PerHour(FromUnits(730)) {
		t.Fatalf("lignes : %+v", lines)
	}
	r.DiskGi = 0
	if got := c.Price(r, hour); len(got) != 0 {
		t.Fatalf("bucket vide facturé : %+v", got)
	}
	free := Catalog{Version: "v1", Currency: "XAF"}
	r.DiskGi = 2
	if got := free.Price(r, hour); len(got) != 1 || got[0].Amount != 0 {
		t.Fatalf("grille sans prix : %+v", got)
	}
	base := CatalogUpdate{Currency: "XAF", Sizes: []SizeView{{ID: "mini", CPUMilli: 250, MemMi: 256, Monthly: 1000}}, StorageGBMonthly: 730}
	u, err := CatalogFromUpdate(base)
	if err != nil || u.StorageGBMonthly != FromUnits(730) || u.Update().StorageGBMonthly != 730 || u.View().StorageGBMonthly != 730 {
		t.Fatalf("grille : %+v %v", u, err)
	}
	base.StorageGBMonthly = -1
	if _, err := CatalogFromUpdate(base); err == nil {
		t.Fatal("prix négatif accepté")
	}
}
