package service

import (
	"context"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/auth"
	"github.com/thekrauss/kubemanager/internal/core/configs"
	registriesDomain "github.com/thekrauss/kubemanager/internal/modules/registries/domain"
	sitesDomain "github.com/thekrauss/kubemanager/internal/modules/sites/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/activities"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/domain"
	"github.com/thekrauss/kubemanager/internal/modules/workloads/workflows"
)

// Domaines personnalisés. La propriété se prouve par un enregistrement TXT
// propre à chaque demande : pointer vers la plateforme ne suffit pas, sinon
// n'importe quel client pourrait capter un domaine qui nous vise déjà. Aucune
// route n'existe avant la preuve, et une demande non prouvée ne réserve rien.

const domainLookupTimeout = 8 * time.Second

// DNSResolver : ce qu'il faut savoir du DNS public (remplaçable en test).
type DNSResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
}

// DomainSettings : réglages des domaines.
type DomainSettings struct {
	Resolver DNSResolver
	// Target : adresse vers laquelle pointer (nom d'hôte : CNAME, IP : A).
	Target string
	// TLS : certificat pour chaque domaine (faux seulement en développement).
	TLS bool
}

// DefaultDomainDNSServers : résolveurs publics des vérifications de domaine.
// Celui de la machine garde en cache une réponse négative (le TXT lu avant
// sa création), parfois une demi-heure : la preuve publiée restait
// « introuvable ».
const DefaultDomainDNSServers = "1.1.1.1:53,8.8.8.8:53"

// NewResolver : résolveur DNS. servers : liste « hôte:port » séparée par des
// virgules, essayés dans l'ordre ; vide = résolveurs publics par défaut ;
// « system » = celui de la machine.
func NewResolver(servers string) DNSResolver {
	servers = strings.TrimSpace(servers)
	if servers == "system" {
		return net.DefaultResolver
	}
	if servers == "" {
		servers = DefaultDomainDNSServers
	}
	var list []string
	for _, s := range strings.Split(servers, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, s)
		}
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		var last error
		for _, s := range list {
			c, err := d.DialContext(ctx, network, s)
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}}
}

// records : preuve (TXT) puis routage. Sous-domaine : CNAME vers la cible
// (l'adresse peut changer sans rien demander au client) ou A si la cible est
// une IP. Sommet de zone (exemple.sn) : un CNAME y est interdit, donc A vers
// les adresses de la cible, et ALIAS proposé à la place quand la cible est un
// nom (le fournisseur suit alors seul un changement d'adresse).
func (s *WorkloadService) records(ctx context.Context, d *domain.WorkloadDomain) []domain.DNSRecord {
	zone := domain.Zone(d.Hostname)
	rel := func(name string) string { return domain.RelativeHost(name, zone) }
	challenge := domain.ChallengePrefix + d.Hostname
	out := []domain.DNSRecord{{Type: domain.RecordTXT, Name: challenge, Host: rel(challenge), Value: d.Token}}
	target := s.DomainCfg.Target
	targetIsIP := net.ParseIP(target) != nil
	switch {
	case targetIsIP:
		out = append(out, domain.DNSRecord{Type: domain.RecordA, Name: d.Hostname, Host: rel(d.Hostname), Value: target})
	case !domain.IsApex(d.Hostname):
		out = append(out, domain.DNSRecord{Type: domain.RecordCNAME, Name: d.Hostname, Host: rel(d.Hostname), Value: target})
	default:
		lookup, cancel := context.WithTimeout(ctx, domainLookupTimeout)
		ips, _ := s.DomainCfg.Resolver.LookupHost(lookup, target)
		cancel()
		for _, ip := range ips {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
				out = append(out, domain.DNSRecord{Type: domain.RecordA, Name: d.Hostname, Host: rel(d.Hostname), Value: ip})
			}
		}
		out = append(out, domain.DNSRecord{Type: domain.RecordALIAS, Name: d.Hostname, Host: rel(d.Hostname), Value: target, Alternative: true})
	}
	return out
}

