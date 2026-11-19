// Package service : accès d'un projet à ses registres d'images externes.
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	projectDomain "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/registries/domain"
	"github.com/thekrauss/kubemanager/internal/modules/registries/pull"
	"github.com/thekrauss/kubemanager/internal/modules/registries/repository"
)

const refreshTimeout = 15 * time.Second

// Projects : nom du projet, pour son namespace.
type Projects interface {
	GetProjectByID(ctx context.Context, id string) (*projectDomain.Project, error)
}

// Service : accès aux registres externes.
type Service struct {
	Store    repository.Store
	Projects Projects
	// Pull : réécrit le Secret de pull d'un projet déjà déployé (nil : au
	// prochain déploiement seulement).
	Pull *pull.Secrets
	// PlatformHost : registre de la plateforme, refusé comme registre externe.
	PlatformHost string
	Prober       *Prober
	Logger       *zap.SugaredLogger
}

func (s *Service) host(raw string) (string, error) {
	h := domain.NormalizeHost(raw)
	if h == "" {
		return "", domain.ErrInvalidHost
	}
	if s.PlatformHost != "" && strings.EqualFold(h, s.PlatformHost) {
		return "", domain.ErrPlatformRegistry
	}
	return h, nil
}

// List : accès du projet, sans jeton.
func (s *Service) List(ctx context.Context, project uuid.UUID) (*domain.ListResponse, error) {
	views, err := s.Store.List(ctx, project)
	if err != nil {
		return nil, err
	}
	return &domain.ListResponse{Registries: views}, nil
}

// Put : crée ou remplace l'accès, puis met à jour le Secret de pull.
func (s *Service) Put(ctx context.Context, project uuid.UUID, req domain.PutRequest, by *uuid.UUID) (*domain.View, error) {
	host, err := s.host(req.Host)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateCredentials(req.Username, req.Password); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(req.Username)
	view, err := s.Store.Put(ctx, project, host, username, req.Password, by)
	if err != nil {
		return nil, err
	}
	s.refresh(ctx, project)
	return &view, nil
}

// Delete : supprime l'accès et le retire tout de suite du Secret de pull.
func (s *Service) Delete(ctx context.Context, project uuid.UUID, rawHost string) error {
	host := domain.NormalizeHost(rawHost)
	if host == "" {
		return domain.ErrNotFound
	}
	if err := s.Store.Delete(ctx, project, host); err != nil {
		return err
	}
	s.refresh(ctx, project)
	return nil
}

// Test : vérifie l'accès auprès du registre.
func (s *Service) Test(ctx context.Context, project uuid.UUID, rawHost string) (*domain.TestResponse, error) {
	host := domain.NormalizeHost(rawHost)
	if host == "" {
		return nil, domain.ErrNotFound
	}
	auths, err := s.Store.Auths(ctx, project, host)
	if err != nil {
		return nil, err
	}
	if len(auths) == 0 {
		return nil, domain.ErrNotFound
	}
	return &domain.TestResponse{Result: s.Prober.Check(ctx, auths[0])}, nil
}

// refresh : un échec n'annule pas la modification enregistrée ; le
// prochain déploiement réécrit le Secret.
func (s *Service) refresh(ctx context.Context, project uuid.UUID) {
	if s.Pull == nil || s.Projects == nil {
		return
	}
	// Le retrait d'un accès doit aboutir même si le client a déjà raccroché.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	p, err := s.Projects.GetProjectByID(ctx, project.String())
	if err == nil {
		err = s.Pull.Refresh(ctx, p.Namespace, project)
	}
	if err != nil && !errors.Is(err, context.Canceled) && s.Logger != nil {
		s.Logger.Warnw("registry pull secret not refreshed", "project", project, "error", err)
	}
}
