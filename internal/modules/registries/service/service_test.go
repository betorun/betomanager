package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
	"github.com/thekrauss/kubemanager/internal/modules/registries/repository"
)

type memStore struct {
	repository.Store
	rows map[string]domain.Auth
}

func (m *memStore) Put(_ context.Context, _ uuid.UUID, host, user, pw string, _ *uuid.UUID) (domain.View, error) {
	m.rows[host] = domain.Auth{Host: host, Username: user, Password: pw}
	return domain.View{Host: host, Username: user}, nil
}

func (m *memStore) Delete(_ context.Context, _ uuid.UUID, host string) error {
	if _, ok := m.rows[host]; !ok {
		return domain.ErrNotFound
	}
	delete(m.rows, host)
	return nil
}

func newService() (*Service, *memStore) {
	m := &memStore{rows: map[string]domain.Auth{}}
	return &Service{Store: m, PlatformHost: "registry.betorun.app"}, m
}

func TestPut_NormalizesAndValidates(t *testing.T) {
	s, m := newService()
	v, err := s.Put(context.Background(), uuid.New(), domain.PutRequest{Host: "GHCR.io", Username: " moi ", Password: "ghp_x"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io", v.Host)
	assert.Equal(t, "moi", m.rows["ghcr.io"].Username)

	_, err = s.Put(context.Background(), uuid.New(), domain.PutRequest{Host: "https://ghcr.io", Username: "moi", Password: "x"}, nil)
	assert.ErrorIs(t, err, domain.ErrInvalidHost)
	_, err = s.Put(context.Background(), uuid.New(), domain.PutRequest{Host: "ghcr.io", Username: "moi", Password: ""}, nil)
	assert.ErrorIs(t, err, domain.ErrInvalidPassword)
}

// Le registre de la plateforme a déjà son accès, limité au projet : un accès
// déclaré par le client le remplacerait dans le Secret de pull.
func TestPut_RefusesPlatformRegistry(t *testing.T) {
	s, m := newService()
	_, err := s.Put(context.Background(), uuid.New(), domain.PutRequest{Host: "Registry.Betorun.app", Username: "moi", Password: "x"}, nil)
	assert.ErrorIs(t, err, domain.ErrPlatformRegistry)
	assert.Empty(t, m.rows)
}

func TestDelete(t *testing.T) {
	s, m := newService()
	m.rows["ghcr.io"] = domain.Auth{Host: "ghcr.io"}
	require.NoError(t, s.Delete(context.Background(), uuid.New(), "GHCR.IO"))
	assert.Empty(t, m.rows)
	assert.ErrorIs(t, s.Delete(context.Background(), uuid.New(), "ghcr.io"), domain.ErrNotFound)
	assert.ErrorIs(t, s.Delete(context.Background(), uuid.New(), "pas un hôte"), domain.ErrNotFound)
}
