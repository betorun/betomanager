package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/thekrauss/kubemanager/internal/audit"
)

// Gestion des politiques de refus (iam.md §14). Toute écriture passe par le
// step-up (DenyPoliciesUpdate est sensible) et laisse une trace d'audit.

var (
	ErrDenyPolicyNotFound   = errors.New("deny policy not found")
	ErrDenyPolicyParent     = errors.New("deny policies live on an organization or a project")
	ErrDenyPolicyName       = errors.New("display_name is required (at most 100 characters)")
	ErrDenyPolicyPerms      = errors.New("a deny policy denies between 1 and 100 permissions")
	ErrDenyPolicyPrincipals = errors.New("a deny policy targets between 1 and 100 principals, with at most 100 exceptions")
	ErrDenyPolicyLimit      = errors.New("at most 50 deny policies per resource")
	ErrDenyPolicyEtag       = errors.New("the deny policy was modified concurrently: reload it and retry")
	// ErrDenyPolicyLockout : la politique retirerait à tous les propriétaires
	// de l'organisation le droit de gérer les politiques de refus.
	ErrDenyPolicyLockout = errors.New("this deny policy would prevent every owner of the organization from managing deny policies")
)

const (
	maxDenyPolicies      = 50
	maxDenyPermissions   = 100
	maxDenyPrincipals    = 100
	maxDenyNameRunes     = 100
	maxDenyDescription   = 500
	denyLockNamespaceSQL = `SELECT pg_advisory_xact_lock(hashtextextended('iam_deny_policies:' || ?, 0))`
)

// DenyPolicyInput : saisie d'une politique (création ou remplacement).
type DenyPolicyInput struct {
	DisplayName string `json:"display_name" description:"Nom affiché (100 caractères au plus)"`
	Description string `json:"description" description:"Pourquoi cette politique existe"`
	// Permissions exactes, ou motifs "service.*", "service.ressource.*", "*".
	DeniedPermissions []string `json:"denied_permissions" description:"Permissions refusées : exactes, ou motifs service.* et service.ressource.* ; * pour toutes"`
	// Principals : user:<id|e-mail>, serviceAccount:<id>, group:<id>, allUsers.
	DeniedPrincipals    []Principal `json:"denied_principals" description:"À qui : user:<id ou e-mail>, serviceAccount:<id>, group:<id>, allUsers"`
	ExceptionPrincipals []Principal `json:"exception_principals" description:"Jamais refusés par cette politique (même format)"`
	Condition           *Condition  `json:"condition,omitempty"`
}

// DenyPolicyService : lecture et écriture des politiques de refus.
type DenyPolicyService struct {
	DB     *gorm.DB
	Engine *Engine
	now    func() time.Time
}

func NewDenyPolicyService(db *gorm.DB, engine *Engine) *DenyPolicyService {
	return &DenyPolicyService{DB: db, Engine: engine, now: time.Now}
}

// denyParent : une politique se pose sur une organisation ou un projet.
func denyParent(r ResourceName) error {
	if id, ok := r.Organization(); ok && r == OrganizationName(id) {
		return nil
	}
	if id, ok := r.Project(); ok && r == ProjectName(id) {
		return nil
	}
	return ErrDenyPolicyParent
}

// List : politiques posées sur r (pas celles des parents).
func (s *DenyPolicyService) List(ctx context.Context, r ResourceName) ([]DenyPolicy, error) {
	if err := denyParent(r); err != nil {
		return nil, err
	}
	return (&GormStore{DB: s.DB}).DenyPoliciesOn(ctx, []ResourceName{r})
}

// Effective : politiques qui s'appliquent à r (les siennes et celles de ses parents).
func (s *DenyPolicyService) Effective(ctx context.Context, r ResourceName) ([]DenyPolicy, error) {
	if err := denyParent(r); err != nil {
		return nil, err
	}
	ancestry, err := s.Engine.Ancestry(ctx, r)
	if err != nil {
		return nil, err
	}
	return (&GormStore{DB: s.DB}).DenyPoliciesOn(ctx, ancestry)
}

func (s *DenyPolicyService) Get(ctx context.Context, r ResourceName, id uuid.UUID) (*DenyPolicy, error) {
	return getDenyPolicy(s.DB.WithContext(ctx), r, id)
}

func getDenyPolicy(db *gorm.DB, r ResourceName, id uuid.UUID) (*DenyPolicy, error) {
	var rows []denyPolicyRow
	if err := db.Where("id = ? AND resource_name = ?", id, string(r)).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrDenyPolicyNotFound
	}
	d := rows[0].policy()
	return &d, nil
}

// Create : nouvelle politique sur r.
func (s *DenyPolicyService) Create(ctx context.Context, caller Principal, r ResourceName, in DenyPolicyInput) (*DenyPolicy, error) {
	return s.write(ctx, caller, r, nil, "", &in)
}

