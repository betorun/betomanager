package ovhvps

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

var _ service.NetworkProvider = (*Provider)(nil)

// ipBlock : bloc d'une adresse seule (/32 ou /128), forme des routes /ip.
func ipBlock(ip string) string {
	if strings.Contains(ip, ":") {
		return url.PathEscape(ip + "/128")
	}
	return url.PathEscape(ip + "/32")
}

// SetReverse : le fournisseur vérifie que le nom pointe vers l'adresse.
func (p *Provider) SetReverse(ctx context.Context, ip, reverse string) error {
	base := "/ip/" + ipBlock(ip) + "/reverse"
	if reverse == "" {
		err := p.api.Call(ctx, http.MethodDelete, base+"/"+url.PathEscape(ip), nil, nil)
		if ovh.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !strings.HasSuffix(reverse, ".") {
		reverse += "."
	}
	return p.api.Call(ctx, http.MethodPost, base, map[string]any{"ipReverse": ip, "reverse": reverse}, nil)
}

// SpamBlocked : état anti-spam de l'adresse.
func (p *Provider) SpamBlocked(ctx context.Context, ipv4 string) (bool, time.Time, error) {
	var s struct {
		State string    `json:"state"`
		Date  time.Time `json:"date"`
	}
	err := p.api.Call(ctx, http.MethodGet, "/ip/"+ipBlock(ipv4)+"/spam/"+url.PathEscape(ipv4), nil, &s)
	if ovh.IsNotFound(err) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	return s.State == "blockedForSpam", s.Date, nil
}

func (p *Provider) UnblockSpam(ctx context.Context, ipv4 string) error {
	return p.api.Call(ctx, http.MethodPost, "/ip/"+ipBlock(ipv4)+"/spam/"+url.PathEscape(ipv4)+"/unblock", nil, nil)
}

// StopRenewal : renouvellement coupé, suppression à l'expiration (le mois
// payé chez le fournisseur n'est pas remboursé).
func (p *Provider) StopRenewal(ctx context.Context, sn string) error {
	var info struct {
		Renew map[string]any `json:"renew"`
	}
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/serviceInfos"), nil, &info); err != nil {
		if ovh.IsNotFound(err) {
			return nil // déjà supprimé
		}
		return err
	}
	if info.Renew == nil {
		info.Renew = map[string]any{}
	}
	info.Renew["automatic"] = false
	info.Renew["deleteAtExpiration"] = true
	return p.api.Call(ctx, http.MethodPut, vpsPath(sn, "/serviceInfos"), map[string]any{"renew": info.Renew}, nil)
}