func (s *WorkloadService) domainView(ctx context.Context, d *domain.WorkloadDomain, pointing bool, providers map[string]string) domain.DomainResponse {
	zone := domain.Zone(d.Hostname)
	return domain.DomainResponse{WorkloadDomain: *d, Records: s.records(ctx, d), Pointing: pointing,
		Zone: zone, Apex: zone == d.Hostname, DNSProvider: s.dnsProvider(ctx, zone, providers)}
}

// dnsProvider : fournisseur DNS de la zone (guide de la console) ; providers
// sert de cache le temps d'une requête (plusieurs domaines, une zone).
func (s *WorkloadService) dnsProvider(ctx context.Context, zone string, providers map[string]string) string {
	if p, ok := providers[zone]; ok {
		return p
	}
	lookup, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()
	ns, err := s.DomainCfg.Resolver.LookupNS(lookup, zone)
	provider := ""
	if err == nil {
		hosts := make([]string, 0, len(ns))
		for _, n := range ns {
			hosts = append(hosts, n.Host)
		}
		provider = domain.DNSProviderOf(hosts)
	}
	if providers != nil {
		providers[zone] = provider
	}
	return provider
}

func (s *WorkloadService) domainsReady() error {
	if s.Domains == nil || s.DomainCfg == nil || s.DomainCfg.Target == "" {
		return domain.ErrDomainsDisabled
	}
	return nil
}

// ListDomains : domaines du conteneur et enregistrements DNS à créer.
func (s *WorkloadService) ListDomains(ctx context.Context, workload uuid.UUID) ([]domain.DomainResponse, error) {
	if err := s.domainsReady(); err != nil {
		return nil, err
	}
	list, err := s.Domains.List(ctx, workload)
	if err != nil {
		return nil, err
	}
	out := make([]domain.DomainResponse, 0, len(list))
	providers := map[string]string{}
	for i := range list {
		out = append(out, s.domainView(ctx, &list[i], false, providers))
	}
	return out, nil
}

// AddDomain : enregistre la demande, sans effet sur le routage.
// Un chemin (/auth) partage le nom avec d'autres conteneurs du projet ; si le
// nom y est déjà prouvé, sa preuve est reprise : rien de plus à créer.
func (s *WorkloadService) AddDomain(ctx context.Context, workload uuid.UUID, hostname, path string) (*domain.DomainResponse, error) {
	if err := s.domainsReady(); err != nil {
		return nil, err
	}
	w, err := s.Repo.GetByID(ctx, workload)
	if err != nil {
		return nil, err
	}
	if err := ensureNotDeleting(w); err != nil {
		return nil, err
	}
	if kindOf(w) != domain.KindWeb {
		return nil, domain.ErrNotWeb
	}
	host, err := sitesDomain.NormalizeHostname(hostname, configs.AppConfig.Sites.BaseDomain)
	if err != nil {
		return nil, err
	}
	routePath, err := domain.NormalizePath(path)
	if err != nil {
		return nil, err
	}
	existing, err := s.Domains.List(ctx, workload)
	if err != nil {
		return nil, err
	}
	if len(existing) >= domain.MaxDomainsPerWorkload {
		return nil, domain.ErrDomainLimit
	}
	// Même nom déjà demandé dans le projet : même preuve (un seul TXT), qu'elle
	// soit déjà validée ou encore en attente.
	token, shared, err := s.Domains.ProjectHostToken(ctx, w.ProjectID, host)
	if err != nil {
		return nil, err
	}
	if token == "" {
		random, err := auth.RandomToken()
		if err != nil {
			return nil, err
		}
		token = "betorun-verify=" + random
	}
	d := &domain.WorkloadDomain{ID: uuid.New(), WorkloadID: workload, Hostname: host, Path: routePath, Token: token, CreatedAt: time.Now()}
	if err := s.Domains.Create(ctx, d); err != nil {
		return nil, err
	}
	view := s.domainView(ctx, d, false, nil)
	view.SharedProof = shared
	return &view, nil
}

