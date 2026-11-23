// Package service : réseau d'un projet, lu dans la base (conteneurs, bases,
// domaines vérifiés, branchements, attentes au démarrage). Rien n'est lu sur
// le cluster : la vue ne dépend pas du système qui applique la politique.
package service

import (
	"context"
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	dbDomain "github.com/thekrauss/kubemanager/internal/modules/databases/domain"
	"github.com/thekrauss/kubemanager/internal/modules/network/domain"
	wlDomain "github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
)

// Workloads : conteneurs du projet (hors supprimés).
type Workloads interface {
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]wlDomain.Workload, error)
}

// Domains : domaines personnalisés d'un conteneur.
type Domains interface {
	List(ctx context.Context, workload uuid.UUID) ([]wlDomain.WorkloadDomain, error)
}

// Databases : bases gérées du projet et leurs branchements.
type Databases interface {
	List(ctx context.Context, projectID uuid.UUID) ([]dbDomain.Database, error)
	ListAttachments(ctx context.Context, databaseID uuid.UUID) ([]dbDomain.Attachment, error)
}

// Service : vue réseau d'un projet. Domains et Databases facultatifs.
type Service struct {
	Workloads Workloads
	Domains   Domains
	Databases Databases
	// DomainTLS : domaines servis en https (sinon http, développement).
	DomainTLS bool
	Now       func() time.Time
}

// webPort : port du Service d'un conteneur web dans le projet.
const webPort = 80

// Get : membres, liens déclarés et règles du réseau du projet.
func (s *Service) Get(ctx context.Context, projectID uuid.UUID) (*domain.ProjectNetwork, error) {
	wls, err := s.Workloads.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	members := make([]domain.Member, 0, len(wls))
	byHost := map[string]string{} // nom interne → membre
	waits := map[string][]string{}
	for i := range wls {
		w := &wls[i]
		m, err := s.workloadMember(ctx, w)
		if err != nil {
			return nil, err
		}
		members = append(members, m)
		byHost[w.Name] = m.ID
		waits[m.ID] = waitFor(w)
	}

	var links []domain.Link
	attached := map[[2]string]bool{}
	if s.Databases != nil {
		dbs, err := s.Databases.List(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, d := range dbs {
			host := d.ClusterName() + "-rw"
			id := d.ID.String()
			members = append(members, domain.Member{ID: id, Kind: domain.KindDatabase, Name: d.Name, Status: d.Status,
				Internal: &domain.Address{Host: host, Port: dbDomain.Port, URL: host + ":" + strconv.Itoa(dbDomain.Port)},
				Public:   []domain.PublicEndpoint{}})
			byHost[host] = id
			atts, err := s.Databases.ListAttachments(ctx, d.ID)
			if err != nil {
				return nil, err
			}
			for _, a := range atts {
				from := a.WorkloadID.String()
				links = append(links, domain.Link{From: from, To: id, Kind: domain.LinkDatabase, Variable: a.Variable})
				attached[[2]string{from, id}] = true
			}
		}
	}

	// Attentes au démarrage : un lien vers le membre qui porte l'adresse
	// (une base déjà branchée garde son seul lien « base »).
	known := map[string]bool{}
	for _, m := range members {
		known[m.ID] = true
	}
	for from, targets := range waits {
		for _, t := range targets {
			to, ok := byHost[t]
			if !ok || to == from || attached[[2]string{from, to}] || !known[from] {
				continue
			}
			links = append(links, domain.Link{From: from, To: to, Kind: domain.LinkWaitsFor})
			attached[[2]string{from, to}] = true
		}
	}

	sortMembers(members)
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].From != links[j].From {
			return links[i].From < links[j].From
		}
		return links[i].To < links[j].To
	})
	if links == nil {
		links = []domain.Link{}
	}
	return &domain.ProjectNetwork{Members: members, Links: links, Rules: domain.PolicyRules(), At: s.now()}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) workloadMember(ctx context.Context, w *wlDomain.Workload) (domain.Member, error) {
	kind := w.Kind
	if kind == "" {
		kind = domain.KindWeb
	}
	m := domain.Member{ID: w.ID.String(), Kind: kind, Name: w.Name, Status: w.Status, Public: []domain.PublicEndpoint{}}
	switch kind {
	case domain.KindWeb:
		m.Internal = &domain.Address{Host: w.Name, Port: webPort, URL: "http://" + w.Name}
	case domain.KindService:
		if w.TargetPort > 0 {
			m.Internal = &domain.Address{Host: w.Name, Port: w.TargetPort, URL: w.Name + ":" + strconv.Itoa(w.TargetPort)}
		}
	}
	if kind != domain.KindWeb {
		return m, nil // jamais public : seul un conteneur web reçoit une entrée
	}
	if u := strings.TrimSpace(w.ExternalURL); u != "" {
		m.Public = append(m.Public, domain.PublicEndpoint{URL: u, Source: domain.PublicAutomatic})
	}
	if s.Domains == nil {
		return m, nil
	}
	ds, err := s.Domains.List(ctx, w.ID)
	if err != nil {
		return m, err
	}
	scheme := "http://"
	if s.DomainTLS {
		scheme = "https://"
	}
	for _, d := range ds {
		if !d.Verified {
			continue // pas encore routé
		}
		u := scheme + d.Hostname
		if d.Path != "" && d.Path != "/" {
			u += d.Path
		}
		m.Public = append(m.Public, domain.PublicEndpoint{URL: u, Source: domain.PublicDomain})
	}
	return m, nil
}

// waitFor : hôtes attendus au démarrage (spec « wait_for », nom:port).
func waitFor(w *wlDomain.Workload) []string {
	if len(w.Spec) == 0 {
		return nil
	}
	var spec struct {
		WaitFor []string `json:"wait_for"`
	}
	if json.Unmarshal(w.Spec, &spec) != nil {
		return nil
	}
	out := make([]string, 0, len(spec.WaitFor))
	for _, a := range spec.WaitFor {
		host, _, err := net.SplitHostPort(strings.TrimSpace(a))
		if err != nil {
			continue
		}
		// Forme longue (nom.espace.svc) : le nom court.
		if i := strings.IndexByte(host, '.'); i > 0 {
			host = host[:i]
		}
		out = append(out, host)
	}
	return out
}

var kindOrder = map[string]int{domain.KindWeb: 0, domain.KindService: 1, domain.KindWorker: 2, domain.KindCron: 3, domain.KindDatabase: 4}

func sortMembers(ms []domain.Member) {
	sort.SliceStable(ms, func(i, j int) bool {
		if kindOrder[ms[i].Kind] != kindOrder[ms[j].Kind] {
			return kindOrder[ms[i].Kind] < kindOrder[ms[j].Kind]
		}
		return ms[i].Name < ms[j].Name
	})
}
