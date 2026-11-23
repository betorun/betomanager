package service

import (
	"context"
	"net"
	"strings"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/core/configs"
)

// profile : réglages du cluster qui porte un projet (socle multi-cluster,
// lot 2c), repliés sur la configuration du déploiement champ par champ.
type profile struct {
	IngressClass     string
	IngressNamespace string
	// PublicIP : IPv4 des adresses automatiques <nom>-<projet>.<ip>.sslip.io
	// (vide : pas d'adresse automatique).
	PublicIP string
	// Target : cible des domaines personnalisés (IPv4 ou nom DNS).
	Target string
	// StorageClass : classe des disques des nouveaux conteneurs (vide : celle
	// demandée, sinon local-path).
	StorageClass string
}

// configProfile : réglages de la configuration (cluster par défaut).
func (s *WorkloadService) configProfile() profile {
	p := profile{
		IngressClass:     configs.AppConfig.Kubernetes.IngressClass,
		IngressNamespace: configs.AppConfig.Kubernetes.IngressNamespace,
		PublicIP:         strings.TrimSpace(configs.AppConfig.Vps.AdressIp),
	}
	if s.DomainCfg != nil {
		p.Target = s.DomainCfg.Target
	}
	return p
}

// profileFor : réglages du cluster du projet. Une erreur de lecture est
// rendue : un déploiement ne part jamais avec les réglages d'un autre cluster.
func (s *WorkloadService) profileFor(ctx context.Context, project uuid.UUID) (profile, error) {
	p := s.configProfile()
	if s.Clusters == nil {
		return p, nil
	}
	st, err := s.Clusters.SettingsForProject(ctx, project)
	if err != nil {
		return p, err
	}
	if st.IngressClass != "" {
		p.IngressClass = st.IngressClass
	}
	if st.IngressNamespace != "" {
		p.IngressNamespace = st.IngressNamespace
	}
	p.StorageClass = st.StorageClass
	if t := strings.TrimSpace(st.IngressTarget); t != "" {
		p.Target = t
		// sslip.io ne connaît que des adresses : un nom DNS est résolu (sa
		// première IPv4) ; introuvable, pas d'adresse automatique plutôt que
		// celle d'un autre cluster.
		p.PublicIP = ""
		if ip := net.ParseIP(t); ip != nil && ip.To4() != nil {
			p.PublicIP = t
		} else if s.DomainCfg != nil && s.DomainCfg.Resolver != nil {
			lookup, cancel := context.WithTimeout(ctx, domainLookupTimeout)
			ips, _ := s.DomainCfg.Resolver.LookupHost(lookup, t)
			cancel()
			for _, a := range ips {
				if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
					p.PublicIP = a
					break
				}
			}
		}
	}
	return p, nil
}

// domainTarget : cible des domaines d'un conteneur (cluster de son projet) ;
// illisible : celle de la configuration (simple affichage).
func (s *WorkloadService) domainTarget(ctx context.Context, workload uuid.UUID) string {
	fallback := s.configProfile().Target
	if s.Clusters == nil {
		return fallback
	}
	w, err := s.Repo.GetByID(ctx, workload)
	if err != nil {
		return fallback
	}
	p, err := s.profileFor(ctx, w.ProjectID)
	if err != nil || p.Target == "" {
		return fallback
	}
	return p.Target
}
