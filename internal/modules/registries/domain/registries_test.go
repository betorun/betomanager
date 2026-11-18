package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io":                  "ghcr.io",
		" GHCR.io ":                "ghcr.io",
		"registry.gitlab.com":      "registry.gitlab.com",
		"index.docker.io":          DockerHub,
		"registry-1.docker.io":     DockerHub,
		"my.registry.example:5000": "my.registry.example:5000",
		"https://ghcr.io":          "",
		"ghcr.io/org":              "",
		"user:pw@ghcr.io":          "",
		"localhost":                "",
		"":                         "",
		"-bad.example":             "",
	} {
		assert.Equal(t, want, NormalizeHost(in), in)
	}
}

// Règles de Docker : sans point ni port dans le premier segment, l'image vient
// de Docker Hub.
func TestImageHost(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                                  DockerHub,
		"nginx:1.27":                             DockerHub,
		"moi/app:v2":                             DockerHub,
		"docker.io/moi/app":                      DockerHub,
		"ghcr.io/org/app:v3":                     "ghcr.io",
		"registry.gitlab.com/g/p/app@sha256:abc": "registry.gitlab.com",
		"my.registry.example:5000/app":           "my.registry.example:5000",
	} {
		assert.Equal(t, want, ImageHost(in), in)
	}
}

func TestValidateCredentials(t *testing.T) {
	assert.NoError(t, ValidateCredentials("moi", "ghp_x"))
	assert.ErrorIs(t, ValidateCredentials("", "x"), ErrInvalidUsername)
	assert.ErrorIs(t, ValidateCredentials("a:b", "x"), ErrInvalidUsername, "le « : » casserait l'en-tête Basic")
	assert.ErrorIs(t, ValidateCredentials("moi", ""), ErrInvalidPassword)
	assert.ErrorIs(t, ValidateCredentials("moi", "a\nb"), ErrInvalidPassword)
}

// Docker Hub est attendu sous son ancienne adresse dans un Secret de pull.
func TestConfigKey(t *testing.T) {
	assert.Equal(t, "https://index.docker.io/v1/", Auth{Host: DockerHub}.ConfigKey())
	assert.Equal(t, "ghcr.io", Auth{Host: "ghcr.io"}.ConfigKey())
}
