package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
)

// Prober : teste un accès auprès du registre (API /v2/ et son défi
// d'authentification). Il ne parle qu'en HTTPS à des adresses publiques :
// sinon l'API servirait à sonder le réseau interne (SSRF).
type Prober struct {
	Client *http.Client
}

const (
	probeTimeout = 10 * time.Second
	maxProbeBody = 64 << 10
	maxRedirects = 3
)

// NewProber : client qui refuse les adresses internes, au moment de la
// connexion (une résolution DNS ne peut pas le contourner).
func NewProber() *Prober {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: refusePrivate}
	return &Prober{Client: &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second,
			Proxy: nil, MaxIdleConns: 10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects || req.URL.Scheme != "https" {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}}
}

var errPrivateAddress = errors.New("registries: refusing to contact a private address")

// sharedAddressSpace : 100.64.0.0/10 (CGNAT), que IsPrivate ne couvre pas.
var sharedAddressSpace = net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || sharedAddressSpace.Contains(ip) {
		return errPrivateAddress
	}
	return nil
}

// apiHost : hôte de l'API du registre (Docker Hub sert docker.io ailleurs).
func apiHost(host string) string {
	if host == domain.DockerHub {
		return "registry-1.docker.io"
	}
	return host
}

// Check : ok, denied ou unreachable. Jamais d'erreur détaillée : le message
// d'un registre tiers n'est pas montré au client.
func (p *Prober) Check(ctx context.Context, a domain.Auth) string {
	base := "https://" + apiHost(a.Host) + "/v2/"
	res, err := p.get(ctx, base, nil)
	if err != nil {
		return domain.TestUnreachable
	}
	switch res.status {
	case http.StatusOK:
		// Registre ouvert : rien ne permet de juger les identifiants ; on
		// vérifie qu'ils ne sont pas refusés.
		auth, err := p.get(ctx, base, &a)
		if err != nil {
			return domain.TestUnreachable
		}
		return verdict(auth.status)
	case http.StatusUnauthorized:
	default:
		return domain.TestUnreachable
	}

	scheme, params := parseChallenge(res.challenge)
	switch scheme {
	case "basic":
		auth, err := p.get(ctx, base, &a)
		if err != nil {
			return domain.TestUnreachable
		}
		return verdict(auth.status)
	case "bearer":
		realm, err := url.Parse(params["realm"])
		if err != nil || realm.Scheme != "https" || realm.Host == "" {
			return domain.TestUnreachable
		}
		q := realm.Query()
		if svc := params["service"]; svc != "" {
			q.Set("service", svc)
		}
		realm.RawQuery = q.Encode()
		tok, err := p.get(ctx, realm.String(), &a)
		if err != nil {
			return domain.TestUnreachable
		}
		return verdict(tok.status)
	default:
		return domain.TestUnreachable
	}
}

func verdict(status int) string {
	switch {
	case status >= 200 && status < 300:
		return domain.TestOK
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return domain.TestDenied
	default:
		return domain.TestUnreachable
	}
}

type probeResult struct {
	status    int
	challenge string
}

func (p *Prober) get(ctx context.Context, rawURL string, a *domain.Auth) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("User-Agent", "betorun-registry-check")
	if a != nil {
		req.SetBasicAuth(a.Username, a.Password)
	}
	res, err := p.Client.Do(req)
	if err != nil {
		return probeResult{}, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxProbeBody))
	return probeResult{status: res.StatusCode, challenge: res.Header.Get("WWW-Authenticate")}, nil
}

// parseChallenge : `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		var part string
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			part, rest = after[1:1+end], after[2+end:]
		} else {
			part, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(key))] = part
	}
	return strings.ToLower(scheme), params
}