// VerifyDomain : vérifie la preuve TXT. Prouvé, le domaine est routé (nouveau
// déploiement). Il peut reprendre un nom réservé par un autre conteneur si la
// preuve de ce dernier a disparu : le domaine a changé de mains.
func (s *WorkloadService) VerifyDomain(ctx context.Context, workload, id uuid.UUID) (*domain.DomainResponse, error) {
	if err := s.domainsReady(); err != nil {
		return nil, err
	}
	d, err := s.Domains.Get(ctx, workload, id)
	if err != nil {
		return nil, err
	}
	w, err := s.Repo.GetByID(ctx, workload)
	if err != nil {
		return nil, err
	}
	if err := ensureNotDeleting(w); err != nil {
		return nil, err
	}
	lookup, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	defer cancel()
	txt := s.challengeTXT(lookup, d.Hostname)
	pointing := s.pointsToPlatform(lookup, d.Hostname)

	if !slices.Contains(txt, d.Token) {
		// Preuve disparue sur un domaine déjà vérifié : il reste routé tant
		// que personne d'autre ne le prouve (une erreur DNS passagère ne doit
		// pas couper un service en production).
		if err := s.Domains.Touch(ctx, d.ID); err != nil {
			return nil, err
		}
		view := s.domainView(ctx, d, pointing, nil)
		return &view, nil
	}
	if d.Verified {
		view := s.domainView(ctx, d, pointing, nil)
		return &view, nil
	}
	if w.Status == utils.WorkloadStopped {
		return nil, domain.ErrWorkloadStopped
	}
	if err := s.ensureNoDeployRunning(ctx, workload); err != nil {
		return nil, err
	}
	taken, err := s.Domains.SiteHostnameTaken(ctx, d.Hostname)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, domain.ErrDomainClaimed
	}
	// Autres routes vérifiées sous ce nom. Un autre projet le garde tant que
	// sa preuve est en place ; sinon le nom a changé de mains et toutes ses
	// routes tombent. Dans le projet, seule la route au même chemin cède la
	// place (déplacée vers ce conteneur) ; les autres chemins restent.
	claims, err := s.Domains.HostClaims(ctx, d.Hostname, d.ID)
	if err != nil {
		return nil, err
	}
	var released []domain.HostClaim
	for _, c := range claims {
		if c.ProjectID != w.ProjectID {
			if slices.Contains(txt, c.Token) {
				return nil, domain.ErrDomainClaimed
			}
			released = append(released, c)
		} else if c.Path == d.Path {
			released = append(released, c)
		}
	}
	for _, c := range released {
		if err := s.Domains.SetVerified(ctx, c.ID, false); err != nil {
			return nil, err
		}
	}
	if err := s.Domains.SetVerified(ctx, d.ID, true); err != nil {
		return nil, err
	}
	if err := s.redeploy(ctx, w); err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{workload: true}
	for _, c := range released {
		if !seen[c.WorkloadID] {
			seen[c.WorkloadID] = true
			s.releaseFrom(ctx, c.WorkloadID)
		}
	}
	updated, err := s.Domains.Get(ctx, workload, id)
	if err != nil {
		return nil, err
	}
	view := s.domainView(ctx, updated, pointing, nil)
	return &view, nil
}

// releaseFrom : retire la route de l'ancien conteneur. Au mieux : le nouveau
// propriétaire a la preuve, et le prochain déploiement de l'ancien la retirera.
func (s *WorkloadService) releaseFrom(ctx context.Context, workload uuid.UUID) {
	w, err := s.Repo.GetByID(ctx, workload)
	if err == nil {
		err = s.ensureNoDeployRunning(ctx, workload)
	}
	if err == nil {
		err = s.redeploy(ctx, w)
	}
	if err != nil && s.Logger != nil {
		s.Logger.Warnw("custom domain: previous route not removed yet", "workload", workload, "error", err)
	}
}