// Update : remplace une politique (etag de la lecture).
func (s *DenyPolicyService) Update(ctx context.Context, caller Principal, r ResourceName, id uuid.UUID, etag string, in DenyPolicyInput) (*DenyPolicy, error) {
	if etag == "" {
		return nil, ErrEtagRequired
	}
	return s.write(ctx, caller, r, &id, etag, &in)
}

// Delete : retire une politique (aucun verrouillage possible : un refus en
// moins ne retire de droit à personne).
func (s *DenyPolicyService) Delete(ctx context.Context, caller Principal, r ResourceName, id uuid.UUID) error {
	if err := denyParent(r); err != nil {
		return err
	}
	res := s.DB.WithContext(ctx).Where("id = ? AND resource_name = ?", id, string(r)).Delete(&denyPolicyRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrDenyPolicyNotFound
	}
	audit.Annotate(ctx, "deny_policy", id.String())
	notifyChanged(AllUsers)
	return nil
}

// write : création (id nil) ou remplacement, sous verrou de la ressource,
// avec le garde-fou contre le verrouillage de l'organisation.
func (s *DenyPolicyService) write(ctx context.Context, caller Principal, r ResourceName, id *uuid.UUID, etag string, in *DenyPolicyInput) (*DenyPolicy, error) {
	if err := denyParent(r); err != nil {
		return nil, err
	}
	ancestry, err := s.Engine.Ancestry(ctx, r)
	if err != nil {
		return nil, err
	}
	org, ok := orgOf(ancestry)
	if !ok {
		return nil, ErrDenyPolicyParent
	}
	next, err := s.normalize(ctx, org, in)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	newEtag, err := newEtag()
	if err != nil {
		return nil, err
	}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Un seul écrivain par organisation : le garde-fou voit l'état final.
		if err := tx.Exec(denyLockNamespaceSQL, string(OrganizationName(org))).Error; err != nil {
			return err
		}
		if id != nil {
			cur, err := getDenyPolicy(tx, r, *id)
			if err != nil {
				return err
			}
			if cur.Etag != etag {
				return ErrDenyPolicyEtag
			}
			next.ID, next.CreatedBy, next.CreatedAt = cur.ID, cur.CreatedBy, cur.CreatedAt
		} else {
			var n int64
			if err := tx.Model(&denyPolicyRow{}).Where("resource_name = ?", string(r)).Count(&n).Error; err != nil {
				return err
			}
			if n >= maxDenyPolicies {
				return ErrDenyPolicyLimit
			}
			next.ID, next.CreatedBy, next.CreatedAt = uuid.New(), string(caller), now
		}
		next.Resource, next.Etag, next.UpdatedBy, next.UpdatedAt = r, newEtag, string(caller), now
		all, err := denyPoliciesOfOrg(ctx, tx, org)
		if err != nil {
			return err
		}
		after := []DenyPolicy{*next}
		for _, d := range all {
			if d.ID != next.ID {
				after = append(after, d)
			}
		}
		if err := s.checkNoLockout(ctx, tx, org, after, now); err != nil {
			return err
		}
		row, err := next.row()
		if err != nil {
			return err
		}
		if id != nil {
			return tx.Model(&denyPolicyRow{}).Where("id = ?", next.ID).Select("*").Updates(&row).Error
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return nil, err
	}
	audit.Annotate(ctx, "deny_policy", next.ID.String())
	audit.Annotate(ctx, "denied_permissions", strings.Join(next.DeniedPermissions, ","))
	notifyChanged(AllUsers)
	return next, nil
}

// denyPoliciesOfOrg : politiques de l'organisation et de tous ses projets
// (le garde-fou juge leur combinaison).
func denyPoliciesOfOrg(ctx context.Context, db *gorm.DB, org uuid.UUID) ([]DenyPolicy, error) {
	var rows []denyPolicyRow
	err := db.WithContext(ctx).Where(`resource_name = ? OR resource_name IN
		(SELECT 'projects/' || id::text FROM projects WHERE organization_id = ? AND deleted_at IS NULL)`,
		string(OrganizationName(org)), org).Order("created_at, id").Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]DenyPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.policy())
	}
	return out, nil
}

func orgOf(ancestry []ResourceName) (uuid.UUID, bool) {
	for _, a := range ancestry {
		if id, ok := a.Organization(); ok && a == OrganizationName(id) {
			return id, true
		}
	}
	return uuid.Nil, false
}

