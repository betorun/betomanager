package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/builds/domain"
)

type fullQuota struct{}

func (fullQuota) CheckQuota(context.Context, uuid.UUID) error { return domain.ErrImageQuota }

func TestFromGit_ImageQuotaReached(t *testing.T) {
	s := &Service{Repo: &startRepo{}, Images: fullQuota{}, Cfg: Config{MaxConcurrent: 5, RegistryHost: "r.example"}}
	_, err := s.FromGit(context.Background(), uuid.New(), domain.CreateGitBuildRequest{Name: "web", GitURL: "https://github.com/a/b"}, nil)
	require.ErrorIs(t, err, domain.ErrImageQuota)
}

type recordingRepo struct {
	startRepo
	created []*domain.Build
}

func (r *recordingRepo) Create(_ context.Context, b *domain.Build) error {
	r.created = append(r.created, b)
	return nil
}
func (*recordingRepo) ActiveForWorkload(context.Context, uuid.UUID) ([]domain.Build, error) {
	return nil, nil
}

// Un push refusé pour quota laisse une trace visible (build FAILED IMAGE_QUOTA).
func TestFromGitHub_QuotaRejectionRecorded(t *testing.T) {
	repo := &recordingRepo{}
	s := &Service{Repo: repo, Images: fullQuota{}, Cfg: Config{MaxConcurrent: 5, RegistryHost: "r.example"}}
	b, err := s.FromGitHub(context.Background(), GitHubBuild{Project: uuid.New(), Workload: uuid.New(), Name: "web",
		Branch: "main", Commit: "0123456789abcdef0123456789abcdef01234567", RecordRejection: true})
	require.ErrorIs(t, err, domain.ErrImageQuota)
	assert.True(t, domain.Rejected(err))
	require.Len(t, repo.created, 1)
	assert.Equal(t, domain.FailureImageQuota, b.Failure)
	assert.Equal(t, domain.StatusFailed, b.Status)
}
