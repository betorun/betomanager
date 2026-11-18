package router

import (
	"github.com/thekrauss/kubemanager/internal/modules/registries"
	registriesSvc "github.com/thekrauss/kubemanager/internal/modules/registries/service"
)

// newRegistriesController : accès des projets aux registres externes.
func (a *App) newRegistriesController() *registries.Controller {
	if a.Registries == nil {
		return nil
	}
	svc := &registriesSvc.Service{
		Store:    a.Registries,
		Projects: a.Repos.Project,
		Pull:     a.pullSecrets(),
		Prober:   registriesSvc.NewProber(),
		Logger:   a.Logger,
	}
	if a.buildsEnabled() {
		svc.PlatformHost = a.Config.Builds.RegistryHost
	}
	return &registries.Controller{Service: svc}
}
