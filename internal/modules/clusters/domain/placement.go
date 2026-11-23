package domain

import (
	"errors"
	"regexp"
	"sort"
)

// Placement d'un nouveau projet (socle multi-cluster, lot 2a) : le client
// choisit une région ; la plateforme prend le cluster ouvert de cette région
// qui a le plus de place.

var (
	// ErrNoClusterInRegion : aucun cluster ouvert et en état de recevoir un
	// projet dans la région demandée.
	ErrNoClusterInRegion = errors.New("no capacity available in this region, choose another region")
	ErrInvalidRegionCode = errors.New("invalid region")
	// ErrClusterHasProjects : un cluster qui porte des projets ne se retire pas.
	ErrClusterHasProjects   = errors.New("this cluster still hosts projects")
	ErrInvalidSetting       = errors.New("invalid cluster setting: lowercase letters, digits, dots or dashes")
	ErrInvalidEgressGateway = errors.New("invalid egress gateway: cilium or empty")
)

var (
	regionCodeRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	settingRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// ValidRegionCode : code de région demandé par un client.
func ValidRegionCode(r string) bool { return regionCodeRe.MatchString(r) }

// ValidEgressGateway : passerelle de sortie connue ("" : aucune).
func ValidEgressGateway(g string) bool { return g == "" || g == "cilium" }

// ValidSetting : classe d'ingress, namespace, classe de stockage ou cible
// (nom DNS ou adresse IPv4) ; vide : celui de la configuration.
func ValidSetting(v string) bool { return v == "" || settingRe.MatchString(v) }

// Candidate : un cluster vu par le placement.
type Candidate struct {
	ClusterID string
	Region    string
	State     string
	IsDefault bool
	// Reachable : un accès existe (kubeconfig déposé ou configuration).
	Reachable bool
	Health    *Health
	Projects  int
}

// eligible : ouvert, joignable, pas en panne. Jamais relevé : seulement le
// défaut (installation à un cluster, worker pas encore passé).
func (c Candidate) eligible() bool {
	if c.State != StateActive || !c.Reachable {
		return false
	}
	if c.Health == nil {
		return c.IsDefault
	}
	return c.Health.State == HealthOK || c.Health.State == HealthDegraded
}

// free : part de CPU allouable encore libre (0 si inconnue).
func (c Candidate) free() float64 {
	if c.Health == nil || c.Health.CPUAllocMilli <= 0 {
		return 0
	}
	return float64(c.Health.CPUAllocMilli-c.Health.CPUReqMilli) / float64(c.Health.CPUAllocMilli)
}

// Place : cluster d'un nouveau projet. Région vide : le défaut s'il peut
// recevoir, sinon le meilleur ; aucun : le défaut quand même ("" s'il n'y en
// a pas), le workflow de création dira s'il ne répond pas. Région demandée
// sans cluster éligible : ErrNoClusterInRegion.
func Place(cs []Candidate, region string) (string, error) {
	var pool []Candidate
	def := ""
	for _, c := range cs {
		if c.IsDefault {
			def = c.ClusterID
		}
		if c.eligible() && (region == "" || c.Region == region) {
			pool = append(pool, c)
		}
	}
	if len(pool) == 0 {
		if region != "" {
			return "", ErrNoClusterInRegion
		}
		return def, nil
	}
	if region == "" {
		for _, c := range pool {
			if c.IsDefault {
				return c.ClusterID, nil
			}
		}
	}
	// Plus de place d'abord (pas de pression avant tout), puis moins de
	// projets, puis l'identifiant (choix stable).
	sort.SliceStable(pool, func(i, j int) bool {
		pi, pj := pool[i].pressured(), pool[j].pressured()
		if pi != pj {
			return !pi
		}
		if fi, fj := pool[i].free(), pool[j].free(); fi != fj {
			return fi > fj
		}
		if pool[i].Projects != pool[j].Projects {
			return pool[i].Projects < pool[j].Projects
		}
		return pool[i].ClusterID < pool[j].ClusterID
	})
	return pool[0].ClusterID, nil
}

func (c Candidate) pressured() bool {
	if c.Health == nil {
		return false
	}
	for _, r := range c.Health.Reasons {
		if r == ReasonCPUPressure || r == ReasonMemoryPressure {
			return true
		}
	}
	return false
}

// Region : une région proposée au client à la création d'un projet.
type Region struct {
	Code string `json:"code" description:"Code de la région (ex. eu-west-gra)"`
	// Available : au moins un cluster peut y recevoir un projet maintenant.
	Available bool `json:"available" description:"Un nouveau projet peut y être créé maintenant"`
	// Default : région du cluster par défaut (choix proposé d'office).
	Default bool `json:"default" description:"Région proposée par défaut"`
}

// Regions : régions des clusters ouverts (sans région : non proposés).
func Regions(cs []Candidate) []Region {
	byCode := map[string]*Region{}
	for _, c := range cs {
		if c.Region == "" || c.State != StateActive {
			continue
		}
		r, ok := byCode[c.Region]
		if !ok {
			r = &Region{Code: c.Region}
			byCode[c.Region] = r
		}
		r.Available = r.Available || c.eligible()
		r.Default = r.Default || c.IsDefault
	}
	out := make([]Region, 0, len(byCode))
	for _, r := range byCode {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Code < out[j].Code
	})
	return out
}
