package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/infrastructure/certstatus"
)

// MaxDomainsPerWorkload : domaines d'un conteneur.
const MaxDomainsPerWorkload = 10

// ChallengePrefix : nom de l'enregistrement TXT de preuve de propriété.
const ChallengePrefix = "_betorun-challenge."

// Types d'enregistrement DNS demandés pour pointer vers la plateforme.
const (
	RecordCNAME = "CNAME"
	RecordA     = "A"
	RecordTXT   = "TXT"
	// RecordALIAS : CNAME au sommet d'une zone (ALIAS, ANAME, « CNAME
	// flattening ») ; proposé à la place des A quand le fournisseur le permet.
	RecordALIAS = "ALIAS"
)

// WorkloadDomain : domaine personnalisé d'un conteneur.
type WorkloadDomain struct {
	ID         uuid.UUID `gorm:"type:uuid;primary_key" json:"id"`
	WorkloadID uuid.UUID `gorm:"type:uuid;not null" json:"workload_id"`
	Hostname   string    `gorm:"not null" json:"hostname"`
	// Path : préfixe routé vers ce conteneur (« / » : tout le domaine). Un
	// même nom peut servir plusieurs conteneurs d'un projet, un chemin chacun.
	Path       string     `gorm:"not null;default:'/'" json:"path"`
	Token      string     `gorm:"not null" json:"-"`
	Verified   bool       `gorm:"not null" json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (WorkloadDomain) TableName() string { return "workload_domains" }

// Route : nom et chemin routés vers un conteneur.
type Route struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

// HostClaim : domaine vérifié portant un nom, avec le projet de son conteneur.
type HostClaim struct {
	WorkloadDomain
	ProjectID uuid.UUID
}

// DNSRecord : enregistrement à créer chez le fournisseur DNS.
type DNSRecord struct {
	Type string `json:"type"`
	// Name : nom complet (FQDN).
	Name string `json:"name"`
	// Host : nom à saisir chez la plupart des fournisseurs, relatif à la zone
	// (« @ » pour le sommet, « www », « _betorun-challenge.www »).
	Host  string `json:"host"`
	Value string `json:"value"`
	// Alternative : à créer À LA PLACE des enregistrements A de même nom, si
	// le fournisseur propose ALIAS / ANAME (l'adresse suit alors la plateforme).
	Alternative bool `json:"alternative,omitempty"`
}

// DomainResponse : domaine et instructions.
type DomainResponse struct {
	WorkloadDomain
	// Records : preuve de propriété (TXT), puis routage (CNAME ou A).
	Records []DNSRecord `json:"records"`
	// Pointing : le domaine pointe déjà vers la plateforme (nécessaire au
	// certificat HTTPS). Connu après une vérification.
	Pointing bool `json:"pointing"`
	// Zone : domaine enregistré chez le registraire (ex. exemple.sn).
	Zone string `json:"zone"`
	// Apex : le nom est le sommet de sa zone (pas de CNAME possible).
	Apex bool `json:"apex"`
	// DNSProvider : fournisseur DNS reconnu par ses serveurs (cloudflare,
	// ovh…) ; vide si inconnu. Sert au guide pas à pas de la console.
	DNSProvider string `json:"dns_provider,omitempty"`
	// SharedProof : la preuve est celle d'un autre chemin du même nom dans le
	// projet, déjà en place.
	SharedProof bool `json:"shared_proof,omitempty"`
	// HTTPS : la plateforme demande un certificat pour ce domaine (faux :
	// mode sans certificat, le domaine n'est servi qu'en HTTP).
	HTTPS bool `json:"https"`
	// Certificate : état du certificat HTTPS dans le cluster (domaine vérifié
	// et HTTPS actif) ; absent si rien à montrer ou cluster injoignable.
	Certificate *certstatus.Status `json:"certificate,omitempty"`
	// Check : ce que la vérification a lu dans le DNS (après « Vérifier »).
	Check *DomainCheck `json:"check,omitempty"`
}

// DomainCheck : diagnostic d'une vérification, pour comprendre un échec.
type DomainCheck struct {
	// Resolver : résolveur interrogé (ex. 1.1.1.1:53, ou « system »).
	Resolver string `json:"resolver"`
	// Name : nom interrogé pour la preuve (_betorun-challenge.<nom>).
	Name string `json:"name"`
	// TXTFound : valeurs TXT lues sous ce nom (5 au plus).
	TXTFound []string `json:"txt_found"`
	// ProofFound : la valeur attendue est parmi elles.
	ProofFound bool `json:"proof_found"`
	// Error : NXDOMAIN, délai dépassé… ; vide si la lecture a réussi.
	Error string `json:"error,omitempty"`
}

// MaxCheckValues : valeurs TXT rapportées au plus (et 255 caractères chacune).
const MaxCheckValues = 5

// AddDomainRequest : nouveau domaine.
type AddDomainRequest struct {
	Hostname string `json:"hostname" binding:"required" description:"Domaine (ex: www.exemple.sn)"`
	Path     string `json:"path" description:"Chemin servi par ce conteneur (défaut /), ex. /auth : le reste du domaine peut aller à un autre conteneur du projet"`
}

var (
	ErrDomainNotFound  = errors.New("domain not found")
	ErrDomainExists    = errors.New("this domain is already attached to this container")
	ErrDomainClaimed   = errors.New("this domain is already in use on betorun")
	ErrDomainLimit     = errors.New("too many domains for this container (10 maximum)")
	ErrDomainsDisabled = errors.New("custom domains are not available: the platform has no public address")
	ErrInvalidPath     = errors.New("invalid path: starts with /, at most 100 characters, letters, digits and - . _ ~ / only")
)