// RemoveDomain : retire le domaine et sa route.
func (s *WorkloadService) RemoveDomain(ctx context.Context, workload, id uuid.UUID) error {
	if err := s.domainsReady(); err != nil {
		return err
	}
	d, err := s.Domains.Get(ctx, workload, id)
	if err != nil {
		return err
	}
	if !d.Verified {
		return s.Domains.Delete(ctx, workload, id)
	}
	w, err := s.Repo.GetByID(ctx, workload)
	if err != nil {
		return err
	}
	if err := ensureNotDeleting(w); err != nil {
		return err
	}
	if w.Status == utils.WorkloadStopped {
		return domain.ErrWorkloadStopped
	}
	if err := s.ensureNoDeployRunning(ctx, workload); err != nil {
		return err
	}
	if err := s.Domains.Delete(ctx, workload, id); err != nil {
		return err
	}
	return s.redeploy(ctx, w)
}

// redeploy : même configuration, routes à jour.
func (s *WorkloadService) redeploy(ctx context.Context, w *domain.Workload) error {
	// Sans image : le premier déploiement reprendra ce changement.
	if w.Image == "" {
		return nil
	}
	spec, err := s.specFor(w)
	if err != nil {
		return err
	}
	in, err := s.inputFor(ctx, w, spec)
	if err != nil {
		return err
	}
	return s.startDeploy(ctx, w, in)
}

// inputFor : entrée du workflow, avec les domaines vérifiés.
func (s *WorkloadService) inputFor(ctx context.Context, w *domain.Workload, spec domain.CreateWorkloadRequest) (workflows.DeployWorkloadInput, error) {
	in := s.deployInput(w, spec)
	if s.Domains != nil {
		routes, err := s.Domains.VerifiedRoutes(ctx, w.ID)
		if err != nil {
			return in, err
		}
		in.CustomRoutes = make([]activities.DomainRoute, 0, len(routes))
		for _, r := range routes {
			in.CustomRoutes = append(in.CustomRoutes, activities.DomainRoute{Host: r.Host, Path: r.Path})
		}
		in.CustomDomainTLS = s.DomainCfg != nil && s.DomainCfg.TLS
	}
	if s.ExternalRegistries != nil && !in.PlatformImage && w.Image != "" {
		has, err := s.ExternalRegistries.Has(ctx, w.ProjectID, registriesDomain.ImageHost(w.Image))
		if err != nil {
			return in, err
		}
		in.ExternalPull = has
	}
	return in, nil
}

func (s *WorkloadService) challengeTXT(ctx context.Context, host string) []string {
	records, err := s.DomainCfg.Resolver.LookupTXT(ctx, domain.ChallengePrefix+host)
	if err != nil {
		// Absent ou résolveur en défaut : la console dit « preuve introuvable »,
		// le journal dit pourquoi.
		if s.Logger != nil {
			s.Logger.Infow("domain proof lookup failed", "name", domain.ChallengePrefix+host, "error", err)
		}
		return nil
	}
	for i := range records {
		records[i] = strings.TrimSpace(records[i])
	}
	return records
}

// pointsToPlatform : toutes les adresses du domaine sont celles de la cible.
func (s *WorkloadService) pointsToPlatform(ctx context.Context, host string) bool {
	hostIPs, err := s.DomainCfg.Resolver.LookupHost(ctx, host)
	if err != nil || len(hostIPs) == 0 {
		return false
	}
	targets := []string{s.DomainCfg.Target}
	if net.ParseIP(s.DomainCfg.Target) == nil {
		if targets, err = s.DomainCfg.Resolver.LookupHost(ctx, s.DomainCfg.Target); err != nil {
			return false
		}
	}
	for _, ip := range hostIPs {
		if !slices.Contains(targets, ip) {
			return false
		}
	}
	return true
}
