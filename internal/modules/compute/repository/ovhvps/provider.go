// Package ovhvps : fournisseur de VPS OVH (ComputeProvider). Utilisé
// seulement par le mode compute (APP_MODE=compute), seul process qui a les
// clés. Aucun terme OVH ne sort vers un client : les états sont traduits en
// domain.State*, les termes bruts ne servent qu'au back-office.
//
// Lecture (liste, VPS, adresses, opérations), commande (order.go), démarrage,
// arrêt, redémarrage, réinstallation avec la clé SSH du client.
package ovhvps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

// ErrNotSupported : action pas encore disponible chez ce fournisseur.
var ErrNotSupported = errors.New("compute: action not supported yet")

// readConcurrency : lectures en parallèle pendant une liste.
const readConcurrency = 4

// Provider : VPS d'un compte OVH. Subsidiary : filiale des commandes (FR).
type Provider struct {
	api        *ovh.Client
	subsidiary string
}

var _ service.ComputeProvider = (*Provider)(nil)

// New : erreur si un identifiant manque.
func New(c ovh.Credentials, subsidiary string) (*Provider, error) {
	api, err := ovh.New(c)
	if err != nil {
		return nil, err
	}
	if subsidiary == "" {
		subsidiary = "FR"
	}
	return &Provider{api: api, subsidiary: subsidiary}, nil
}

// API : client signé (tests).
func (p *Provider) API() *ovh.Client { return p.api }

func (p *Provider) Name() string { return domain.ProviderOVH }

func vpsPath(sn string, rest ...string) string {
	return "/vps/" + url.PathEscape(sn) + strings.Join(rest, "")
}

// Names : noms des VPS du compte (un appel).
func (p *Provider) Names(ctx context.Context) ([]string, error) {
	var names []string
	return names, p.api.Call(ctx, http.MethodGet, "/vps", nil, &names)
}

// List : tous les VPS du compte, lus un par un (4 à la fois).
func (p *Provider) List(ctx context.Context) ([]service.ProviderVM, error) {
	names, err := p.Names(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*service.ProviderVM, len(names))
	errs := make([]error, len(names))
	sem := make(chan struct{}, readConcurrency)
	var wg sync.WaitGroup
	for i, sn := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, sn string) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i], errs[i] = p.Get(ctx, sn)
		}(i, sn)
	}
	wg.Wait()
	// Un VPS illisible n'arrête pas la passe (il est rendu « illisible ») ;
	// tous illisibles : panne du fournisseur ou des clés, la passe échoue.
	failed := 0
	list := make([]service.ProviderVM, 0, len(names))
	for i, vm := range out {
		switch {
		case errs[i] != nil:
			failed++
			list = append(list, service.ProviderVM{ProviderID: names[i], Unread: true})
		case vm != nil: // nil : supprimé entre la liste et la lecture
			list = append(list, *vm)
		}
	}
	if failed > 0 && failed == len(names) {
		return nil, errors.Join(errs...)
	}
	return list, nil
}

type vpsInfo struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Zone  string `json:"zone"`
	Model struct {
		Name string `json:"name"`
	} `json:"model"`
}

// Get : un VPS ; nil s'il n'existe pas (ou plus).
func (p *Provider) Get(ctx context.Context, sn string) (*service.ProviderVM, error) {
	var info vpsInfo
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn), nil, &info); err != nil {
		if ovh.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var ips []string
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/ips"), nil, &ips); err != nil && !ovh.IsNotFound(err) {
		return nil, err
	}
	pending, err := p.openTasks(ctx, sn)
	if err != nil {
		return nil, err
	}
	state, busy := TranslateState(info.State)
	vm := &service.ProviderVM{
		ProviderID: sn, State: state, RawState: info.State, Plan: info.Model.Name, Zone: info.Zone,
		Busy: busy || pending,
	}
	vm.IPv4, vm.IPv6 = splitIPs(ips)
	return vm, nil
}

