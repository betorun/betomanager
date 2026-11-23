// Package service : IP de sortie dédiée par projet (ADR 0014).
package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/egressagent"
	"github.com/thekrauss/kubemanager/internal/modules/egress/domain"
	"github.com/thekrauss/kubemanager/internal/modules/egress/repository"
)

// Store : réserve (repository.Store).
type Store interface {
	List(ctx context.Context, clusterID string) ([]domain.AdminAddress, error)
	Declare(ctx context.Context, a *domain.Address) error
	Delete(ctx context.Context, clusterID string, id uuid.UUID) error
	ForProject(ctx context.Context, project uuid.UUID) (*domain.Address, error)
	FreeCount(ctx context.Context, clusterID string) (int, error)
	Assign(ctx context.Context, clusterID string, project uuid.UUID, by string, now time.Time, only []string) (*domain.Address, error)
	Release(ctx context.Context, id uuid.UUID, now time.Time) error
	Place(ctx context.Context, project uuid.UUID, defaultCluster string) (*repository.ProjectPlace, error)
	Orphans(ctx context.Context) ([]repository.Orphan, error)
	Clusters(ctx context.Context) ([]string, error)
}

// Nodes : consigne des agents réseau et ce qu'ils portent (lot 2,
// repository.Nodes). nil : IP montées à la main (lot 1).
type Nodes interface {
	Publish(ctx context.Context, clusterID string, d egressagent.Desired) error
	// Mounted : par nœud, les IP portées ; agent false : aucun agent sur le cluster.
	Mounted(ctx context.Context, clusterID string) (map[string]map[string]bool, bool, error)
}

// Gateway : pose et retire la règle de sortie d'un projet sur son cluster.
type Gateway interface {
	Apply(ctx context.Context, b domain.Binding) error
	Remove(ctx context.Context, clusterID, namespace string) error
}

// Service : réserve, demandes et restitutions.
type Service struct {
	Store Store
	// Gateways : implémentation par sorte de passerelle (domain.GatewayCilium…).
	Gateways map[string]Gateway
	Nodes    Nodes
	// DefaultCluster : cluster des projets sans cluster.
	DefaultCluster string
	Logger         *zap.SugaredLogger
	Now            func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) place(ctx context.Context, project uuid.UUID) (*repository.ProjectPlace, error) {
	p, err := s.Store.Place(ctx, project, s.DefaultCluster)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, domain.ErrProjectNotFound
	}
	return p, nil
}

func (s *Service) gateway(kind string) Gateway {
	if kind == domain.GatewayNone {
		return nil
	}
	return s.Gateways[kind]
}

// Get : IP du projet, et si l'on peut en demander une.
func (s *Service) Get(ctx context.Context, project uuid.UUID) (*domain.ProjectEgress, error) {
	a, err := s.Store.ForProject(ctx, project)
	if err != nil {
		return nil, err
	}
	p, err := s.place(ctx, project)
	if err != nil {
		return nil, err
	}
	out := &domain.ProjectEgress{Offered: s.gateway(p.Gateway) != nil}
	if a != nil {
		out.Address, out.AssignedAt, out.Offered = a.Address, a.AssignedAt, true
		return out, nil
	}
	if out.Offered {
		only, err := s.usable(ctx, p.ClusterID)
		if err != nil {
			return nil, err
		}
		if only == nil {
			n, err := s.Store.FreeCount(ctx, p.ClusterID)
			if err != nil {
				return nil, err
			}
			out.Available = n > 0
		} else {
			out.Available = len(only) > 0
		}
	}
	return out, nil
}

// Request : une IP libre du cluster du projet, puis la règle posée ; la
// règle refusée, l'IP revient dans la réserve.
func (s *Service) Request(ctx context.Context, project uuid.UUID, by string) (*domain.ProjectEgress, error) {
	p, err := s.place(ctx, project)
	if err != nil {
		return nil, err
	}
	gw := s.gateway(p.Gateway)
	if gw == nil {
		return nil, domain.ErrNoGateway
	}
	if cur, err := s.Store.ForProject(ctx, project); err != nil {
		return nil, err
	} else if cur != nil {
		return nil, domain.ErrAlreadyHasAddress
	}
	only, err := s.usable(ctx, p.ClusterID)
	if err != nil {
		return nil, err
	}
	a, err := s.Store.Assign(ctx, p.ClusterID, project, by, s.now(), only)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, domain.ErrNoFreeAddress
	}
	if err := gw.Apply(ctx, domain.Binding{ClusterID: p.ClusterID, Namespace: p.Namespace, Address: a.Address, GatewayNode: a.GatewayNode}); err != nil {
		s.undo(a.ID)
		return nil, err
	}
	return &domain.ProjectEgress{Address: a.Address, AssignedAt: a.AssignedAt, Offered: true}, nil
}

// Release : règle retirée, puis l'IP rendue (elle cesse d'être facturée).
func (s *Service) Release(ctx context.Context, project uuid.UUID) error {
	a, err := s.Store.ForProject(ctx, project)
	if err != nil {
		return err
	}
	if a == nil {
		return domain.ErrNoAddress
	}
	p, err := s.place(ctx, project)
	if err != nil {
		return err
	}
	if gw := s.gateway(p.Gateway); gw != nil {
		if err := gw.Remove(ctx, a.ClusterID, p.Namespace); err != nil {
			return err
		}
	}
	return s.Store.Release(ctx, a.ID, s.now())
}

