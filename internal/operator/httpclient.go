package operator

import (
	"net/http"
	"time"
)

// apiTransport : connexions vers l'API gardées ouvertes et partagées (outils,
// identité, cartes, journal) : les lectures parallèles d'un tour ne rouvrent
// pas une connexion TLS chacune (le transport par défaut n'en garde que 2
// par hôte).
var apiTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 16
	t.IdleConnTimeout = 90 * time.Second
	return t
}()

// apiClient : client de l'API, délai par requête.
func apiClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: apiTransport, Timeout: timeout}
}

// Clients par défaut (un par délai), créés une fois.
var (
	authClient    = apiClient(10 * time.Second)
	toolsClient   = apiClient(30 * time.Second)
	journalClient = apiClient(20 * time.Second)
)
