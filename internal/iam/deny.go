package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Politiques de refus (iam.md §14). Une politique posée sur une organisation
// ou un projet refuse des permissions à des principals, quels que soient
// leurs rôles, sauf à ses exceptions. Elle s'applique à la ressource et à
// toutes ses descendantes, et passe avant les rôles : un refus l'emporte
// toujours sur une autorisation.
//
// Un accès accordé sur "platform" (staff betorun) n'est pas concerné : une
// organisation règle les droits de ses membres, pas ceux de l'exploitant.

// DenyPolicy : une politique de refus.
type DenyPolicy struct {
	ID          uuid.UUID    `json:"id"`
	Resource    ResourceName `json:"resource"`
	DisplayName string       `json:"display_name"`
	Description string       `json:"description,omitempty"`
	// DeniedPermissions : permissions exactes, ou motifs "service.*",
	// "service.ressource.*" et "*" (toutes).
	DeniedPermissions []string `json:"denied_permissions"`
	// DeniedPrincipals : à qui (allUsers : tout le monde).
	DeniedPrincipals []Principal `json:"denied_principals"`
	// ExceptionPrincipals : jamais refusés par cette politique.
	ExceptionPrincipals []Principal `json:"exception_principals"`
	// Condition : la politique cesse à cette date (même format que les bindings).
	Condition *Condition `json:"condition,omitempty"`
	Etag      string     `json:"etag"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedBy string     `json:"updated_by"`
	UpdatedAt time.Time  `json:"updated_at"`

	// broken : ligne illisible, lue comme « tout refuser à tous » sauf la
	// gestion des politiques de refus (pour pouvoir la corriger).
	broken bool
}

// DenyStore : politiques de refus posées sur des ressources (optionnel pour un Store).
type DenyStore interface {
	DenyPoliciesOn(ctx context.Context, resources []ResourceName) ([]DenyPolicy, error)
}

// denyPoliciesPrefix : permissions de gestion des politiques de refus.
const denyPoliciesPrefix = "iam.denyPolicies."

// ErrInvalidDenyPermission : motif de permission inconnu ou réservé au staff.
var ErrInvalidDenyPermission = errors.New("invalid denied permission")

// ValidateDenyPermission : permission connue (hors staff) ou motif qui en
// couvre au moins une.
func ValidateDenyPermission(pattern string) error {
	if pattern == "*" {
		return nil
	}
	for p, info := range catalog {
		if !info.PlatformOnly && permissionMatches(pattern, p) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrInvalidDenyPermission, pattern)
}

// permissionMatches : "*", préfixe terminé par ".*", ou permission exacte.
func permissionMatches(pattern string, p Permission) bool {
	if pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		return strings.HasPrefix(string(p), prefix+".")
	}
	return pattern == string(p)
}

// active : une condition illisible garde la politique active (échec fermé :
// pour un refus, fermé veut dire refuser).
func (d *DenyPolicy) active(now time.Time) bool {
	if d.Condition == nil {
		return true
	}
	deadline, err := conditionDeadline(d.Condition)
	if err != nil {
		return true
	}
	return now.Before(deadline)
}

// Denies : la politique refuse-t-elle perm à un principal dont principals
// est l'expansion (lui, ses groupes, allUsers…) ?
func (d *DenyPolicy) Denies(principals []Principal, perm Permission, now time.Time) bool {
	if d.broken && strings.HasPrefix(string(perm), denyPoliciesPrefix) {
		return false
	}
	if !d.active(now) || !containsAny(d.DeniedPrincipals, principals) || containsAny(d.ExceptionPrincipals, principals) {
		return false
	}
	for _, pat := range d.DeniedPermissions {
		if permissionMatches(pat, perm) {
			return true
		}
	}
	return false
}

func containsAny(list, set []Principal) bool {
	for _, a := range list {
		for _, b := range set {
			if a == b {
				return true
			}
		}
	}
	return false
}

// firstDeny : première politique qui refuse perm (nil : aucune).
func firstDeny(policies []DenyPolicy, principals []Principal, perm Permission, now time.Time) *DenyPolicy {
	for i := range policies {
		if policies[i].Denies(principals, perm, now) {
			return &policies[i]
		}
	}
	return nil
}

//  Cache du moteur

type cachedDenies struct {
	list    []DenyPolicy
	expires time.Time
}

// denyPolicies : politiques posées sur la chaîne de ressources (platform
// exclue) ; fromCache : toutes venaient du cache.
func (e *Engine) denyPolicies(ctx context.Context, ancestry []ResourceName, force bool) ([]DenyPolicy, bool, error) {
	ds, ok := e.store.(DenyStore)
	if !ok {
		return nil, false, nil
	}
	now := e.now()
	var out []DenyPolicy
	var missing []ResourceName
	e.mu.Lock()
	for _, r := range ancestry {
		if r == Platform {
			continue
		}
		if c, ok := e.denies[r]; ok && !force && now.Before(c.expires) {
			out = append(out, c.list...)
			continue
		}
		missing = append(missing, r)
	}
	e.mu.Unlock()
	if len(missing) == 0 {
		return out, true, nil
	}
	loaded, err := ds.DenyPoliciesOn(ctx, missing)
	if err != nil {
		return nil, false, fmt.Errorf("iam: load deny policies: %w", err)
	}
	byRes := make(map[ResourceName][]DenyPolicy, len(missing))
	for _, d := range loaded {
		byRes[d.Resource] = append(byRes[d.Resource], d)
	}
	if e.cacheTTL > 0 {
		e.mu.Lock()
		for _, r := range missing {
			e.denies[r] = cachedDenies{list: byRes[r], expires: now.Add(e.cacheTTL)}
		}
		e.mu.Unlock()
	}
	for _, r := range missing {
		out = append(out, byRes[r]...)
	}
	return out, false, nil
}

// applyDenies : une autorisation venue d'une ressource client est retirée si
// une politique la refuse. Rend vrai si au moins un refus a été prononcé.
func applyDenies(out []Decision, perms []Permission, denies []DenyPolicy, principals []Principal, now time.Time) bool {
	hit := false
	for i, perm := range perms {
		if !out[i].Allowed || out[i].Resource == Platform {
			continue
		}
		if d := firstDeny(denies, principals, perm, now); d != nil {
			out[i] = Decision{Reason: fmt.Sprintf("%s denied by policy %q on %s", perm, d.DisplayName, d.Resource),
				DenyPolicy: &DenyRef{ID: d.ID, Name: d.DisplayName, Resource: d.Resource}}
			hit = true
		}
	}
	return hit
}

// DenyRef : politique qui a refusé (réponse 403, journal d'audit).
type DenyRef struct {
	ID       uuid.UUID    `json:"id"`
	Name     string       `json:"name"`
	Resource ResourceName `json:"resource"`
}

//  Stockage ─

type denyPolicyRow struct {
	ID                  uuid.UUID `gorm:"type:uuid;primaryKey"`
	ResourceName        string
	DisplayName         string
	Description         string
	DeniedPermissions   []byte `gorm:"type:jsonb"`
	DeniedPrincipals    []byte `gorm:"type:jsonb"`
	ExceptionPrincipals []byte `gorm:"type:jsonb"`
	Condition           []byte `gorm:"type:jsonb"`
	Etag                string
	CreatedBy           string
	CreatedAt           time.Time
	UpdatedBy           string
	UpdatedAt           time.Time
}

func (denyPolicyRow) TableName() string { return "iam_deny_policies" }

func (s *GormStore) DenyPoliciesOn(ctx context.Context, resources []ResourceName) ([]DenyPolicy, error) {
	keys := make([]string, len(resources))
	for i, r := range resources {
		keys[i] = string(r)
	}
	var rows []denyPolicyRow
	if err := s.DB.WithContext(ctx).Where("resource_name IN ?", keys).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]DenyPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.policy())
	}
	return out, nil
}

// policy : ligne → politique. Une colonne illisible donne une politique qui
// refuse tout à tout le monde (échec fermé), visible et corrigible.
func (r denyPolicyRow) policy() DenyPolicy {
	d := DenyPolicy{ID: r.ID, Resource: ResourceName(r.ResourceName), DisplayName: r.DisplayName, Description: r.Description,
		Etag: r.Etag, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt,
		DeniedPermissions: []string{}, DeniedPrincipals: []Principal{}, ExceptionPrincipals: []Principal{}}
	broken := json.Unmarshal(r.DeniedPermissions, &d.DeniedPermissions) != nil ||
		json.Unmarshal(r.DeniedPrincipals, &d.DeniedPrincipals) != nil
	if len(r.ExceptionPrincipals) > 0 && json.Unmarshal(r.ExceptionPrincipals, &d.ExceptionPrincipals) != nil {
		broken = true
	}
	if len(r.Condition) > 0 {
		var c Condition
		if json.Unmarshal(r.Condition, &c) != nil {
			c = Condition{Title: "invalid", Expression: "invalid"}
		}
		d.Condition = &c
	}
	if broken {
		d.DeniedPermissions, d.DeniedPrincipals, d.ExceptionPrincipals = []string{"*"}, []Principal{AllUsers}, []Principal{}
		d.broken = true
	}
	return d
}

func (s *GormStore) DenyPoliciesInOrganizations(ctx context.Context, orgs []uuid.UUID) ([]DenyPolicy, error) {
	names := make([]string, len(orgs))
	for i, o := range orgs {
		names[i] = string(OrganizationName(o))
	}
	var rows []denyPolicyRow
	err := s.DB.WithContext(ctx).Where(`resource_name IN ? OR resource_name IN
		(SELECT 'projects/' || id::text FROM projects WHERE organization_id IN ? AND deleted_at IS NULL)`, names, orgs).
		Order("created_at, id").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]DenyPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.policy())
	}
	return out, nil
}
