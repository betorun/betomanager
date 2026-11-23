package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/temporal"
	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
	"github.com/thekrauss/kubemanager/internal/modules/projects/repository"
	"github.com/thekrauss/kubemanager/internal/modules/projects/workflows"
)

// Nouveau projet : espace Kubernetes tiré de l'identifiant, nom nettoyé,
// organisation transmise ; le workflow reçoit l'espace, pas le nom.
func TestCreateProject_NamespaceFromIDAndOrganization(t *testing.T) {
	dist := &fakeDistributor{handle: temporal.WorkflowHandle{WorkflowID: "wf"}}
	repo := &fakeProjectRepo{}
	org := uuid.New()
	resp, err := newProjectServiceForTest(dist, repo).CreateProject(context.Background(),
		typesproject.CreateProjectRequest{Name: "  Mon API  ", OrganizationID: org.String()}, uuid.NewString())
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	p := repo.created[0]
	assert.Equal(t, "Mon API", p.Name, "nom libre, espaces de bord retirés")
	assert.Equal(t, org, p.OrganizationID)
	id := uuid.MustParse(resp.ProjectID)
	assert.Equal(t, "km-"+strings.ReplaceAll(id.String(), "-", "")[:12], p.Namespace)
	input := dist.gotArgs[0].(workflows.CreateProjectInput)
	assert.Equal(t, p.Namespace, input.Namespace)
}

func TestCreateProject_Refusals(t *testing.T) {
	ctx := context.Background()
	svc := newProjectServiceForTest(&fakeDistributor{}, &fakeProjectRepo{})
	for _, name := range []string{"ab", "   ab  ", strings.Repeat("x", 31), "a\x00bc"} {
		_, err := svc.CreateProject(ctx, typesproject.CreateProjectRequest{Name: name}, uuid.NewString())
		assert.ErrorIs(t, err, ErrInvalidProjectName, "%q", name)
	}
	_, err := svc.CreateProject(ctx, typesproject.CreateProjectRequest{Name: "alpha", OrganizationID: "pas-un-uuid"}, uuid.NewString())
	assert.ErrorIs(t, err, ErrInvalidOrganization)

	full := newProjectServiceForTest(&fakeDistributor{}, &fakeProjectRepo{createErr: repository.ErrProjectLimit})
	_, err = full.CreateProject(ctx, typesproject.CreateProjectRequest{Name: "alpha"}, uuid.NewString())
	assert.ErrorIs(t, err, ErrProjectLimit)
}

// Renommer ne touche pas à l'espace Kubernetes ; nom pris : conflit.
func TestUpdateProject(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	repo := &fakeProjectRepo{project: &authdomain.Project{ID: id, Name: "bomba", Namespace: "km-bomba", Description: "x"}}
	svc := newProjectServiceForTest(&fakeDistributor{}, repo)

	name := " Bomba prod "
	got, err := svc.UpdateProject(ctx, typesproject.UpdateProjectRequest{ProjectID: id.String(), Name: &name})
	require.NoError(t, err)
	assert.Equal(t, "Bomba prod", got.Name)
	assert.Equal(t, "x", got.Description, "description absente : inchangée")
	assert.Equal(t, "km-bomba", repo.project.Namespace)

	bad := "ab"
	_, err = svc.UpdateProject(ctx, typesproject.UpdateProjectRequest{ProjectID: id.String(), Name: &bad})
	assert.ErrorIs(t, err, ErrInvalidProjectName)

	repo.updateErr = repository.ErrNameTaken
	_, err = svc.UpdateProject(ctx, typesproject.UpdateProjectRequest{ProjectID: id.String(), Name: &name})
	assert.ErrorIs(t, err, ErrProjectNameTaken)
}

type fakePlacer struct {
	cluster, gotRegion string
	err                error
}

func (f *fakePlacer) Place(_ context.Context, region string) (string, error) {
	f.gotRegion = region
	return f.cluster, f.err
}

// Le projet porte le cluster choisi pour sa région ; une région sans place
// est refusée avant toute écriture.
func TestCreateProject_PlacedOnCluster(t *testing.T) {
	ctx := context.Background()
	repo := &fakeProjectRepo{}
	svc := newProjectServiceForTest(&fakeDistributor{handle: temporal.WorkflowHandle{WorkflowID: "wf"}}, repo)
	placer := &fakePlacer{cluster: "prox-gra-1"}
	svc.Placer = placer
	_, err := svc.CreateProject(ctx, typesproject.CreateProjectRequest{Name: "alpha", Region: "eu-west-gra"}, uuid.NewString())
	require.NoError(t, err)
	assert.Equal(t, "eu-west-gra", placer.gotRegion)
	require.Len(t, repo.created, 1)
	require.NotNil(t, repo.created[0].ClusterID)
	assert.Equal(t, "prox-gra-1", *repo.created[0].ClusterID)

	placer.cluster = "" // registre vide : cluster par défaut
	_, err = svc.CreateProject(ctx, typesproject.CreateProjectRequest{Name: "beta"}, uuid.NewString())
	require.NoError(t, err)
	assert.Nil(t, repo.created[1].ClusterID)

	placer.err = errNoPlace
	_, err = svc.CreateProject(ctx, typesproject.CreateProjectRequest{Name: "gamma", Region: "eu-west-rbx"}, uuid.NewString())
	assert.ErrorIs(t, err, errNoPlace)
	assert.Len(t, repo.created, 2, "rien d'écrit")
}

var errNoPlace = errors.New("no capacity")
