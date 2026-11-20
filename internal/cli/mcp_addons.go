package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thekrauss/kubemanager/internal/cli/mcp"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
)

// Outils MCP des services (Redis via Valkey…, ADR 0013). Suppression et mise
// à jour restent à la personne (console ou CLI).

func (m *mcpCtx) addons(ctx context.Context) (*cliAddonList, error) {
	if m.project == "" {
		return nil, errors.New(`pass "project"`)
	}
	return listAddons(ctx, m.cl, m.project)
}

func (m *mcpCtx) addon(ctx context.Context, ref string) (*cliAddon, error) {
	if ref == "" {
		return nil, errors.New(`"addon" is required`)
	}
	list, err := m.addons(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list.Addons {
		if list.Addons[i].ID == ref || list.Addons[i].Name == ref {
			return &list.Addons[i], nil
		}
	}
	return nil, fmt.Errorf("no service %q in this project", ref)
}

func (e mcpEnv) listAddons(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, _, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	list, err := m.addons(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	out := map[string]any{"addons": list.Addons, "max": list.Max}
	return mcp.Result{Text: jsonText(out), Structured: out}, nil
}

func (e mcpEnv) attachAddonTo(ctx context.Context, m *mcpCtx, a *cliAddon, workloadRef, variable string) (string, error) {
	w, err := m.workload(ctx, workloadRef)
	if err != nil {
		return "", err
	}
	if w.ManagedBy != "" {
		return "", fmt.Errorf("container %q runs a service: it cannot receive another one", w.Name)
	}
	var res struct {
		Variable    string `json:"variable"`
		KeyVariable string `json:"key_variable"`
		Redeploy    bool   `json:"redeploy"`
	}
	body := map[string]string{"workload_id": w.ID.String()}
	if variable != "" {
		body["variable"] = variable
	}
	if err := m.cl.Do(ctx, http.MethodPost, addonsPath(m.project)+"/"+url.PathEscape(a.ID)+"/attach", body, &res); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Service %q attached to container %q as the secret variable %s.", a.Name, w.Name, res.Variable)
	if res.KeyVariable != "" {
		msg = fmt.Sprintf("Service %q attached to container %q: address in the secret variable %s, key in %s.", a.Name, w.Name, res.Variable, res.KeyVariable)
	}
	if res.Redeploy {
		msg += " The container is being redeployed; it waits for the service to answer before starting."
	}
	return msg, nil
}

func (e mcpEnv) createAddon(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	body := cliAddonCreate{Template: str(in["template"]), Name: str(in["name"]), Version: str(in["version"]),
		Mode: str(in["mode"]), Size: str(in["size"]), Disk: str(in["disk"])}
	if body.Template == "" {
		body.Template = "redis"
	}
	var a cliAddon
	if err := m.cl.Do(ctx, http.MethodPost, addonsPath(m.project), body, &a); err != nil {
		return mcp.Result{}, err
	}
	// Branchement tout de suite : le mot de passe existe dès la création, et
	// le conteneur attend que le service réponde avant de démarrer.
	var attached string
	if target := str(in["attach_to"]); target != "" {
		msg, err := e.attachAddonTo(ctx, m, &a, target, "")
		if err != nil {
			return mcp.Result{Text: fmt.Sprintf("Service %q created, but attaching failed: %s", a.Name, err), Structured: a, IsError: true}, nil
		}
		attached = "\n" + msg
	}
	deadline := e.now().Add(e.workloadTimeout)
	for a.Status != "RUNNING" {
		if workloadTone(a.Status) == ui.ToneError {
			return mcp.Result{Text: fmt.Sprintf("The service %q did not start (%s); call get_logs on container %q.", a.Name, a.Status, a.Name),
				Structured: a, IsError: true}, nil
		}
		if e.now().After(deadline) {
			return mcp.Result{Text: "The service is still starting; call list_addons in a moment." + attached, Structured: a}, nil
		}
		call.Progress(0, 0, "service "+strings.ToLower(a.Status))
		select {
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		case <-time.After(e.poll):
		}
		if err := m.cl.Do(ctx, http.MethodGet, addonsPath(m.project)+"/"+url.PathEscape(a.ID), nil, &a); err != nil {
			return mcp.Result{}, err
		}
	}
	text := fmt.Sprintf("Service %q is ready (%s %s), address %s, reachable only from the project's containers. The password is never shown: "+
		"attach the service to a container to give it %s.", a.Name, a.Title, a.Version, a.Host, a.Variable)
	return mcp.Result{Text: text + attached, Structured: a}, nil
}

func (e mcpEnv) attachAddon(ctx context.Context, call mcp.Call) (mcp.Result, error) {
	m, in, err := e.session(ctx, call.Arguments, true)
	if err != nil {
		return mcp.Result{}, err
	}
	a, err := m.addon(ctx, str(in["addon"]))
	if err != nil {
		return mcp.Result{}, err
	}
	msg, err := e.attachAddonTo(ctx, m, a, str(in["workload"]), str(in["variable"]))
	if err != nil {
		return mcp.Result{}, err
	}
	return mcp.Result{Text: msg}, nil
}