// normalize : saisie validée ; membres résolus (e-mail → id), groupes de
// l'organisation seulement.
func (s *DenyPolicyService) normalize(ctx context.Context, org uuid.UUID, in *DenyPolicyInput) (*DenyPolicy, error) {
	name := strings.TrimSpace(in.DisplayName)
	if name == "" || utf8.RuneCountInString(name) > maxDenyNameRunes {
		return nil, ErrDenyPolicyName
	}
	desc := strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(desc) > maxDenyDescription {
		desc = string([]rune(desc)[:maxDenyDescription])
	}
	if len(in.DeniedPermissions) == 0 || len(in.DeniedPermissions) > maxDenyPermissions {
		return nil, ErrDenyPolicyPerms
	}
	perms := make([]string, 0, len(in.DeniedPermissions))
	seenPerm := map[string]bool{}
	for _, p := range in.DeniedPermissions {
		p = strings.TrimSpace(p)
		if err := ValidateDenyPermission(p); err != nil {
			return nil, err
		}
		if !seenPerm[p] {
			seenPerm[p] = true
			perms = append(perms, p)
		}
	}
	if len(in.DeniedPrincipals) == 0 || len(in.DeniedPrincipals) > maxDenyPrincipals || len(in.ExceptionPrincipals) > maxDenyPrincipals {
		return nil, ErrDenyPolicyPrincipals
	}
	denied, err := s.members(ctx, org, in.DeniedPrincipals)
	if err != nil {
		return nil, err
	}
	except, err := s.members(ctx, org, in.ExceptionPrincipals)
	if err != nil {
		return nil, err
	}
	if err := ValidateCondition(in.Condition); err != nil {
		return nil, err
	}
	return &DenyPolicy{DisplayName: name, Description: desc, DeniedPermissions: perms,
		DeniedPrincipals: denied, ExceptionPrincipals: except, Condition: in.Condition}, nil
}

func (s *DenyPolicyService) members(ctx context.Context, org uuid.UUID, in []Principal) ([]Principal, error) {
	out := make([]Principal, 0, len(in))
	seen := map[Principal]bool{}
	for _, m := range in {
		p, err := resolveMember(ctx, s.DB, Principal(strings.TrimSpace(string(m))))
		if err != nil {
			return nil, err
		}
		if kind, id, _ := p.Parse(); kind == KindGroup {
			g, err := groupOrganization(ctx, s.DB, id)
			if err != nil {
				return nil, err
			}
			if g != org {
				return nil, fmt.Errorf("%w: %s", ErrGroupOutOfScope, p)
			}
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// checkNoLockout : au moins un propriétaire de l'organisation (personne ayant
// roles/owner sur elle, directement ou par un groupe) garde le droit de gérer
// les politiques de refus, sur l'organisation et sur chaque projet concerné.
func (s *DenyPolicyService) checkNoLockout(ctx context.Context, tx *gorm.DB, org uuid.UUID, policies []DenyPolicy, now time.Time) error {
	var owners []string
	if err := tx.WithContext(ctx).Raw(`
		SELECT b.principal FROM iam_bindings b
		WHERE b.resource_name = ? AND b.role = ? AND b.principal LIKE 'user:%' AND b.condition IS NULL
		UNION
		SELECT m.principal FROM iam_bindings b
		JOIN iam_group_members m ON b.principal = 'group:' || m.group_id::text
		JOIN iam_groups g ON g.id = m.group_id AND g.deleted_at IS NULL
		WHERE b.resource_name = ? AND b.role = ? AND m.principal LIKE 'user:%' AND b.condition IS NULL`,
		string(OrganizationName(org)), RoleOwner, string(OrganizationName(org)), RoleOwner).Scan(&owners).Error; err != nil {
		return err
	}
	if len(owners) == 0 {
		return nil // organisation sans propriétaire : rien à protéger ici
	}
	// Chaque ressource portant une politique doit rester gérable.
	targets := map[ResourceName]bool{OrganizationName(org): true}
	for _, d := range policies {
		targets[d.Resource] = true
	}
	store := &GormStore{DB: tx}
	for target := range targets {
		var applicable []DenyPolicy
		for _, d := range policies {
			if d.Resource == target || d.Resource == OrganizationName(org) {
				applicable = append(applicable, d)
			}
		}
		kept := false
		for _, o := range owners {
			p := Principal(o)
			groups, err := store.GroupsOf(ctx, p)
			if err != nil {
				return err
			}
			expanded := append([]Principal{p, AllUsers, AllAuthenticatedUsers}, groups...)
			if firstDeny(applicable, expanded, DenyPoliciesUpdate, now) == nil {
				kept = true
				break
			}
		}
		if !kept {
			return ErrDenyPolicyLockout
		}
	}
	return nil
}

func (d *DenyPolicy) row() (denyPolicyRow, error) {
	r := denyPolicyRow{ID: d.ID, ResourceName: string(d.Resource), DisplayName: d.DisplayName, Description: d.Description,
		Etag: d.Etag, CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt, UpdatedBy: d.UpdatedBy, UpdatedAt: d.UpdatedAt}
	var err error
	if r.DeniedPermissions, err = json.Marshal(d.DeniedPermissions); err != nil {
		return r, err
	}
	if r.DeniedPrincipals, err = json.Marshal(d.DeniedPrincipals); err != nil {
		return r, err
	}
	if d.ExceptionPrincipals == nil {
		d.ExceptionPrincipals = []Principal{}
	}
	if r.ExceptionPrincipals, err = json.Marshal(d.ExceptionPrincipals); err != nil {
		return r, err
	}
	if d.Condition != nil {
		if r.Condition, err = json.Marshal(d.Condition); err != nil {
			return r, err
		}
	}
	return r, nil
}
