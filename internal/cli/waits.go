package cli

import (
	"context"
	"errors"
	"time"

	"github.com/thekrauss/kubemanager/internal/cli/api"
	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/ui"
)

// Attentes communes : un projet en préparation, un déploiement en cours.
// Remplaçables dans les tests.
var (
	projectPollEvery = 2 * time.Second
	projectTimeout   = 5 * time.Minute
	busyRetryEvery   = attachRetryEvery
	busyTimeout      = createTimeout
)

// sleepCtx : attend d, ou l'annulation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// retryWhileDeploying : rejoue call tant que l'API répond DEPLOY_IN_PROGRESS
// (un déploiement de name bloque la modification) ; hintKey s'affiche une fois.
func retryWhileDeploying(ctx context.Context, p *ui.Printer, name, hintKey, timeoutKey string, call func() error) error {
	deadline, warned := time.Now().Add(busyTimeout), false
	for {
		err := call()
		var apiErr *api.Error
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != codeDeployInProgress {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New(i18n.T(timeoutKey, name))
		}
		if !warned && !flagJSON {
			p.Hint(i18n.T(hintKey, name))
			warned = true
		}
		if err := sleepCtx(ctx, busyRetryEvery); err != nil {
			return err
		}
	}
}

// waitProjectReady : attend que le projet créé soit prêt (namespace, quotas)
// avant de rendre la main ; les commandes suivantes échoueraient sinon.
func waitProjectReady(ctx context.Context, cl *api.Client, p *ui.Printer, projectID string) error {
	deadline := time.Now().Add(projectTimeout)
	for {
		list, err := cl.ListProjects(ctx)
		if err != nil {
			return err
		}
		for _, pr := range list.Projects {
			if pr.ProjectID != projectID {
				continue
			}
			switch pr.Status {
			case projectStatusReady:
				return nil
			case projectStatusError:
				return errors.New(i18n.T("proj.create.failed", pr.Name))
			}
		}
		if time.Now().After(deadline) {
			return errors.New(i18n.T("proj.create.timeout"))
		}
		if err := sleepCtx(ctx, projectPollEvery); err != nil {
			return err
		}
	}
}
