package repository_test

// Parcours complet du support sur un vrai Postgres. Ignoré sans IAM_E2E :
//
//	bash scripts/iam_e2e_db.sh
//	IAM_E2E=1 go test ./internal/modules/support/... -count=1

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
	"github.com/thekrauss/kubemanager/internal/modules/support/repository"
	"github.com/thekrauss/kubemanager/internal/modules/support/service"
)

// e2eDSN : base des tests (IAM_E2E_DSN pour en viser une autre).
var e2eDSN = func() string {
	if d := os.Getenv("IAM_E2E_DSN"); d != "" {
		return d
	}
	return "postgres://postgres:pw@127.0.0.1:55433/postgres?sslmode=disable"
}()

type files map[string][]byte

func (f files) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	f[key] = b
	return err
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func (f files) Get(_ context.Context, key string) (objectstore.Object, objectstore.ObjectInfo, error) {
	b, ok := f[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return nopCloser{bytes.NewReader(b)}, objectstore.ObjectInfo{Size: int64(len(b))}, nil
}

type outbox struct{ got []mail.Message }

func (o *outbox) Send(_ context.Context, m mail.Message) error {
	o.got = append(o.got, m)
	return nil
}

func TestE2E_SupportFlow(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	store := &repository.GormStore{DB: db}
	box := &outbox{}
	clock := time.Date(2030, 3, 1, 9, 0, 0, 0, time.UTC)
	s := &service.Service{Store: store, Files: files{}, Now: func() time.Time { clock = clock.Add(time.Minute); return clock },
		Notify: &service.MailNotifier{Store: store, Mail: box, ConsoleURL: "https://console.test", Sync: true}}

	org, other, project := uuid.New(), uuid.New(), uuid.New()
	client, colleague, agent := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e support'), (?, 'e2e autre')`, org, other).Error)
	require.NoError(t, db.Exec(`INSERT INTO projects (id, name, namespace, organization_id) VALUES (?, ?, ?, ?)`,
		project, "sup-"+project.String()[:8], "km-"+project.String()[:8], org).Error)
	for _, u := range []struct {
		id           uuid.UUID
		email, local string
	}{{client, "client-" + client.String()[:8] + "@ex.sn", "fr"}, {colleague, "coll-" + colleague.String()[:8] + "@ex.sn", "en"}, {agent, "agent-" + agent.String()[:8] + "@betorun.test", ""}} {
		require.NoError(t, db.Exec(`INSERT INTO users (id, keycloak_user_id, email, full_name, locale) VALUES (?, ?, ?, '', ?)`,
			u.id, u.id.String(), u.email, u.local).Error)
	}
	// Membres : le client directement sur l'organisation, le collègue par un
	// groupe rôlé sur le projet (les e-mails ne vont qu'aux membres actuels).
	group := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO iam_bindings (resource_name, role, principal) VALUES (?, 'roles/owner', ?)`,
		"organizations/"+org.String(), "user:"+client.String()).Error)
	require.NoError(t, db.Exec(`INSERT INTO iam_groups (id, organization_id, group_id) VALUES (?, ?, ?)`, group, org, "sup-"+group.String()[:8]).Error)
	require.NoError(t, db.Exec(`INSERT INTO iam_group_members (group_id, principal) VALUES (?, ?)`, group, "user:"+colleague.String()).Error)
	require.NoError(t, db.Exec(`INSERT INTO iam_bindings (resource_name, role, principal) VALUES (?, 'roles/developer', ?)`,
		"projects/"+project.String(), "group:"+group.String()).Error)
	me := service.ActorOf("user:" + client.String())
	mate := service.ActorOf("user:" + colleague.String())
	staff := service.ActorOf("user:" + agent.String())

	// Création : projet d'une autre organisation refusé.
	_, err = s.Create(ctx, other, me, service.CreateInput{Subject: "Mauvais projet", Description: "x", ProjectID: &project})
	assert.ErrorIs(t, err, domain.ErrProjectNotInOrg)
	vps := uuid.New()
	v, err := s.Create(ctx, org, me, service.CreateInput{Subject: "Ouvrir le port 25", Description: "Serveur mail 100% légitime",
		Category: "REQUEST", Tags: []string{"smtp"}, ProjectID: &project, ResourceKind: "vps", ResourceID: &vps})
	require.NoError(t, err)
	id := v.Ticket.ID
	assert.Equal(t, domain.PriorityMedium, v.Ticket.Priority)
	assert.Equal(t, []string{"smtp"}, v.Ticket.Tags)
	require.Len(t, box.got, 1, "accusé de réception")

	// Un second ticket, ailleurs, pour les listes.
	_, err = s.Create(ctx, other, me, service.CreateInput{Subject: "Facture", Description: "Question", Priority: "URGENT"})
	require.NoError(t, err)

	// Côté client : seulement son organisation ; recherche (le % est cherché tel quel).
	list, err := s.List(ctx, org, service.ListInput{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, list.Total)
	list, err = s.List(ctx, org, service.ListInput{Search: "100%"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, list.Total)
	list, err = s.List(ctx, org, service.ListInput{Search: "10%0"})
	require.NoError(t, err)
	assert.EqualValues(t, 0, list.Total)
	_, err = s.Get(ctx, other, id)
	assert.ErrorIs(t, err, domain.ErrTicketNotFound)

	// L'équipe répond : en attente du client, e-mail au créateur.
	box.got = nil
	v, err = s.AdminReply(ctx, id, staff, "Quel usage prévoyez-vous ?")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusWaitingCustomer, v.Ticket.Status)
	require.Len(t, box.got, 1)
	assert.Equal(t, "client-"+client.String()[:8]+"@ex.sn", box.got[0].To)

	// Un collègue répond : à traiter ; la réponse suivante de l'équipe prévient les deux.
	v, err = s.Reply(ctx, org, id, mate, "Envoi de factures à nos clients.")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusInProgress, v.Ticket.Status)
	box.got = nil
	_, err = s.AdminReply(ctx, id, staff, "Port 25 ouvert.")
	require.NoError(t, err)
	require.Len(t, box.got, 2)
	subjects := box.got[0].Subject + " | " + box.got[1].Subject
	assert.Contains(t, subjects, "Reply to your request", "collègue en anglais")
	assert.Contains(t, subjects, "Réponse à votre demande")

	// Le client voit tout le fil, dans l'ordre.
	v, err = s.Get(ctx, org, id)
	require.NoError(t, err)
	require.Len(t, v.Messages, 3)
	assert.Equal(t, domain.AuthorSupportAgent, v.Messages[0].AuthorRole)
	assert.Equal(t, domain.AuthorCustomer, v.Messages[1].AuthorRole)
	assert.Equal(t, &colleague, v.Messages[1].AuthorUserID)

	// Pièce jointe rattachée à un message.
	body := "%PDF-1.4\nfacture"
	msgID := v.Messages[1].ID
	v, err = s.Attach(ctx, org, id, mate, service.AttachInput{FileName: "facture.pdf", Size: int64(len(body)), Content: strings.NewReader(body), MessageID: &msgID})
	require.NoError(t, err)
	require.Len(t, v.Attachments, 1)
	assert.Equal(t, "application/pdf", v.Attachments[0].MimeType)
	obj, _, err := s.OpenAttachment(ctx, nil, id, v.Attachments[0].ID)
	require.NoError(t, err)
	got, _ := io.ReadAll(obj)
	assert.Equal(t, body, string(got))
	bad := uuid.New()
	_, err = s.Attach(ctx, org, id, mate, service.AttachInput{FileName: "x.txt", Size: 1, Content: strings.NewReader("x"), MessageID: &bad})
	assert.ErrorIs(t, err, domain.ErrMessageNotOnTicket)

	// Équipe : priorité, étiquettes, assignation ; journal.
	high := "HIGH"
	v, err = s.AdminUpdate(ctx, id, staff, service.UpdateInput{Priority: &high, Tags: []string{"port-25"}})
	require.NoError(t, err)
	v, err = s.Assign(ctx, id, staff, &agent)
	require.NoError(t, err)
	assert.Equal(t, &agent, v.Ticket.AssignedToUserID)
	counts := map[domain.EventType]int{}
	for _, e := range v.Events {
		counts[e.Type]++
	}
	assert.Equal(t, 1, counts[domain.EventTicketCreated])
	assert.Equal(t, 3, counts[domain.EventCommentAdded])
	assert.Equal(t, 1, counts[domain.EventPriorityChanged])
	assert.Equal(t, 1, counts[domain.EventTagAdded])
	assert.Equal(t, 1, counts[domain.EventTagRemoved])
	assert.Equal(t, 1, counts[domain.EventAssigned])
	assert.Equal(t, 3, counts[domain.EventStatusChanged])

	// File de l'équipe : à traiter d'abord, filtre par organisation.
	all, err := s.AdminList(ctx, service.AdminListInput{OrganizationID: org.String()})
	require.NoError(t, err)
	assert.EqualValues(t, 1, all.Total)
	all, err = s.AdminList(ctx, service.AdminListInput{ListInput: service.ListInput{Priority: "URGENT", Limit: 100}})
	require.NoError(t, err)
	require.NotEmpty(t, all.Items)
	for _, it := range all.Items {
		assert.Equal(t, domain.PriorityUrgent, it.Priority)
	}
	sum, err := s.Summary(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, sum.WaitingCustomer, int64(1))
	assert.GreaterOrEqual(t, sum.HighPriority, int64(1), "le ticket urgent de l'autre organisation")

	// Résolu par le client, rouvert, puis fermé par l'équipe : définitif.
	v, err = s.Resolve(ctx, org, id, me)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusResolved, v.Ticket.Status)
	require.NotNil(t, v.Ticket.ClosedAt)
	v, err = s.Reopen(ctx, org, id, me)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOpen, v.Ticket.Status)
	assert.Nil(t, v.Ticket.ClosedAt)
	box.got = nil
	closed := "CLOSED"
	v, err = s.AdminUpdate(ctx, id, staff, service.UpdateInput{Status: &closed})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusClosed, v.Ticket.Status)
	assert.Len(t, box.got, 1, "demande traitée")
	_, err = s.Reopen(ctx, org, id, me)
	assert.ErrorIs(t, err, domain.ErrTicketClosed)
	_, err = s.Reply(ctx, org, id, me, "Merci")
	assert.ErrorIs(t, err, domain.ErrTicketClosed)

	// Relu depuis la base : tout est là.
	reread, err := store.Ticket(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusClosed, reread.Status)
	assert.Equal(t, domain.PriorityHigh, reread.Priority)
	assert.Equal(t, []string{"port-25"}, reread.Tags)
	assert.Equal(t, &project, reread.ProjectID)
	assert.Equal(t, &vps, reread.ResourceID)
	assert.Equal(t, "user:"+client.String(), reread.CreatedBy)

	// Réponses types.
	m, err := s.CreateMacro(ctx, service.MacroInput{Title: "Port 25", Body: "Le port 25 est ouvert.", Category: "REQUEST"})
	require.NoError(t, err)
	m, err = s.UpdateMacro(ctx, m.ID, service.MacroInput{Title: "Port 25 ouvert", Body: "C'est fait."})
	require.NoError(t, err)
	assert.Equal(t, "Port 25 ouvert", m.Title)
	assert.Empty(t, m.Category)
	macros, err := s.Macros(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, macros)
	require.NoError(t, s.DeleteMacro(ctx, m.ID))
	assert.ErrorIs(t, s.DeleteMacro(ctx, m.ID), domain.ErrMacroNotFound)
	_, err = s.UpdateMacro(ctx, m.ID, service.MacroInput{Title: "x", Body: "y"})
	assert.ErrorIs(t, err, domain.ErrMacroNotFound)
}

// Une personne retirée de l'organisation ne reçoit plus le contenu des tickets.
func TestE2E_SupportMemberContacts(t *testing.T) {
	if os.Getenv("IAM_E2E") == "" {
		t.Skip("IAM_E2E not set")
	}
	db, err := gorm.Open(postgres.Open(e2eDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()
	store := &repository.GormStore{DB: db}
	org, member, gone := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO organizations (id, display_name) VALUES (?, 'e2e membres')`, org).Error)
	for _, u := range []uuid.UUID{member, gone} {
		require.NoError(t, db.Exec(`INSERT INTO users (id, keycloak_user_id, email, full_name, locale) VALUES (?, ?, ?, '', 'fr')`,
			u, u.String(), "m-"+u.String()[:8]+"@ex.sn").Error)
	}
	require.NoError(t, db.Exec(`INSERT INTO iam_bindings (resource_name, role, principal) VALUES (?, 'roles/viewer', ?)`,
		"organizations/"+org.String(), "user:"+member.String()).Error)
	cs, err := store.MemberContacts(ctx, org, []uuid.UUID{member, gone})
	require.NoError(t, err)
	require.Len(t, cs, 1)
	assert.Equal(t, member, cs[0].ID)
}
