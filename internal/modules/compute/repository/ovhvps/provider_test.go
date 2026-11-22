package ovhvps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/ovh"
	"github.com/thekrauss/kubemanager/internal/modules/compute/domain"
	"github.com/thekrauss/kubemanager/internal/modules/compute/service"
)

func fakeOVH(t *testing.T, routes map[string]string) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.Header.Get("X-Ovh-Signature"), "appel signé")
		key := r.Method + " " + r.URL.Path
		if q := r.URL.RawQuery; q != "" {
			key += "?" + q
		}
		body, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p, err := New(ovh.Credentials{Endpoint: srv.URL, ApplicationKey: "ak", ApplicationSecret: "as", ConsumerKey: "ck"}, "FR")
	require.NoError(t, err)
	return p
}

func TestList(t *testing.T) {
	p := fakeOVH(t, map[string]string{
		"GET /vps":                                     `["vps-a.vps.ovh.net","vps-b.vps.ovh.net","vps-gone.vps.ovh.net"]`,
		"GET /vps/vps-a.vps.ovh.net":                   `{"name":"vps-a.vps.ovh.net","state":"running","zone":"Region OpenStack: os-gra7","model":{"name":"vps-2025-model1"}}`,
		"GET /vps/vps-a.vps.ovh.net/ips":               `["2001:41d0:1:2::1","51.68.1.2"]`,
		"GET /vps/vps-a.vps.ovh.net/tasks?state=doing": `[]`,
		"GET /vps/vps-a.vps.ovh.net/tasks?state=todo":  `[]`,
		"GET /vps/vps-b.vps.ovh.net":                   `{"name":"vps-b.vps.ovh.net","state":"stopped","zone":"Region OpenStack: os-sbg5","model":{"name":"vps-2025-model2"}}`,
		"GET /vps/vps-b.vps.ovh.net/ips":               `["51.68.3.4/32"]`,
		"GET /vps/vps-b.vps.ovh.net/tasks?state=doing": `[812]`,
		// vps-gone : supprimé entre la liste et la lecture (404).
	})
	list, err := p.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, service.ProviderVM{ProviderID: "vps-a.vps.ovh.net", State: domain.StateRunning, RawState: "running",
		Plan: "vps-2025-model1", Zone: "Region OpenStack: os-gra7", IPv4: "51.68.1.2", IPv6: "2001:41d0:1:2::1"}, list[0])
	assert.Equal(t, domain.StateStopped, list[1].State)
	assert.Equal(t, "51.68.3.4", list[1].IPv4)
	assert.True(t, list[1].Busy, "opération en cours")
}

func TestListError(t *testing.T) {
	p := fakeOVH(t, map[string]string{"GET /vps": `["vps-a"]`, "GET /vps/vps-a": `{"state":"running"}`})
	// /ips répond 404 (toléré), /tasks aussi ; mais une erreur 500 remonte.
	_, err := p.List(context.Background())
	require.NoError(t, err)

	p = fakeOVH(t, map[string]string{})
	_, err = p.List(context.Background())
	assert.True(t, ovh.IsNotFound(err))
}

func TestTask(t *testing.T) {
	p := fakeOVH(t, map[string]string{
		"GET /vps/vps-a/tasks/812": `{"id":812,"type":"reinstallVm","state":"doing","progress":40}`,
	})
	task, err := p.Task(context.Background(), "vps-a", "812")
	require.NoError(t, err)
	assert.Equal(t, &service.ProviderTask{ID: "812", Type: "reinstallVm", State: service.TaskDoing, Progress: 40}, task)
	_, err = p.Task(context.Background(), "vps-a", "../me")
	assert.Error(t, err, "identifiant d'opération numérique seulement")
}

func TestTranslateState(t *testing.T) {
	for raw, want := range map[string][2]any{
		"running":     {domain.StateRunning, false},
		"stopped":     {domain.StateStopped, false},
		"installing":  {domain.StateCreating, true},
		"rebooting":   {domain.StateRunning, true},
		"maintenance": {"", true},
		"nouveau":     {"", true},
	} {
		s, busy := TranslateState(raw)
		assert.Equal(t, want[0], s, raw)
		assert.Equal(t, want[1], busy, raw)
	}
}

func TestActionsNotYet(t *testing.T) {
	p := fakeOVH(t, nil)
	_, err := p.ConsoleURL(context.Background(), "x")
	assert.ErrorIs(t, err, ErrNotSupported)
}
