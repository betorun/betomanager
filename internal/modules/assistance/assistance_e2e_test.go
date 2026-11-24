package assistance_test

// Accès d'assistance sur un vrai Postgres (toutes les migrations), ignoré
// sans IAM_E2E (bash scripts/iam_e2e_db.sh).

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/audit"
	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/assistance"
)

type notices struct {
	mu   sync.Mutex
	kind []string
}

func (n *notices) AssistanceNotice(_ context.Context, _ uuid.UUID, kind, _, _ string, _ *time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.kind = append(n.kind, kind)
}

func open(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open("postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

func user(t *testing.T, db *gorm.DB, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	email = id.String()[:8] + "-" + email
	require.NoError(t, db.Exec(`INSERT INTO users (id, keycloak_user_id, email, full_name) VALUES (?, ?, ?, ?)`, id, id.String(), email, email).Error)
	return id
}

// can : décision sans cache (moteur neuf).
func can(t *testing.T, db *gorm.DB, u uuid.UUID, perm iam.Permission, r iam.ResourceName) bool {
	t.Helper()
	d, err := iam.NewEngine(&iam.GormStore{DB: db}, iam.DefaultCacheTTL).Check(context.Background(), iam.UserPrincipal(u), perm, r)
	require.NoError(t, err)
	return d.Allowed
}

func TestE2E_Assistance(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, staff, other := user(t, db, "owner@client.test"), user(t, db, "awa@betorun.test"), user(t, db, "moussa@betorun.test")
	org := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'Client')`, org).Error)
	orgRes := iam.OrganizationName(org)
	require.NoError(t, iam.AddBinding(ctx, db, iam.Binding{Resource: orgRes, Role: iam.RoleOwner, Principal: iam.UserPrincipal(owner)}, "test"))
	ticket := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO support_tickets (id, organization_id, subject, description, status, priority, assigned_to_user_id)
		VALUES (?, ?, 'Site en panne', 'x', 'open', 'normal', ?)`, ticket, org, other).Error)

	now := time.Now().UTC()
	n := &notices{}
	s := &assistance.Service{DB: db, Audit: &audit.GormWriter{DB: db}, Notify: n, Now: func() time.Time { return now }}
	staffP, ownerP := assistance.Person{ID: staff, Email: "awa@betorun.test"}, assistance.Person{ID: owner, Email: "owner@client.test"}
	reason := "Le conteneur web redémarre en boucle depuis ce matin, il faut revenir à la version précédente."

	// Aucun droit par défaut.
	require.False(t, can(t, db, staff, iam.WorkloadsRestart, orgRes))

	// Demande : rien n'est ouvert ; une seule demande à la fois.
	g, err := s.Request(ctx, staffP, org, &ticket, reason, 2)
	require.NoError(t, err)
	require.Equal(t, assistance.StatusRequested, g.Status)
	_, err = s.Request(ctx, staffP, org, &ticket, reason, 2)
	require.ErrorIs(t, err, assistance.ErrAlreadyOpen)
	require.False(t, can(t, db, staff, iam.WorkloadsRestart, orgRes))
	_, err = s.Request(ctx, staffP, org, &ticket, "trop court", 2)
	require.Error(t, err)

	// Acceptée : lecture et actions réversibles, rien d'autre.
	g, err = s.Approve(ctx, org, g.ID, ownerP, 0)
	require.NoError(t, err)
	require.Equal(t, assistance.StatusActive, g.Status)
	require.WithinDuration(t, now.Add(2*time.Hour), *g.ExpiresAt, time.Second)
	_, err = s.Approve(ctx, org, g.ID, ownerP, 0)
	require.ErrorIs(t, err, assistance.ErrNotPending)
	for _, p := range []iam.Permission{iam.WorkloadsRestart, iam.WorkloadsRollback, iam.WorkloadLogsRead, iam.SiteDeploymentsActive, iam.VPSReboot} {
		require.True(t, can(t, db, staff, p, orgRes), p)
	}
	for _, p := range []iam.Permission{iam.WorkloadsDelete, iam.WorkloadsUpdate, iam.WorkloadVariablesList, iam.BillingAccountsGet,
		iam.OrganizationsSetIamPolicy, iam.DatabasesConnect, iam.AuditLogsRead, iam.OrganizationsGrantAssistance} {
		require.False(t, can(t, db, staff, p, orgRes), p)
	}
	a, err := s.Active(ctx, staff, org)
	require.NoError(t, err)
	require.NotNil(t, a)
	active, err := s.HasActive(ctx, staff)
	require.NoError(t, err)
	require.True(t, active)
	active, err = s.HasActive(ctx, other)
	require.NoError(t, err)
	require.False(t, active)

	// Le client édite ses accès : la liaison d'assistance reste, avec son échéance.
	ps := &iam.PolicyService{DB: db, Engine: iam.NewEngine(&iam.GormStore{DB: db}, iam.DefaultCacheTTL)}
	view, err := ps.Get(ctx, orgRes)
	require.NoError(t, err)
	edited := view.Policy
	edited.Bindings = []iam.PolicyBinding{{Role: iam.RoleOwner, Members: []iam.Principal{iam.UserPrincipal(owner)}}}
	_, err = ps.Set(ctx, iam.UserPrincipal(owner), orgRes, edited)
	require.NoError(t, err)
	require.True(t, can(t, db, staff, iam.WorkloadsRestart, orgRes), "liaison d'assistance effacée par l'éditeur")
	// Et l'éditeur ne peut pas la poser lui-même (rôle réservé, ignoré).
	view, err = ps.Get(ctx, orgRes)
	require.NoError(t, err)
	edited = view.Policy
	edited.Bindings = append(withoutRole(edited.Bindings, iam.RoleSupportAssistant),
		iam.PolicyBinding{Role: iam.RoleSupportAssistant, Members: []iam.Principal{iam.UserPrincipal(other)}})
	_, err = ps.Set(ctx, iam.UserPrincipal(owner), orgRes, edited)
	require.NoError(t, err)
	require.False(t, can(t, db, other, iam.WorkloadsRestart, orgRes), "rôle réservé posé par l'éditeur")

	// Retiré : effet immédiat.
	g, err = s.Revoke(ctx, org, g.ID, ownerP, "fini")
	require.NoError(t, err)
	require.Equal(t, assistance.StatusRevoked, g.Status)
	require.False(t, can(t, db, staff, iam.WorkloadsRestart, orgRes))
	active, err = s.HasActive(ctx, staff)
	require.NoError(t, err)
	require.False(t, active)
	_, err = s.Revoke(ctx, org, g.ID, ownerP, "")
	require.ErrorIs(t, err, assistance.ErrNotActive)

	// Accordé de lui-même par le client, à l'agent du ticket ; puis échu.
	g2, err := s.GrantForTicket(ctx, org, ticket, ownerP, reason, 1)
	require.NoError(t, err)
	require.Equal(t, other, g2.StaffUserID)
	require.True(t, can(t, db, other, iam.WorkloadsRestart, orgRes))
	// La liaison porte l'échéance : le moteur cesse d'accorder à l'heure dite,
	// même si le ramasseur ne passe pas.
	var cond string
	require.NoError(t, db.Raw(`SELECT condition->>'expression' FROM iam_bindings WHERE role = ? AND principal = ?`,
		iam.RoleSupportAssistant, string(iam.UserPrincipal(other))).Scan(&cond).Error)
	require.Equal(t, "request.time < timestamp('"+g2.ExpiresAt.UTC().Format(time.RFC3339)+"')", cond)
	require.False(t, iam.ConditionHolds(&iam.Condition{Title: "assistance", Expression: cond}, g2.ExpiresAt.Add(time.Second)))
	now = now.Add(61 * time.Minute)
	require.NoError(t, s.Reap(ctx))
	var st string
	require.NoError(t, db.Raw(`SELECT status FROM assistance_grants WHERE id = ?`, g2.ID).Scan(&st).Error)
	require.Equal(t, assistance.StatusExpired, st)
	var left int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM iam_bindings WHERE role = ? AND resource_name = ?`, iam.RoleSupportAssistant, string(orgRes)).Scan(&left).Error)
	require.Zero(t, left)

	// Bris de glace : 1 h au plus, propriétaires prévenus.
	g3, err := s.BreakGlass(ctx, staffP, org, &ticket, reason, 3*time.Hour)
	require.NoError(t, err)
	require.Equal(t, assistance.ModeBreakglass, g3.Mode)
	require.WithinDuration(t, now.Add(time.Hour), *g3.ExpiresAt, time.Second)
	_, err = s.Close(ctx, assistance.Person{ID: other}, g3.ID, "")
	require.ErrorIs(t, err, assistance.ErrNotFound, "seule la personne clôt son accès")
	_, err = s.Close(ctx, staffP, g3.ID, "redémarré")
	require.NoError(t, err)
	require.False(t, can(t, db, staff, iam.WorkloadsRestart, orgRes))

	// Rapprochement : une liaison réservée sans accès actif est retirée.
	require.NoError(t, iam.AddBinding(ctx, db, iam.Binding{Resource: orgRes, Role: iam.RoleSupportAssistant, Principal: iam.UserPrincipal(other)}, "restauration"))
	require.True(t, can(t, db, other, iam.WorkloadsRestart, orgRes))
	require.NoError(t, s.Reap(ctx))
	require.False(t, can(t, db, other, iam.WorkloadsRestart, orgRes))

	// Demande sans réponse : expirée après 72 h ; refus.
	g4, err := s.Request(ctx, staffP, org, &ticket, reason, 4)
	require.NoError(t, err)
	_, err = s.Deny(ctx, org, g4.ID, ownerP, "pas maintenant")
	require.NoError(t, err)
	g5, err := s.Request(ctx, staffP, org, &ticket, reason, 4)
	require.NoError(t, err)
	now = now.Add(assistance.RequestTTL + time.Minute)
	require.NoError(t, s.Reap(ctx))
	_, err = s.Approve(ctx, org, g5.ID, ownerP, 0)
	require.ErrorIs(t, err, assistance.ErrNotPending)
	// Une autre organisation ne voit ni ne décide les demandes de celle-ci.
	_, err = s.Deny(ctx, uuid.New(), g5.ID, ownerP, "")
	require.ErrorIs(t, err, assistance.ErrNotFound)

	// Journal de l'organisation : chaque étape, lisible par le client.
	var actions []string
	require.NoError(t, db.Raw(`SELECT action FROM audit_log WHERE resource = ? AND action LIKE 'organization.assistance.%' ORDER BY id`, string(orgRes)).
		Scan(&actions).Error)
	require.Equal(t, []string{
		"organization.assistance.requested", "organization.assistance.granted", "organization.assistance.revoked",
		"organization.assistance.granted", "organization.assistance.expired",
		"organization.assistance.breakglass", "organization.assistance.closed",
		"organization.assistance.reconciled",
		"organization.assistance.requested", "organization.assistance.denied",
		"organization.assistance.requested", "organization.assistance.expired",
	}, actions)
	n.mu.Lock()
	defer n.mu.Unlock()
	require.Equal(t, []string{"requested", "granted", "granted", "breakglass", "requested", "requested"}, n.kind)
}

// Deux propriétaires acceptent la même demande en même temps : une seule passe.
func TestE2E_AssistanceConcurrentApprove(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	owner, staff := user(t, db, "owner@client.test"), user(t, db, "awa@betorun.test")
	org := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'Client')`, org).Error)
	s := &assistance.Service{DB: db}
	g, err := s.Request(ctx, assistance.Person{ID: staff}, org, nil, "Diagnostic du site, il faut lire les journaux du conteneur.", 1)
	require.NoError(t, err)
	var wins, lost int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			_, err := s.Approve(ctx, org, g.ID, assistance.Person{ID: owner}, 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, assistance.ErrNotPending):
				lost++
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	require.Equal(t, 1, wins)
	require.Equal(t, 5, lost)
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM iam_bindings WHERE role = ? AND principal = ?`, iam.RoleSupportAssistant, string(iam.UserPrincipal(staff))).Scan(&n).Error)
	require.EqualValues(t, 1, n)
}

func withoutRole(in []iam.PolicyBinding, role string) []iam.PolicyBinding {
	var out []iam.PolicyBinding
	for _, b := range in {
		if b.Role != role {
			out = append(out, b)
		}
	}
	return out
}
