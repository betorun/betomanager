package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Registry : accès du projet à un registre externe (jamais le jeton).
type Registry struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RegistryTest : ok, denied ou unreachable.
type RegistryTest struct {
	Result string `json:"result"`
}

func registriesPath(project string) string {
	return pathProjects + "/" + url.PathEscape(project) + "/registries"
}

// ListRegistries : accès du projet.
func (c *Client) ListRegistries(ctx context.Context, project string) ([]Registry, error) {
	var out struct {
		Registries []Registry `json:"registries"`
	}
	return out.Registries, c.Do(ctx, http.MethodGet, registriesPath(project), nil, &out)
}

// PutRegistry : crée ou remplace l'accès.
func (c *Client) PutRegistry(ctx context.Context, project, host, username, password string) (*Registry, error) {
	var out Registry
	body := map[string]string{"username": username, "password": password}
	return &out, c.Do(ctx, http.MethodPut, registriesPath(project)+"/"+url.PathEscape(host), body, &out)
}

// DeleteRegistry : retire l'accès.
func (c *Client) DeleteRegistry(ctx context.Context, project, host string) error {
	return c.Do(ctx, http.MethodDelete, registriesPath(project)+"/"+url.PathEscape(host), nil, nil)
}

// TestRegistry : vérifie l'accès auprès du registre.
func (c *Client) TestRegistry(ctx context.Context, project, host string) (*RegistryTest, error) {
	var out RegistryTest
	return &out, c.Do(ctx, http.MethodPost, registriesPath(project)+"/"+url.PathEscape(host)+"/test", nil, &out)
}