// openTasks : une opération est prévue ou en cours.
func (p *Provider) openTasks(ctx context.Context, sn string) (bool, error) {
	for _, st := range []string{service.TaskDoing, service.TaskTodo} {
		var ids []int64
		if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/tasks?state="+st), nil, &ids); err != nil {
			if ovh.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if len(ids) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Task : suivi d'une opération.
func (p *Provider) Task(ctx context.Context, sn, taskID string) (*service.ProviderTask, error) {
	if _, err := strconv.ParseInt(taskID, 10, 64); err != nil {
		return nil, fmt.Errorf("compute: invalid task id %q", taskID)
	}
	var t struct {
		ID       int64  `json:"id"`
		Type     string `json:"type"`
		State    string `json:"state"`
		Progress int    `json:"progress"`
	}
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/tasks/", taskID), nil, &t); err != nil {
		return nil, err
	}
	return &service.ProviderTask{ID: strconv.FormatInt(t.ID, 10), Type: t.Type, State: translateTask(t.State), Progress: t.Progress}, nil
}

// TranslateState : état OVH → état de la plateforme, et si une opération
// est en cours (la réconciliation ne touche alors à rien).
func TranslateState(raw string) (state string, busy bool) {
	switch raw {
	case "running":
		return domain.StateRunning, false
	case "stopped":
		return domain.StateStopped, false
	case "installing":
		return domain.StateCreating, true
	case "stopping":
		return domain.StateStopped, true
	case "rebooting":
		return domain.StateRunning, true
	}
	// maintenance, rescued, upgrading, backuping (états du fournisseur), ou inconnu.
	return "", true
}

func translateTask(raw string) string {
	switch raw {
	case "todo", "init", "paused", "blocked", "waitingAck":
		return service.TaskTodo
	case "doing":
		return service.TaskDoing
	case "done":
		return service.TaskDone
	case "cancelled":
		return service.TaskCancelled
	}
	return service.TaskError
}

// splitIPs : première IPv4 et première IPv6 publiques.
func splitIPs(ips []string) (v4, v6 string) {
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if i := strings.IndexByte(ip, '/'); i > 0 {
			ip = ip[:i]
		}
		switch {
		case strings.Contains(ip, ":"):
			if v6 == "" {
				v6 = ip
			}
		case ip != "":
			if v4 == "" {
				v4 = ip
			}
		}
	}
	return v4, v6
}

// ── Actions ──────────────────────────────────────────────────────────────

type ovhTask struct {
	ID int64 `json:"id"`
}

func (p *Provider) action(ctx context.Context, sn, verb string, body any) (string, error) {
	var t ovhTask
	if err := p.api.Call(ctx, http.MethodPost, vpsPath(sn, "/", verb), body, &t); err != nil {
		return "", err
	}
	return strconv.FormatInt(t.ID, 10), nil
}

func (p *Provider) Start(ctx context.Context, sn string) (string, error) {
	return p.action(ctx, sn, "start", nil)
}
func (p *Provider) Stop(ctx context.Context, sn string) (string, error) {
	return p.action(ctx, sn, "stop", nil)
}
func (p *Provider) Reboot(ctx context.Context, sn string) (string, error) {
	return p.action(ctx, sn, "reboot", nil)
}

// ErrImageNotFound : le système demandé n'est pas proposé pour ce VPS.
var ErrImageNotFound = errors.New("compute: image not available for this VPS")

// Rebuild : réinstallation avec la clé du client, sans mot de passe envoyé
// par e-mail (doNotSendPassword). image : nom affiché chez le fournisseur
// (« Ubuntu 24.04 ») ; son identifiant est cherché parmi les images du VPS.
func (p *Provider) Rebuild(ctx context.Context, sn, image, sshPublicKey, script string) (string, error) {
	id, err := p.imageID(ctx, sn, image)
	if err != nil {
		return "", err
	}
	body := map[string]any{"imageId": id, "publicSshKey": sshPublicKey, "doNotSendPassword": true}
	if script != "" {
		body["postInstallScript"] = script
	}
	return p.action(ctx, sn, "rebuild", body)
}

func (p *Provider) imageID(ctx context.Context, sn, name string) (string, error) {
	var ids []string
	if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/images/available"), nil, &ids); err != nil {
		return "", err
	}
	for _, id := range ids {
		var img struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := p.api.Call(ctx, http.MethodGet, vpsPath(sn, "/images/available/", url.PathEscape(id)), nil, &img); err != nil {
			return "", err
		}
		if strings.EqualFold(strings.TrimSpace(img.Name), strings.TrimSpace(name)) {
			return img.ID, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrImageNotFound, name)
}

// ConsoleURL : lot 4.
func (p *Provider) ConsoleURL(context.Context, string) (string, error) { return "", ErrNotSupported }
