package ovhvps

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

var (
	_ service.StockProvider = (*Provider)(nil)
	_ service.Inbox         = (*Mailbox)(nil)
)

// platformLocations : codes de la plateforme, reconnus en tête du nom du datacenter.
var platformLocations = []string{domain.LocationGRA, domain.LocationRBX, domain.LocationSBG}

// Stock : règles de commande du fournisseur (stock et délai par datacenter).
func (p *Provider) Stock(ctx context.Context, planCode string) ([]service.ProviderStock, error) {
	q := url.Values{"ovhSubsidiary": {p.subsidiary}, "planCode": {planCode}}
	var res struct {
		Datacenters []struct {
			Datacenter         string `json:"datacenter"`
			Code               string `json:"code"`
			Status             string `json:"status"`
			LinuxStatus        string `json:"linuxStatus"`
			DaysBeforeDelivery int    `json:"daysBeforeDelivery"`
		} `json:"datacenters"`
	}
	if err := p.api.Call(ctx, http.MethodGet, "/vps/order/rule/datacenter?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	best := map[string]service.ProviderStock{}
	for _, dc := range res.Datacenters {
		loc := ""
		name := strings.ToLower(dc.Datacenter + " " + dc.Code)
		for _, l := range platformLocations {
			if strings.HasPrefix(strings.ToLower(dc.Datacenter), l) || strings.HasPrefix(strings.ToLower(dc.Code), l) || strings.Contains(name, " "+l) {
				loc = l
			}
		}
		if loc == "" {
			continue // emplacement que la plateforme ne vend pas
		}
		status := dc.LinuxStatus // nos systèmes sont tous des Linux
		if status == "" {
			status = dc.Status
		}
		st := service.ProviderStock{Location: loc, Status: translateStock(status), Days: dc.DaysBeforeDelivery}
		// Plusieurs datacenters pour un emplacement : le meilleur l'emporte.
		if prev, ok := best[loc]; !ok || rank(st.Status) > rank(prev.Status) {
			best[loc] = st
		}
	}
	out := make([]service.ProviderStock, 0, len(best))
	for _, l := range platformLocations {
		if st, ok := best[l]; ok {
			out = append(out, st)
		}
	}
	return out, nil
}

func translateStock(s string) string {
	switch s {
	case "available":
		return domain.StockAvailable
	case "out-of-stock-preorder-allowed":
		return domain.StockPreorder
	}
	return domain.StockOutOfStock
}

func rank(s string) int {
	switch s {
	case domain.StockAvailable:
		return 2
	case domain.StockPreorder:
		return 1
	}
	return 0
}

// Recent : e-mails reçus depuis since, hors e-mails du mode secours (gardés
// pour l'interception du mot de passe).
func (m *Mailbox) Recent(ctx context.Context, since time.Time) ([]service.InboxMail, error) {
	msgs, err := m.Box.Search(ctx, since, "")
	if err != nil {
		return nil, err
	}
	out := make([]service.InboxMail, 0, len(msgs))
	for _, msg := range msgs {
		if rescueHint.MatchString(msg.Subject) && passwordRe.MatchString(msg.Text) {
			continue
		}
		out = append(out, service.InboxMail{ID: msg.UID, From: msg.From, Subject: msg.Subject, Date: msg.Date, Text: msg.Text})
	}
	return out, nil
}
