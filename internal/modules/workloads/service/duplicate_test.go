package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

type dupVariables struct {
	fakeVariables
	views  []domain.VariableView
	values map[string]string
	got    domain.VariableChange
}

func (d *dupVariables) List(context.Context, uuid.UUID) ([]domain.VariableView, error) {
	return d.views, nil
}
func (d *dupVariables) Values(context.Context, uuid.UUID) (map[string]string, error) {
	return d.values, nil
}
func (d *dupVariables) Apply(_ context.Context, _ uuid.UUID, c domain.VariableChange) (int64, bool, error) {
	d.got = c
	return 1, true, nil
}

// La copie reprend image et variables, secrets compris, sans qu'ils passent par l'appelant.
func TestDuplicate_CopiesVariablesAndSecrets(t *testing.T) {
	s, repo, dist := createService(t)
	vars := &dupVariables{
		views:  []domain.VariableView{{Name: "LOG_LEVEL"}, {Name: "DATABASE_URL", Secret: true}},
		values: map[string]string{"LOG_LEVEL": "debug", "DATABASE_URL": "postgres://x"},
	}
	s.Variables = vars
	repo.w = domain.Workload{ID: uuid.New(), ProjectID: uuid.New(), Name: "api", Image: "registry/api:3", Status: "RUNNING",
		TargetPort: 8080, Replicas: 1, CPULimit: "500m", MemoryLimit: "512Mi"}

	w, err := s.Duplicate(context.Background(), repo.w.ID, "api-copie", "user:a@b.c")
	require.NoError(t, err)
	assert.Equal(t, "api-copie", w.Name)
	assert.Equal(t, "registry/api:3", w.Image)
	require.Len(t, dist.started, 1, "la copie est déployée")
	assert.Equal(t, map[string]string{"LOG_LEVEL": "debug"}, vars.got.Env)
	assert.Equal(t, map[string]string{"DATABASE_URL": "postgres://x"}, vars.got.Secrets)
}

func TestDuplicate_NotBuilt(t *testing.T) {
	s, repo, _ := createService(t)
	repo.w = domain.Workload{ID: uuid.New(), ProjectID: uuid.New(), Name: "api", Status: "BUILDING"}
	_, err := s.Duplicate(context.Background(), repo.w.ID, "api-copie", "user:a@b.c")
	assert.ErrorIs(t, err, domain.ErrWorkloadNotBuilt)
}

// multiRepo : plusieurs conteneurs, lus par identifiant.
type multiRepo struct {
	fakeRepo
	byID map[uuid.UUID]domain.Workload
}

func (p *multiRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Workload, error) {
	w := p.byID[id]
	return &w, nil
}
func (p *multiRepo) ListByProject(context.Context, uuid.UUID) ([]domain.Workload, error) {
	out := make([]domain.Workload, 0, len(p.byID))
	for _, w := range p.byID {
		out = append(out, w)
	}
	return out, nil
}

// Une nouvelle image de l'API part aussi vers le worker qui la suit, pas ailleurs.
func TestUpdate_PropagatesImageToFollowers(t *testing.T) {
	s, _, dist := createService(t)
	project := uuid.New()
	mk := func(name, from string) domain.Workload {
		spec := []byte(`{"name":"` + name + `","image_from":"` + from + `"}`)
		return domain.Workload{ID: uuid.New(), ProjectID: project, Name: name, Image: "api:1", Status: "RUNNING",
			Replicas: 1, CPULimit: "100m", MemoryLimit: "128Mi", Spec: spec}
	}
	api, worker, other := mk("api", ""), mk("worker", "api"), mk("site", "")
	repo := &multiRepo{byID: map[uuid.UUID]domain.Workload{api.ID: api, worker.ID: worker, other.ID: other}}
	s.Repo = repo

	require.NoError(t, s.Update(context.Background(), api.ID.String(), domain.UpdateWorkloadRequest{Image: "api:2"}))
	assert.Len(t, dist.started, 2, "l'API puis le worker qui la suit")
}
