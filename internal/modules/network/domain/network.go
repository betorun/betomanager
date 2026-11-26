// Package domain : réseau privé d'un projet, décrit sans rien du système
// qui l'applique. Aujourd'hui des NetworkPolicy Kubernetes (chart
// standard-app, bases CloudNativePG) ; demain le pare-feu d'un autre
// hébergeur (Proxmox, VPS reliés au projet). Toute implémentation doit
// garantir PolicyRules ; la console affiche ce modèle, jamais l'outil.
package domain

import "time"

// Sortes de membres du réseau d'un projet.
const (
	KindWeb      = "web"
	KindWorker   = "worker"
	KindCron     = "cron"
	KindService  = "service"
	KindDatabase = "database"
	// KindVPS : VPS rattaché au réseau privé du projet (ADR 0019).
	KindVPS = "vps"
)

// Côtés d'une règle.
const (
	PeerProject       = "project"        // membres du projet
	PeerDatabases     = "databases"      // bases gérées du projet
	PeerPublicWeb     = "public_web"     // conteneurs web avec une adresse publique ou un domaine vérifié
	PeerInternet      = "internet"       // tout ce qui n'est pas la plateforme
	PeerOtherProjects = "other_projects" // projets voisins, même organisation comprise
	PeerPrivateRanges = "private_ranges" // adresses privées (RFC 1918…) : réseau de la plateforme
)

// Ports d'une règle.
const (
	PortsAll      = "all"
	PortsHTTP     = "http_https" // 80/443 au point d'entrée de la plateforme
	PortsPostgres = "5432"
	PortsListed   = "listed" // ceux de PortList
)

// EgressPorts : ports TCP ouverts vers Internet (HTTP(S), envoi de courriel
// 465/587, bases externes MySQL, Postgres, Redis, MongoDB). Le 25 n'y est
// jamais (anti-spam). Source unique : le chart des conteneurs les lit ici.
var EgressPorts = []int{80, 443, 465, 587, 3306, 5432, 6379, 27017}

// Rule : une garantie de la politique réseau (code stable, traduit par la console).
type Rule struct {
	Code  string `json:"code"`
	From  string `json:"from"`
	To    string `json:"to"`
	Ports string `json:"ports"`
	// PortList : ports quand Ports vaut "listed".
	PortList []int `json:"port_list,omitempty"`
	Allowed  bool  `json:"allowed"`
}

// PolicyRules : politique v1 de tout projet, dans l'ordre d'affichage.
func PolicyRules() []Rule {
	return []Rule{
		{Code: "project_internal", From: PeerProject, To: PeerProject, Ports: PortsAll, Allowed: true},
		{Code: "project_to_databases", From: PeerProject, To: PeerDatabases, Ports: PortsPostgres, Allowed: true},
		{Code: "internet_to_public_web", From: PeerInternet, To: PeerPublicWeb, Ports: PortsHTTP, Allowed: true},
		{Code: "internet_to_internal", From: PeerInternet, To: PeerProject, Ports: PortsAll, Allowed: false},
		{Code: "other_projects", From: PeerOtherProjects, To: PeerProject, Ports: PortsAll, Allowed: false},
		{Code: "egress_internet", From: PeerProject, To: PeerInternet, Ports: PortsListed, PortList: append([]int(nil), EgressPorts...), Allowed: true},
		{Code: "egress_private", From: PeerProject, To: PeerPrivateRanges, Ports: PortsAll, Allowed: false},
	}
}

// Address : adresse d'un membre dans le réseau privé du projet (nom court,
// résolu dans le projet seulement).
type Address struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// URL : forme à copier (http://api, cache:6379, db-main-rw:5432).
	URL string `json:"url"`
}

// Sources d'une adresse publique.
const (
	PublicAutomatic = "automatic" // adresse donnée par la plateforme
	PublicDomain    = "domain"    // domaine du client, vérifié
)

// PublicEndpoint : adresse joignable depuis Internet.
type PublicEndpoint struct {
	URL    string `json:"url"`
	Source string `json:"source"`
}

// Member : conteneur ou base du projet.
type Member struct {
	ID       string           `json:"id"`
	Kind     string           `json:"kind"`
	Name     string           `json:"name"`
	Status   string           `json:"status"`
	Internal *Address         `json:"internal,omitempty"` // nil : n'écoute rien (worker, cron)
	Public   []PublicEndpoint `json:"public"`
}

// Sortes de liens connus entre membres.
const (
	// LinkDatabase : base branchée sur un conteneur (adresse posée en variable secrète).
	LinkDatabase = "database"
	// LinkWaitsFor : le conteneur attend cette adresse avant de démarrer.
	LinkWaitsFor = "waits_for"
)

// Link : un membre qui en joint un autre (déclaré, pas observé : le réseau
// du projet permet aussi tout lien non déclaré entre ses membres).
type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	// Variable : variable du conteneur qui porte l'adresse (base branchée).
	Variable string `json:"variable,omitempty"`
}

// ProjectNetwork : réseau privé d'un projet.
type ProjectNetwork struct {
	Members []Member  `json:"members"`
	Links   []Link    `json:"links"`
	Rules   []Rule    `json:"rules"`
	At      time.Time `json:"at"`
}
