// Package action : les actions de l'Opérateur (ADR 0016 §6).
//
// L'agent n'agit jamais lui-même. Il PROPOSE une action typée du catalogue ;
// la proposition devient une carte dans le dossier ; une personne de l'équipe
// la valide dans la console ; l'Opérateur l'exécute alors, avec la trace de
// cette validation (journal recopié dans l'audit, ordre signé pour les
// nœuds). Une action hors catalogue n'existe pas : pas de shell, pas de
// commande libre, pas d'URL fabriquée.
//
// Chaque fournisseur (nœuds, clusters, puis l'API de la plateforme) déclare
// ses actions, vérifie une proposition (Prepare : la cible existe, les
// paramètres sont bornés, le risque est évalué sur l'état réel) et l'exécute
// après validation (Execute).
package action

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

// Risk : gravité, affichée sur la carte.
type Risk string

const (
	// RiskLow : sans interruption visible (pod recréé par son contrôleur,
	// redémarrage progressif).
	RiskLow Risk = "low"
	// RiskMedium : effet visible et réversible (service redémarré, nœud isolé).
	RiskMedium Risk = "medium"
	// RiskHigh : interruption probable ou large (plan de contrôle, mise à
	// zéro) : la personne retape le nom de la cible pour valider.
	RiskHigh Risk = "high"
)

// Rank : ordre des risques (comparaisons).
func (r Risk) Rank() int {
	switch r {
	case RiskLow:
		return 1
	case RiskMedium:
		return 2
	case RiskHigh:
		return 3
	}
	return 0
}

// Capability : droit exigé pour VALIDER une action (proposer n'engage rien).
type Capability string

const (
	// CapInfrastructure : chemin de secours (nœuds, clusters) :
	// platform.clusters.manage.
	CapInfrastructure Capability = "infrastructure"
	// CapAPI : l'API décide (droits de la route, à la validation) ; il
	// faut seulement une session du chemin normal (pas de secours).
	CapAPI Capability = "api"
)

// Spec : une action du catalogue.
type Spec struct {
	// Kind : identifiant stable, « domaine.objet.verbe » (ex. node.service.restart).
	Kind        string
	Description string
	// Parameters : schéma JSON (objet) des paramètres, sans la justification
	// (ajoutée par le catalogue des propositions).
	Parameters json.RawMessage
	Capability Capability
	// External : la validation a lieu ailleurs (l'API, avec la session de
	// la personne) ; l'Opérateur ne fait que relire l'issue (Remote).
	External bool
}

// Outcome : issue d'une action validée ailleurs.
type Outcome struct {
	// Done : décidée (exécutée, en échec ou refusée) ; sinon en attente.
	Done bool
	OK   bool
	// Expired : expirée sans décision (rien n'a été fait).
	Expired   bool
	Rejected  bool
	DecidedBy string
	Result    string
}

// Remote : fournisseur dont les actions sont validées et jouées ailleurs.
type Remote interface {
	Outcome(ctx context.Context, kind string, params json.RawMessage) (Outcome, error)
	Reject(ctx context.Context, kind string, params json.RawMessage, note string) error
}

var kindRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9_]+){1,3}$`)

// ValidKind : forme d'un identifiant d'action.
func ValidKind(k string) bool { return kindRe.MatchString(k) }

// Prepared : une proposition vérifiée, prête à être présentée.
type Prepared struct {
	// Target : la cible lisible (ex. « server-1 / k3s »).
	Target string
	// Summary : ce qui sera fait, en une phrase, pour la carte.
	Summary string
	// Params : paramètres normalisés, rejoués tels quels à l'exécution.
	Params json.RawMessage
	Risk   Risk
	// Confirm : nom que la personne retape pour valider (risque élevé) ;
	// vide : un clic suffit.
	Confirm string
	// State : état observé au moment de la proposition (pour la carte).
	State string
}

// Approval : la validation humaine.
type Approval struct {
	CardID          string
	InvestigationID string
	ApprovedBy      string // e-mail de la personne
}

// Provider : un fournisseur d'actions.
type Provider interface {
	Actions() []Spec
	Prepare(ctx context.Context, kind string, params json.RawMessage) (Prepared, error)
	// Execute : rend un compte rendu court ; une erreur est un échec de
	// l'action (la carte passe en échec, avec le message).
	Execute(ctx context.Context, kind string, params json.RawMessage, a Approval) (string, error)
}

// Errors.
var (
	ErrUnknown = errors.New("action inconnue")
	// ErrRefused : la proposition ne tient pas (cible absente, garde-fou).
	ErrRefused = errors.New("action refusée")
)

// Registry : les fournisseurs, par action.
type Registry struct {
	specs []Spec
	by    map[string]Provider
	spec  map[string]Spec
}

// NewRegistry : réunit des fournisseurs ; une action en double est une erreur.
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{by: map[string]Provider{}, spec: map[string]Spec{}}
	for _, p := range providers {
		if p == nil {
			continue
		}
		for _, s := range p.Actions() {
			if !ValidKind(s.Kind) {
				return nil, errors.New("operator: action mal nommée : " + s.Kind)
			}
			if _, dup := r.by[s.Kind]; dup {
				return nil, errors.New("operator: action en double : " + s.Kind)
			}
			r.by[s.Kind], r.spec[s.Kind] = p, s
			r.specs = append(r.specs, s)
		}
	}
	return r, nil
}

// With : le catalogue augmenté d'un fournisseur (par exemple les actions de
// l'API ouvertes à une personne, pour un tour).
func (r *Registry) With(p Provider) (*Registry, error) {
	var base []Provider
	seen := map[Provider]bool{}
	if r != nil {
		for _, s := range r.specs {
			if pr := r.by[s.Kind]; !seen[pr] {
				seen[pr] = true
				base = append(base, pr)
			}
		}
	}
	return NewRegistry(append(base, p)...)
}

// Provider : le fournisseur d'une action.
func (r *Registry) Provider(kind string) (Provider, bool) { return r.provider(kind) }

// Specs : les actions du catalogue.
func (r *Registry) Specs() []Spec {
	if r == nil {
		return nil
	}
	return r.specs
}

// Spec : une action.
func (r *Registry) Spec(kind string) (Spec, bool) {
	if r == nil {
		return Spec{}, false
	}
	s, ok := r.spec[kind]
	return s, ok
}

// Prepare : vérifie une proposition.
func (r *Registry) Prepare(ctx context.Context, kind string, params json.RawMessage) (Prepared, error) {
	p, ok := r.provider(kind)
	if !ok {
		return Prepared{}, ErrUnknown
	}
	return p.Prepare(ctx, kind, params)
}

// Execute : joue une action validée.
func (r *Registry) Execute(ctx context.Context, kind string, params json.RawMessage, a Approval) (string, error) {
	p, ok := r.provider(kind)
	if !ok {
		return "", ErrUnknown
	}
	if a.CardID == "" || a.ApprovedBy == "" {
		return "", errors.New("operator: une action exige la carte et la personne qui l'a validée")
	}
	return p.Execute(ctx, kind, params, a)
}

func (r *Registry) provider(kind string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.by[kind]
	return p, ok
}
