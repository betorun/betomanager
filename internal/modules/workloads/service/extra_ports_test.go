package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/workloads/activities"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/workflows"
)

// Création : les ports supplémentaires sont enregistrés dans la spec et
// transmis au déploiement ; une liste invalide est refusée avant tout.
func TestCreate_ExtraPorts(t *testing.T) {
	s, _, dist := createService(t)
	w, err := s.Deploy(context.Background(), createRequest(func(r *domain.CreateWorkloadRequest) {
		r.Name, r.TargetPort = "mailpit", 8025
		r.ExtraPorts = []domain.ExtraPort{{Name: "smtp", Port: 1025}}
	}))
	require.NoError(t, err)
	in := dist.args[0].(workflows.DeployWorkloadInput)
	assert.Equal(t, []activities.NamedPort{{Name: "smtp", Port: 1025}}, in.Process.ExtraPorts)
	assert.Equal(t, []domain.ExtraPort{{Name: "smtp", Port: 1025}}, domain.SpecExtraPorts(w.Spec))

	s, repo, _ := createService(t)
	_, err = s.Deploy(context.Background(), createRequest(func(r *domain.CreateWorkloadRequest) {
		r.ExtraPorts = []domain.ExtraPort{{Name: "alt", Port: 8080}}
	}))
	assert.ErrorIs(t, err, domain.ErrInvalidExtraPorts)
	assert.Zero(t, repo.created, "rien d'enregistré")
}

// Mise à jour : absent = inchangés (nouvelle image comprise), liste =
// remplace, vide = retire ; un nouveau port principal ne doit pas créer de
// doublon ; un cron n'en a jamais.
func TestUpdate_ExtraPorts(t *testing.T) {
	ports := []domain.ExtraPort{{Name: "smtp", Port: 1025}}
	mk := func(kind string, target int) (*WorkloadService, *fakeRepo, *fakeDistributor) {
		raw, err := json.Marshal(domain.CreateWorkloadRequest{Name: "mailpit", Kind: kind, TargetPort: target, ExtraPorts: ports, Replicas: 1})
		require.NoError(t, err)
		repo := &fakeRepo{w: domain.Workload{ID: uuid.New(), ProjectID: uuid.New(), Name: "mailpit", Namespace: "km-x",
			Kind: kind, TargetPort: target, Replicas: 1, Image: "axllent/mailpit:1", Spec: raw}}
		dist := &fakeDistributor{}
		return &WorkloadService{Repo: repo, Distributor: dist, Variables: &fakeVariables{}}, repo, dist
	}
	saved := func(repo *fakeRepo) []domain.ExtraPort { return domain.SpecExtraPorts(repo.spec) }

	s, repo, dist := mk(domain.KindWeb, 8025)
	require.NoError(t, s.Update(context.Background(), repo.w.ID.String(), domain.UpdateWorkloadRequest{Image: "axllent/mailpit:2"}))
	assert.Equal(t, ports, saved(repo), "nouvelle image : ports conservés")
	assert.Equal(t, []activities.NamedPort{{Name: "smtp", Port: 1025}}, dist.args[0].(workflows.DeployWorkloadInput).Process.ExtraPorts)

	s, repo, _ = mk(domain.KindWeb, 8025)
	next := []domain.ExtraPort{{Name: "grpc", Port: 9090}, {Name: "metrics", Port: 9100}}
	require.NoError(t, s.Update(context.Background(), repo.w.ID.String(), domain.UpdateWorkloadRequest{ExtraPorts: &next}))
	assert.Equal(t, next, saved(repo), "liste remplacée")

	s, repo, dist = mk(domain.KindService, 6379)
	require.NoError(t, s.Update(context.Background(), repo.w.ID.String(), domain.UpdateWorkloadRequest{ExtraPorts: &[]domain.ExtraPort{}}))
	assert.Empty(t, saved(repo), "liste vide : tout retiré")
	assert.Nil(t, dist.args[0].(workflows.DeployWorkloadInput).Process.ExtraPorts)

	s, repo, _ = mk(domain.KindWeb, 8025)
	err := s.Update(context.Background(), repo.w.ID.String(), domain.UpdateWorkloadRequest{TargetPort: 1025})
	assert.ErrorIs(t, err, domain.ErrInvalidExtraPorts, "nouveau port principal déjà pris")

	s, repo, _ = mk(domain.KindWorker, 0)
	bad := []domain.ExtraPort{{Name: "a", Port: 9000}, {Name: "a", Port: 9001}}
	assert.ErrorIs(t, s.Update(context.Background(), repo.w.ID.String(), domain.UpdateWorkloadRequest{ExtraPorts: &bad}), domain.ErrInvalidExtraPorts)
	assert.Nil(t, repo.spec, "rien d'enregistré")

	s, repo, _ = mk(domain.KindCron, 0)
	assert.ErrorIs(t, s.Update(context.Background(), repo.w.ID.String(), domain.UpdateWorkloadRequest{ExtraPorts: &ports}), domain.ErrInvalidExtraPorts)
}
