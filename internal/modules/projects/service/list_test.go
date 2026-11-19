package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	authdomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	typesproject "github.com/thekrauss/kubemanager/internal/modules/projects/domain"
)

func TestListProjectsTruncatesAndMaps(t *testing.T) {
	org := uuid.New()
	repo := &fakeProjectRepo{}
	for i := 0; i < typesproject.MaxListedProjects+5; i++ {
		repo.rows = append(repo.rows, authdomain.Project{ID: uuid.New(), Name: "p", Status: "READY",
			OrganizationID: org, CreatedAt: time.Unix(int64(i), 0)})
	}
	s := &ProjectService{Repos: repo}
	out, err := s.ListProjects(context.Background(), []uuid.UUID{org}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Projects) != typesproject.MaxListedProjects || !out.Truncated {
		t.Fatalf("len=%d truncated=%v", len(out.Projects), out.Truncated)
	}
	if repo.listed[0].limit != typesproject.MaxListedProjects+1 {
		t.Fatalf("limit=%d (doit demander un de plus pour détecter la troncature)", repo.listed[0].limit)
	}
	if out.Projects[0].OrganizationID != org.String() || out.Projects[0].Status != "READY" {
		t.Fatalf("mapping : %+v", out.Projects[0])
	}

	// Aucune organisation ni projet : liste vide, jamais null.
	empty, err := (&ProjectService{Repos: &fakeProjectRepo{}}).ListProjects(context.Background(), nil, nil, nil)
	if err != nil || empty.Projects == nil || len(empty.Projects) != 0 || empty.Truncated {
		t.Fatalf("vide : %+v %v", empty, err)
	}
}