// undo : l'IP revient dans la réserve même si la requête est interrompue.
func (s *Service) undo(id uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Store.Release(ctx, id, s.now()); err != nil && s.Logger != nil {
		s.Logger.Errorw("egress address not released after a failed apply", "address", id, "error", err)
	}
}

// Sweep : IP des projets supprimés rendues (règle retirée au mieux : le
// namespace disparaît de toute façon).
func (s *Service) Sweep(ctx context.Context) {
	orphans, err := s.Store.Orphans(ctx)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warnw("egress sweep failed", "error", err)
		}
		return
	}
	for _, o := range orphans {
		for kind, gw := range s.Gateways {
			if err := gw.Remove(ctx, o.ClusterID, o.Namespace); err != nil && s.Logger != nil {
				s.Logger.Warnw("egress policy not removed", "gateway", kind, "address", o.Address.Address, "error", err)
			}
		}
		if err := s.Store.Release(ctx, o.ID, s.now()); err != nil && s.Logger != nil {
			s.Logger.Warnw("egress address not released", "address", o.Address.Address, "error", err)
		}
	}
}

// reconcile : consigne de chaque cluster qui a une réserve republiée
// (cluster recréé, consigne effacée à la main, agent posé après coup).
func (s *Service) reconcile(ctx context.Context) {
	if s.Nodes == nil {
		return
	}
	ids, err := s.Store.Clusters(ctx)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warnw("egress clusters not listed", "error", err)
		}
		return
	}
	for _, id := range ids {
		s.syncLogged(ctx, id)
	}
}

// Run : passe de ménage, toutes les every.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.Sweep(ctx)
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ── Back-office ────────────────────────────────────────────────────────

// DeclareInput : nouvelle IP d'un cluster, déjà montée sur le nœud passerelle.
type DeclareInput struct {
	Address     string `json:"address" binding:"required" description:"Adresse IPv4 publique, déjà montée sur l'interface du nœud passerelle"`
	GatewayNode string `json:"gateway_node" binding:"required" description:"Nom du nœud passerelle qui porte l'adresse"`
	Notes       string `json:"notes,omitempty" description:"Notes de l'équipe (référence chez l'hébergeur…)"`
}

// List : réserve d'un cluster, et si chaque IP est montée sur son nœud
// (connu seulement avec un agent réseau).
func (s *Service) List(ctx context.Context, clusterID string) ([]domain.AdminAddress, error) {
	list, err := s.Store.List(ctx, clusterID)
	if err != nil || s.Nodes == nil {
		return list, err
	}
	mounted, agent, err := s.Nodes.Mounted(ctx, clusterID)
	if err != nil || !agent {
		return list, nil // cluster muet : l'état reste inconnu
	}
	for i := range list {
		m := mounted[list[i].GatewayNode][list[i].Address.Address]
		list[i].Mounted = &m
	}
	return list, nil
}

// usable : IP libres et montées sur leur nœud ; nil : pas d'agent réseau
// (montage à la main, toute IP libre convient).
func (s *Service) usable(ctx context.Context, clusterID string) ([]string, error) {
	if s.Nodes == nil {
		return nil, nil
	}
	mounted, agent, err := s.Nodes.Mounted(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	if !agent {
		return nil, nil
	}
	list, err := s.Store.List(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	only := []string{}
	for _, a := range list {
		if !a.Assigned && mounted[a.GatewayNode][a.Address.Address] {
			only = append(only, a.Address.Address)
		}
	}
	return only, nil
}

// Sync : consigne du cluster = toute sa réserve, par nœud (montée d'avance,
// libre ou attribuée). Sans agent réseau, elle n'a pas d'effet.
func (s *Service) Sync(ctx context.Context, clusterID string) error {
	if s.Nodes == nil {
		return nil
	}
	list, err := s.Store.List(ctx, clusterID)
	if err != nil {
		return err
	}
	d := egressagent.Desired{}
	for _, a := range list {
		d[a.GatewayNode] = append(d[a.GatewayNode], a.Address.Address)
	}
	return s.Nodes.Publish(ctx, clusterID, d)
}

func (s *Service) syncLogged(ctx context.Context, clusterID string) {
	if err := s.Sync(ctx, clusterID); err != nil && s.Logger != nil {
		s.Logger.Warnw("egress addresses not published to the cluster", "cluster", clusterID, "error", err)
	}
}

// Declare : IP ajoutée à la réserve du cluster.
func (s *Service) Declare(ctx context.Context, clusterID string, in DeclareInput, by string) (*domain.AdminAddress, error) {
	addr := domain.NormalizeAddress(in.Address)
	node := strings.TrimSpace(in.GatewayNode)
	switch {
	case addr == "":
		return nil, domain.ErrInvalidAddress
	case !domain.ValidNode(node):
		return nil, domain.ErrInvalidNode
	case len([]rune(in.Notes)) > 500:
		return nil, domain.ErrNotesTooLong
	}
	now := s.now()
	a := &domain.Address{ID: uuid.New(), ClusterID: clusterID, Address: addr, GatewayNode: node,
		Notes: strings.TrimSpace(in.Notes), CreatedBy: by, CreatedAt: now, UpdatedAt: now}
	if err := s.Store.Declare(ctx, a); err != nil {
		return nil, err
	}
	s.syncLogged(ctx, clusterID)
	return &domain.AdminAddress{Address: *a}, nil
}

// Remove : IP libre retirée de la réserve.
func (s *Service) Remove(ctx context.Context, clusterID string, id uuid.UUID) error {
	if err := s.Store.Delete(ctx, clusterID, id); err != nil {
		return err
	}
	s.syncLogged(ctx, clusterID)
	return nil
}
