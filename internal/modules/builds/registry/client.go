package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Action de suppression d'un manifeste (API du registre).
const ActionDelete = "delete"

// platformUser : sujet des jetons que l'API se délivre pour gérer le registre.
const platformUser = "platform"

// Types de manifestes lus (image simple ou index multi-plateforme / avec
// attestations, comme BuildKit les pousse).
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// maxManifestBytes : un manifeste plus gros est refusé.
const maxManifestBytes = 4 << 20

// ErrNotFound : manifeste absent du registre.
var ErrNotFound = errors.New("registry: manifest not found")

// Client : appels de l'API au registre de la plateforme (taille et
// suppression des images), avec des jetons qu'elle signe elle-même.
type Client struct {
	// BaseURL : registre vu par l'API (http://127.0.0.1:5000, https://registry…).
	BaseURL string
	Issuer  *Issuer
	HTTP    *http.Client
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    *descriptor  `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"`
}

// ImageSize : taille compressée de l'image (couches et configuration ; pour
// un index, celle de chaque manifeste qu'il contient). Une couche commune à
// plusieurs plateformes est comptée une fois.
func (c *Client) ImageSize(ctx context.Context, project uuid.UUID, repo, digest string) (int64, error) {
	tok, err := c.token(project, repo, ActionPull)
	if err != nil {
		return 0, err
	}
	root, err := c.manifest(ctx, tok, repo, digest)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	var total int64
	add := func(m *manifest) {
		if m.Config != nil && !seen[m.Config.Digest] {
			seen[m.Config.Digest] = true
			total += m.Config.Size
		}
		for _, l := range m.Layers {
			if !seen[l.Digest] {
				seen[l.Digest] = true
				total += l.Size
			}
		}
	}
	add(root)
	for _, child := range root.Manifests {
		m, err := c.manifest(ctx, tok, repo, child.Digest)
		if err != nil {
			return 0, err
		}
		add(m)
	}
	return total, nil
}

// Delete : retire le manifeste (et ses étiquettes). Les couches ne sont
// libérées qu'au prochain ménage du registre (garbage-collect). Absent : nil.
func (c *Client) Delete(ctx context.Context, project uuid.UUID, repo, digest string) error {
	tok, err := c.token(project, repo, ActionDelete)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(repo, digest), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch res.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("registry: delete %s@%s: status %d", repo, digest, res.StatusCode)
}

func (c *Client) manifest(ctx context.Context, tok, repo, digest string) (*manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(repo, digest), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", manifestAccept)
	res, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry: manifest %s@%s: status %d", repo, digest, res.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(res.Body, maxManifestBytes)).Decode(&m); err != nil {
		return nil, fmt.Errorf("registry: manifest %s@%s: %w", repo, digest, err)
	}
	return &m, nil
}

func (c *Client) token(project uuid.UUID, repo, action string) (string, error) {
	if c.Issuer == nil {
		return "", errors.New("registry: no token issuer")
	}
	tok, _, err := c.Issuer.Issue(platformUser, project, []string{action},
		[]string{repositoryScope + ":" + repo + ":" + action}, time.Now())
	return tok, err
}

func (c *Client) url(repo, digest string) string {
	return strings.TrimRight(c.BaseURL, "/") + "/v2/" + repo + "/manifests/" + digest
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Repository : dépôt d'une image du registre (<projet>/<nom>), sans hôte ni
// étiquette ; vide si l'image n'est pas de ce registre.
func Repository(host, image string) string {
	if !IsPlatformImage(host, image) {
		return ""
	}
	ref := strings.TrimPrefix(strings.TrimSpace(image), host+"/")
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		ref = ref[:i]
	}
	return ref
}
