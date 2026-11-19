package domain

import "strings"

// Cause d'un échec de récupération d'image (ADR 0011) : la console et la CLI
// disent « ghcr.io refuse l'accès » ou « image introuvable » plutôt que le
// seul statut IMAGE_PULL_FAILED.
const (
	PullDenied      = "denied"              // identifiants absents, faux ou sans droit
	PullNotFound    = "not_found"           // image ou étiquette inexistante
	PullDeniedOrNot = "denied_or_not_found" // Docker Hub ne distingue pas les deux
	PullUnreachable = "unreachable"         // registre injoignable (DNS, réseau, TLS)
	PullInvalid     = "invalid"             // nom d'image mal formé
	PullUnknown     = "unknown"
)

// PullFailure : pourquoi l'image d'un conteneur ne se récupère pas.
type PullFailure struct {
	Cause    string `json:"cause"`
	Registry string `json:"registry"`
	Image    string `json:"image"`
	// Le message brut du pod n'est jamais renvoyé : il peut contenir des
	// informations sensibles (ADR 0011).
}

// Raisons Kubernetes d'un conteneur en attente de son image.
const (
	ReasonErrImagePull     = "ErrImagePull"
	ReasonImagePullBackOff = "ImagePullBackOff"
	ReasonInvalidImageName = "InvalidImageName"
)

// IsPullWaiting : le conteneur attend une image qu'il n'arrive pas à récupérer.
func IsPullWaiting(reason string) bool {
	return reason == ReasonErrImagePull || reason == ReasonImagePullBackOff || reason == ReasonInvalidImageName
}

// pullPatterns : morceaux de messages de containerd et des registres, du plus
// précis au plus général (le premier trouvé l'emporte).
var pullPatterns = []struct {
	cause string
	parts []string
}{
	{PullDeniedOrNot, []string{"repository does not exist or may require"}},
	{PullNotFound, []string{"manifest unknown", "not found", "name unknown", "404"}},
	{PullDenied, []string{"401", "403", "unauthorized", "forbidden", "denied", "authentication required", "insufficient_scope", "no basic auth credentials"}},
	{PullUnreachable, []string{"no such host", "i/o timeout", "connection refused", "connection reset", "tls handshake", "x509", "deadline exceeded", "network is unreachable", "server misbehaving"}},
}

// ClassifyPull : cause d'un échec à partir de la raison et du message de
// Kubernetes.
func ClassifyPull(image, reason, message string) PullFailure {
	out := PullFailure{Cause: PullUnknown, Registry: RegistryOf(image), Image: image}
	if reason == ReasonInvalidImageName {
		out.Cause = PullInvalid
		return out
	}
	m := strings.ToLower(message)
	for _, p := range pullPatterns {
		for _, part := range p.parts {
			if strings.Contains(m, part) {
				out.Cause = p.cause
				return out
			}
		}
	}
	return out
}

// RegistryOf : registre d'une référence d'image (docker.io par défaut).
func RegistryOf(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io"
	}
	return first
}
