package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// Duplicate : nouveau conteneur du même projet, même image et mêmes réglages,
// variables et secrets recopiés ici, côté serveur (les secrets ne passent
// jamais par la console). Les domaines et le disque ne suivent pas : un nom
// ne sert qu'un conteneur, et le disque de la copie part vide.
func (s *WorkloadService) Duplicate(ctx context.Context, source uuid.UUID, name string, caller iam.Principal) (*domain.Workload, error) {
	w, err := s.Repo.GetByID(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := ensureNotDeleting(w); err != nil {
		return nil, err
	}
	if err := ensureBuilt(w); err != nil {
		return nil, err
	}
	spec, err := s.specFor(w)
	if err != nil {
		return nil, err
	}
	spec.Name, spec.ProjectID, spec.Image = name, w.ProjectID.String(), w.Image
	spec.EnvVars, spec.SecretData = map[string]string{}, map[string]string{}
	if s.Variables != nil {
		views, err := s.Variables.List(ctx, w.ID)
		if err != nil {
			return nil, err
		}
		values, err := s.Variables.Values(ctx, w.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range views {
			value, ok := values[v.Name]
			if !ok {
				continue
			}
			if v.Secret {
				spec.SecretData[v.Name] = value
			} else {
				spec.EnvVars[v.Name] = value
			}
		}
	}
	return s.Deploy(ctx, &WorkloadServiceRequest{CreateWorkloadRequest: spec, Caller: caller})
}

// propagateImage : déploie image sur les conteneurs du projet qui suivent
// celui-ci (un seul niveau : pas de chaîne ni de boucle). Au mieux : un
// suiveur arrêté ou en cours de déploiement est passé, et le journal le dit.
func (s *WorkloadService) propagateImage(ctx context.Context, id, image string) {
	wID, err := uuid.Parse(id)
	if err != nil {
		return
	}
	src, err := s.Repo.GetByID(ctx, wID)
	if err != nil {
		return
	}
	list, err := s.Repo.ListByProject(ctx, src.ProjectID)
	if err != nil {
		s.warn("image followers not listed", "workload", src.ID, "error", err)
		return
	}
	for i := range list {
		f := &list[i]
		if f.ID == src.ID || f.Image == image || f.Status == utils.WorkloadStopped || ensureNotDeleting(f) != nil {
			continue
		}
		spec, err := s.specFor(f)
		if err != nil || spec.ImageFrom != src.Name {
			continue
		}
		if err := s.update(ctx, f.ID.String(), domain.UpdateWorkloadRequest{Image: image}); err != nil {
			s.warn("image follower not updated", "workload", f.ID, "source", src.ID, "error", err)
		}
	}
}
