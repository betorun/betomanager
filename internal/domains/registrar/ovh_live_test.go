package registrar

// Essai réel contre l'API OVH (aucune commande : panier jetable). Ignoré
// sans OVH_LIVE=1 et les variables KUBEMANAGER_DOMAINS_OVH_* :
//
//	set -a; . ./.env; set +a; OVH_LIVE=1 go test ./internal/domains/registrar -run TestLive -v

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveCheck(t *testing.T) {
	if os.Getenv("OVH_LIVE") == "" {
		t.Skip("OVH_LIVE not set")
	}
	o, err := NewOVH(OVHConfig{
		Endpoint:          os.Getenv("KUBEMANAGER_DOMAINS_OVH_ENDPOINT"),
		ApplicationKey:    os.Getenv("KUBEMANAGER_DOMAINS_OVH_APPLICATION_KEY"),
		ApplicationSecret: os.Getenv("KUBEMANAGER_DOMAINS_OVH_APPLICATION_SECRET"),
		ConsumerKey:       os.Getenv("KUBEMANAGER_DOMAINS_OVH_CONSUMER_KEY"),
		Subsidiary:        os.Getenv("KUBEMANAGER_DOMAINS_OVH_SUBSIDIARY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, d := range []string{"google.com", "betorun-essai-" + time.Now().Format("060102150405") + ".com"} {
		a, err := o.Check(ctx, d)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		t.Logf("%s : disponible=%v premium=%v prix=%d centimes %s", d, a.Available, a.Premium, a.PriceCents, a.Currency)
	}
}
