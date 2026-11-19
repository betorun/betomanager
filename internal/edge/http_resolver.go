package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tokens : jetons d'accès à l'API (fichier de clé du compte système du
// cluster, échangé contre des jetons courts).
type Tokens interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// HTTPResolver : l'edge du cluster demande à l'API quel site servir. Il ne
// joint la plateforme qu'en HTTPS ; la clé du cluster ne vaut que pour lui.
type HTTPResolver struct {
	BaseURL   string // …/kmanager/v1
	ClusterID string
	Tokens    Tokens
	Client    *http.Client
}

// edgeSite : réponse de GET /clusters/EdgeService/:clusterID/Resolve.
type edgeSite struct {
	SiteID       string `json:"site_id"`
	DeploymentID string `json:"deployment_id"`
	SPAFallback  bool   `json:"spa_fallback"`
	Suspended    bool   `json:"suspended"`
}

const (
	resolveTimeout  = 5 * time.Second
	maxResolveBytes = 4096
)

func (r *HTTPResolver) Resolve(ctx context.Context, host string) (*Site, error) {
	u := strings.TrimRight(r.BaseURL, "/") + "/clusters/EdgeService/" + url.PathEscape(r.ClusterID) +
		"/Resolve?host=" + url.QueryEscape(host)
	for attempt := 0; ; attempt++ {
		site, status, err := r.get(ctx, u)
		// Jeton refusé (expiré, clé tournée) : un nouveau, une fois.
		if status == http.StatusUnauthorized && attempt == 0 {
			r.Tokens.Invalidate()
			continue
		}
		return site, err
	}
}

func (r *HTTPResolver) get(ctx context.Context, u string) (*Site, int, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	token, err := r.Tokens.Token(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("edge token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResolveBytes))
	switch resp.StatusCode {
	case http.StatusOK:
		var s edgeSite
		if err := json.Unmarshal(body, &s); err != nil || s.SiteID == "" || s.DeploymentID == "" {
			return nil, resp.StatusCode, fmt.Errorf("edge resolve: invalid response")
		}
		return &Site{ID: s.SiteID, DeploymentID: s.DeploymentID, SPAFallback: s.SPAFallback, Suspended: s.Suspended},
			resp.StatusCode, nil
	case http.StatusNotFound:
		return nil, resp.StatusCode, nil
	default:
		return nil, resp.StatusCode, fmt.Errorf("edge resolve: HTTP %d", resp.StatusCode)
	}
}
