package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"

	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/repository"
)

type fakeRepo struct {
	rows    map[string]*dauth.Project
	created int
}

func (f *fakeRepo) CreateProject(_ context.Context, p *dauth.Project, _ string) error {
	f.created++
	f.rows[p.ID.String()] = p
	return nil
}
func (f *fakeRepo) DeleteProject(context.Context, string) error { return nil }
func (f *fakeRepo) GetProjectByID(_ context.Context, id string) (*dauth.Project, error) {
	if p, ok := f.rows[id]; ok {
		return p, nil
	}
	return nil, gorm.ErrRecordNotFound
}
func (f *fakeRepo) UpdateProjectStatus(context.Context, string, string) error { return nil }
func (f *fakeRepo) UpdateProjectLimits(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeRepo) ListProjects(context.Context, []uuid.UUID, []uuid.UUID, []uuid.UUID, int) ([]dauth.Project, error) {
	return nil, nil
}
func (f *fakeRepo) UpdateProject(context.Context, uuid.UUID, string, string) error { return nil }
func (f *fakeRepo) OrganizationQuotas(context.Context, []uuid.UUID) ([]repository.OrgQuota, error) {
	return nil, nil
}
func (f *fakeRepo) SetMaxProjects(context.Context, uuid.UUID, int) error { return nil }

// Ligne déjà écrite par CreateProject : l'activité la renvoie sans la recréer.
func TestCreateProjectInDB_ExistingRow(t *testing.T) {
	id := uuid.New()
	repo := &fakeRepo{rows: map[string]*dauth.Project{id.String(): {ID: id, Name: "alpha"}}}
	a := &ProjectDBActivities{Repo: repo, Logger: zap.NewNop().Sugar()}

	p, err := a.CreateProjectInDB(context.Background(), id.String(), "alpha", "", uuid.NewString())

	require.NoError(t, err)
	assert.Equal(t, id, p.ID)
	assert.Zero(t, repo.created)
}

// Workflow lancé avant le correctif : la ligne n'existe pas, l'activité la crée.
func TestCreateProjectInDB_MissingRow(t *testing.T) {
	id := uuid.New()
	repo := &fakeRepo{rows: map[string]*dauth.Project{}}
	a := &ProjectDBActivities{Repo: repo, Logger: zap.NewNop().Sugar()}

	p, err := a.CreateProjectInDB(context.Background(), id.String(), "alpha", "", uuid.NewString())

	require.NoError(t, err)
	assert.Equal(t, id, p.ID)
	assert.Equal(t, 1, repo.created)
}
