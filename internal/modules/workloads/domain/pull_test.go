package domain

import "testing"

func TestClassifyPull(t *testing.T) {
	cases := []struct {
		name, image, reason, message, cause, registry string
	}{
		{"ghcr sans droit", "ghcr.io/acme/api:1", ReasonErrImagePull,
			`failed to pull and unpack image "ghcr.io/acme/api:1": failed to resolve reference "ghcr.io/acme/api:1": failed to authorize: failed to fetch anonymous token: unexpected status from GET request to https://ghcr.io/token?scope=repository%3Aacme%2Fapi%3Apull&service=ghcr.io: 401 Unauthorized`,
			PullDenied, "ghcr.io"},
		{"étiquette absente", "ghcr.io/acme/api:nope", ReasonErrImagePull,
			`failed to pull and unpack image "ghcr.io/acme/api:nope": failed to resolve reference "ghcr.io/acme/api:nope": ghcr.io/acme/api:nope: not found`,
			PullNotFound, "ghcr.io"},
		{"Docker Hub ambigu", "acme/private", ReasonErrImagePull,
			`failed to pull and unpack image "docker.io/acme/private:latest": failed to resolve reference "docker.io/acme/private:latest": pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed`,
			PullDeniedOrNot, "docker.io"},
		{"registre injoignable", "registry.acme.test:5000/api:1", ReasonErrImagePull,
			`failed to do request: Head "https://registry.acme.test:5000/v2/api/manifests/1": dial tcp: lookup registry.acme.test: no such host`,
			PullUnreachable, "registry.acme.test:5000"},
		{"nom invalide", "Acme/API", ReasonInvalidImageName, `couldn't parse image reference "Acme/API"`, PullInvalid, "docker.io"},
		{"back-off sans détail", "nginx:1", ReasonImagePullBackOff, `Back-off pulling image "nginx:1"`, PullUnknown, "docker.io"},
		{"localhost", "localhost/api:1", ReasonErrImagePull, "connection refused", PullUnreachable, "localhost"},
	}
	for _, c := range cases {
		got := ClassifyPull(c.image, c.reason, c.message)
		if got.Cause != c.cause || got.Registry != c.registry {
			t.Errorf("%s : %s/%s, attendu %s/%s", c.name, got.Cause, got.Registry, c.cause, c.registry)
		}
	}
}
