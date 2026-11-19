package iam

import (
	"context"
	"sort"

	"gorm.io/gorm"
)

// Avis aux personnes dont les accès changent (e-mail « vous avez accès à… »).
// Appelés après l'enregistrement, jamais dans la transaction.

// MemberChange : rôles d'un membre sur une ressource, avant et après.
type MemberChange struct {
	Principal Principal
	Before    []string
	After     []string
}

// MembershipNotifier : reçoit les changements d'accès des personnes.
type MembershipNotifier interface {
	AccessChanged(ctx context.Context, caller Principal, r ResourceName, c MemberChange)
	GroupMembershipChanged(ctx context.Context, caller Principal, g *Group, member Principal, added bool)
}

// rolesByUser : rôles de chaque personne (user:…) dans une liste de bindings.
func rolesByUser(bindings []Binding) map[Principal][]string {
	out := map[Principal][]string{}
	for _, b := range bindings {
		if kind, _, err := b.Principal.Parse(); err != nil || kind != KindUser {
			continue
		}
		out[b.Principal] = appendUnique(out[b.Principal], b.Role)
	}
	for p := range out {
		sort.Strings(out[p])
	}
	return out
}

func policyBindings(p *Policy, r ResourceName) []Binding {
	var out []Binding
	for _, pb := range p.Bindings {
		for _, m := range pb.Members {
			out = append(out, Binding{Resource: r, Role: pb.Role, Principal: m})
		}
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// memberChanges : personnes dont l'ensemble des rôles change (hors l'appelant,
// qui sait ce qu'il fait).
func memberChanges(caller Principal, before, after []Binding) []MemberChange {
	b, a := rolesByUser(before), rolesByUser(after)
	seen := map[Principal]bool{}
	var out []MemberChange
	for _, set := range []map[Principal][]string{b, a} {
		for p := range set {
			if seen[p] || p == caller {
				continue
			}
			seen[p] = true
			if !sameRoles(b[p], a[p]) {
				out = append(out, MemberChange{Principal: p, Before: b[p], After: a[p]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Principal < out[j].Principal })
	return out
}

func sameRoles(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// RoleTitle : titre d'un rôle prédéfini ou personnalisé ; le nom à défaut.
func RoleTitle(ctx context.Context, db *gorm.DB, name string) string {
	if r, ok, err := resolveRole(ctx, db, name); err == nil && ok && r.Title != "" {
		return r.Title
	}
	return name
}
