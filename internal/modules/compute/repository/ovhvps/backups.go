package ovhvps

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

var _ service.BackupProvider = (*Provider)(nil)

// Options du fournisseur.
const (
	optAutomatedBackup = "automatedBackup"
	optSnapshot        = "snapshot"
)

// Backups : options du VPS, points de restauration disponibles, points
// montés (seul le disque est gardé : les accès réseau nomment le fournisseur),
// snapshot.
func (p *Provider) Backups(ctx context.Context, sn string) (*service.ProviderBackups, error) {
	var opts []string
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/option"), nil, &opts); err != nil {
		return nil, err
	}
	out := &service.ProviderBackups{}
	for _, o := range opts {
		switch o {
		case optAutomatedBackup:
			out.Automated = true
		case optSnapshot:
			out.SnapshotOption = true
		}
	}
	if out.Automated {
		var points []time.Time
		if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/automatedBackup/restorePoints?state=available"), nil, &points); err != nil && !ovh.IsNotFound(err) {
			return nil, err
		}
		sort.Slice(points, func(i, j int) bool { return points[i].After(points[j]) })
		out.RestorePoints = points
		var attached []struct {
			RestorePoint time.Time `json:"restorePoint"`
			Access       struct {
				AdditionalDisk string `json:"additionalDisk"`
			} `json:"access"`
		}
		if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/automatedBackup/attachedBackup"), nil, &attached); err != nil && !ovh.IsNotFound(err) {
			return nil, err
		}
		for _, a := range attached {
			out.Mounted = append(out.Mounted, domain.MountedBackup{RestorePoint: a.RestorePoint, Disk: a.Access.AdditionalDisk})
		}
	}
	if out.SnapshotOption {
		var snap struct {
			CreationDate time.Time `json:"creationDate"`
			Description  string    `json:"description"`
		}
		err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/snapshot"), nil, &snap)
		switch {
		case err == nil:
			out.Snapshot = &domain.SnapshotInfo{CreatedAt: snap.CreationDate, Description: snap.Description}
		case !ovh.IsNotFound(err):
			return nil, err
		}
	}
	return out, nil
}

func (p *Provider) Restore(ctx context.Context, sn string, point time.Time, full bool) (string, error) {
	typ := "file"
	if full {
		typ = "full"
	}
	return p.action(ctx, sn, "automatedBackup/restore", map[string]any{
		"restorePoint": point.UTC().Format(time.RFC3339), "type": typ, "changePassword": false,
	})
}

func (p *Provider) Unmount(ctx context.Context, sn string, point time.Time) (string, error) {
	return p.action(ctx, sn, "automatedBackup/detachBackup", map[string]any{"restorePoint": point.UTC().Format(time.RFC3339)})
}

func (p *Provider) CreateSnapshot(ctx context.Context, sn, description string) (string, error) {
	return p.action(ctx, sn, "createSnapshot", map[string]any{"description": description})
}

func (p *Provider) RevertSnapshot(ctx context.Context, sn string) (string, error) {
	return p.action(ctx, sn, "snapshot/revert", nil)
}

func (p *Provider) DeleteSnapshot(ctx context.Context, sn string) (string, error) {
	var t ovhTask
	if err := p.api.Call(ctx, http.MethodDelete, vpsPath(sn, "/snapshot"), nil, &t); err != nil {
		return "", err
	}
	return strconv.FormatInt(t.ID, 10), nil
}

// snapshotTasks : opérations que abortSnapshot interrompt.
var snapshotTasks = map[string]bool{"createSnapshot": true, "revertSnapshot": true, "deleteSnapshot": true, "restoreVm": true}

// AbortStuck : une opération de snapshot ou de sauvegarde en cours depuis
// plus de maxAge est annulée.
func (p *Provider) AbortStuck(ctx context.Context, sn string, maxAge time.Duration) (bool, error) {
	var ids []int64
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/tasks?state=doing"), nil, &ids); err != nil {
		if ovh.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	now := p.api.Now()
	for _, id := range ids {
		var t struct {
			Type string    `json:"type"`
			Date time.Time `json:"date"`
		}
		if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/tasks/", strconv.FormatInt(id, 10)), nil, &t); err != nil {
			return false, err
		}
		if snapshotTasks[t.Type] && !t.Date.IsZero() && now.Sub(t.Date) > maxAge {
			if err := p.api.Call(ctx, http.MethodPost, vpsPath(sn, "/abortSnapshot"), nil, nil); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// OrderSnapshotOption : option ajoutée au VPS (panier d'option de service),
// prix contrôlé avant le paiement.
func (p *Provider) OrderSnapshotOption(ctx context.Context, sn, planCode string, maxPriceCents int64) (string, error) {
	var cart struct {
		CartID string `json:"cartId"`
	}
	if err := p.api.Call(ctx, http.MethodPost, "/order/cart", map[string]any{"ovhSubsidiary": p.subsidiary, "description": sn + " snapshot"}, &cart); err != nil {
		return "", err
	}
	if err := p.api.Call(ctx, http.MethodPost, "/order/cartServiceOption/vps/"+url.PathEscape(sn), map[string]any{
		"cartId": cart.CartID, "planCode": planCode, "duration": "P1M", "pricingMode": "default", "quantity": 1,
	}, nil); err != nil {
		return "", err
	}
	return p.checkout(ctx, "/order/cart/"+url.PathEscape(cart.CartID), maxPriceCents)
}

// ReleaseSnapshotOption : option résiliée par l'API des services (service du
// VPS → son option snapshot → résiliation immédiate). Si l'API des services
// ne la trouve pas (API encore en bêta chez le fournisseur), repli sur
// l'ancienne route du VPS. Le snapshot est supprimé avec l'option.
func (p *Provider) ReleaseSnapshotOption(ctx context.Context, sn, planCode string) error {
	optionID, err := p.snapshotServiceID(ctx, sn, planCode)
	if err == nil && optionID != 0 {
		err = p.api.Call(ctx, http.MethodDelete, "/services/"+strconv.FormatInt(optionID, 10), nil, nil)
		if err == nil || ovh.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err != nil && !ovh.IsNotFound(err) {
		return err
	}
	err = p.api.Call(ctx, http.MethodDelete, vpsPath(sn, "/option/", optSnapshot), nil, nil)
	if ovh.IsNotFound(err) {
		return nil // déjà rendue
	}
	return err
}

// snapshotServiceID : identifiant de service de l'option snapshot du VPS (0 : introuvable).
func (p *Provider) snapshotServiceID(ctx context.Context, sn, planCode string) (int64, error) {
	var ids []int64
	if err := p.api.Call(ctx, http.MethodGet, "/services?resourceName="+url.QueryEscape(sn), nil, &ids); err != nil {
		return 0, err
	}
	for _, id := range ids {
		var opts []struct {
			ServiceID int64 `json:"serviceId"`
			Billing   struct {
				Plan struct {
					Code string `json:"code"`
				} `json:"plan"`
			} `json:"billing"`
			Route struct {
				Path string `json:"path"`
			} `json:"route"`
		}
		if err := p.api.Call(ctx, http.MethodGet, "/services/"+strconv.FormatInt(id, 10)+"/options", nil, &opts); err != nil {
			return 0, err
		}
		for _, o := range opts {
			code := strings.ToLower(o.Billing.Plan.Code)
			if (planCode != "" && strings.EqualFold(o.Billing.Plan.Code, planCode)) || strings.Contains(code, "snapshot") ||
				strings.Contains(strings.ToLower(o.Route.Path), "/option/snapshot") {
				return o.ServiceID, nil
			}
		}
	}
	return 0, nil
}
