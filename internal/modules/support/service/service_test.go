package service

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/infrastructure/mail"
	"github.com/thekrauss/kubemanager/internal/infrastructure/objectstore"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

// memStore : Store en mémoire (sans pagination ni tri, inutiles ici).
type memStore struct {
	tickets  map[uuid.UUID]domain.Ticket
	messages []domain.Message
	events   []domain.Event
	atts     []domain.Attachment
	projects map[uuid.UUID]uuid.UUID
	contacts map[uuid.UUID]Contact
}

func newMem() *memStore {
	return &memStore{tickets: map[uuid.UUID]domain.Ticket{}, projects: map[uuid.UUID]uuid.UUID{}, contacts: map[uuid.UUID]Contact{}}
}

func (m *memStore) CreateTicket(_ context.Context, t *domain.Ticket, ev domain.Event) error {
	m.tickets[t.ID] = *t
	m.events = append(m.events, ev)
	return nil
}
func (m *memStore) SaveTicket(_ context.Context, t *domain.Ticket, msg *domain.Message, evs []domain.Event) error {
	m.tickets[t.ID] = *t
	if msg != nil {
		m.messages = append(m.messages, *msg)
	}
	m.events = append(m.events, evs...)
	return nil
}
func (m *memStore) Ticket(_ context.Context, id uuid.UUID) (*domain.Ticket, error) {
	t, ok := m.tickets[id]
	if !ok {
		return nil, domain.ErrTicketNotFound
	}
	return &t, nil
}
func (m *memStore) ListTickets(_ context.Context, f domain.Filter) ([]domain.Ticket, int64, error) {
	var out []domain.Ticket
	for _, t := range m.tickets {
		if f.OrganizationID != nil && t.OrganizationID != *f.OrganizationID {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		out = append(out, t)
	}
	return out, int64(len(out)), nil
}
func (m *memStore) Messages(_ context.Context, id uuid.UUID) ([]domain.Message, error) {
	var out []domain.Message
	for _, x := range m.messages {
		if x.TicketID == id {
			out = append(out, x)
		}
	}
	return out, nil
}
func (m *memStore) Events(_ context.Context, id uuid.UUID) ([]domain.Event, error) {
	var out []domain.Event
	for _, x := range m.events {
		if x.TicketID == id {
			out = append(out, x)
		}
	}
	return out, nil
}
func (m *memStore) Attachments(_ context.Context, id uuid.UUID) ([]domain.Attachment, error) {
	var out []domain.Attachment
	for _, x := range m.atts {
		if x.TicketID == id {
			out = append(out, x)
		}
	}
	return out, nil
}
func (m *memStore) AddAttachment(_ context.Context, a *domain.Attachment, max int) error {
	n := 0
	for _, x := range m.atts {
		if x.TicketID == a.TicketID {
			n++
		}
	}
	if n >= max {
		return domain.ErrAttachmentLimit
	}
	m.atts = append(m.atts, *a)
	return nil
}
func (m *memStore) Attachment(_ context.Context, ticket, id uuid.UUID) (*domain.Attachment, error) {
	for _, x := range m.atts {
		if x.ID == id && x.TicketID == ticket {
			return &x, nil
		}
	}
	return nil, domain.ErrAttachmentNotFound
}
func (m *memStore) MessageOnTicket(_ context.Context, ticket, id uuid.UUID) (bool, error) {
	for _, x := range m.messages {
		if x.ID == id && x.TicketID == ticket {
			return true, nil
		}
	}
	return false, nil
}
func (m *memStore) ResourceInOrganization(_ context.Context, kind string, _, _ uuid.UUID) (bool, error) {
	return kind == "vps", nil
}

func (m *memStore) ProjectInOrganization(_ context.Context, p, org uuid.UUID) (bool, error) {
	return m.projects[p] == org, nil
}
func (m *memStore) MemberContacts(ctx context.Context, _ uuid.UUID, ids []uuid.UUID) ([]Contact, error) {
	return m.Contacts(ctx, ids)
}

func (m *memStore) Contacts(_ context.Context, ids []uuid.UUID) ([]Contact, error) {
	var out []Contact
	for _, id := range ids {
		if c, ok := m.contacts[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}
func (m *memStore) UserExists(_ context.Context, id uuid.UUID) (bool, error) {
	_, ok := m.contacts[id]
	return ok, nil
}
func (m *memStore) Summary(context.Context) (domain.Summary, error)  { return domain.Summary{}, nil }
func (m *memStore) Macros(context.Context) ([]domain.Macro, error)   { return nil, nil }
func (m *memStore) CreateMacro(context.Context, *domain.Macro) error { return nil }
func (m *memStore) UpdateMacro(context.Context, *domain.Macro) error { return domain.ErrMacroNotFound }
func (m *memStore) DeleteMacro(context.Context, uuid.UUID) error     { return domain.ErrMacroNotFound }

type memFiles struct{ objects map[string][]byte }

func (f *memFiles) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	f.objects[key] = b
	return err
}

func (f *memFiles) Delete(_ context.Context, key string) error {
	delete(f.objects, key)
	return nil
}

type readSeekNopCloser struct{ *bytes.Reader }

func (readSeekNopCloser) Close() error { return nil }

func (f *memFiles) Get(_ context.Context, key string) (objectstore.Object, objectstore.ObjectInfo, error) {
	b, ok := f.objects[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return readSeekNopCloser{bytes.NewReader(b)}, objectstore.ObjectInfo{Size: int64(len(b))}, nil
}

type outbox struct {
	mu  sync.Mutex
	got []mail.Message
}

func (o *outbox) Send(_ context.Context, m mail.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, m)
	return nil
}

func fixture() (*Service, *memStore, *outbox, uuid.UUID, Actor, Actor) {
	store := newMem()
	box := &outbox{}
	s := &Service{Store: store, Files: &memFiles{objects: map[string][]byte{}},
		Notify: &MailNotifier{Store: store, Mail: box, ConsoleURL: "https://console.test/", Sync: true}}
	org := uuid.New()
	client, staff := uuid.New(), uuid.New()
	store.contacts[client] = Contact{ID: client, Email: "awa@ex.sn", FullName: "Awa", Locale: "fr"}
	store.contacts[staff] = Contact{ID: staff, Email: "agent@betorun.test", Locale: "en"}
	return s, store, box, org, ActorOf("user:" + client.String()), ActorOf("user:" + staff.String())
}

func TestCreate_ValidatesAndRecords(t *testing.T) {
	s, store, box, org, client, _ := fixture()
	ctx := context.Background()

	_, err := s.Create(ctx, org, client, CreateInput{Subject: "ab", Description: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalidSubject)
	_, err = s.Create(ctx, org, client, CreateInput{Subject: "Port 25", Description: "x", Category: "LOGIN"})
	assert.ErrorIs(t, err, domain.ErrInvalidCategory)
	_, err = s.Create(ctx, org, client, CreateInput{Subject: "Port 25", Description: "x", ResourceKind: "vps"})
	assert.ErrorIs(t, err, domain.ErrInvalidResource)
	other := uuid.New()
	store.projects[other] = uuid.New()
	_, err = s.Create(ctx, org, client, CreateInput{Subject: "Port 25", Description: "x", ProjectID: &other})
	assert.ErrorIs(t, err, domain.ErrProjectNotInOrg, "projet d'une autre organisation")

	vps := uuid.New()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: " Ouvrir le port 25 ", Description: "Pour mon serveur mail.",
		Category: "request", Priority: "high", Tags: []string{"SMTP"}, ResourceKind: "VPS", ResourceID: &vps})
	require.NoError(t, err)
	tk := v.Ticket
	assert.Equal(t, "Ouvrir le port 25", tk.Subject)
	assert.Equal(t, domain.StatusOpen, tk.Status)
	assert.Equal(t, domain.PriorityHigh, tk.Priority)
	assert.Equal(t, domain.CategoryRequest, tk.Category)
	assert.Equal(t, "vps", tk.ResourceKind)
	assert.Equal(t, []string{"smtp"}, tk.Tags)
	assert.Equal(t, "awa@ex.sn", tk.CreatedByEmail)
	assert.Empty(t, v.Messages, "la description n'est pas un message")
	require.Len(t, v.Events, 1)
	assert.Equal(t, domain.EventTicketCreated, v.Events[0].Type)
	require.Len(t, box.got, 1, "accusé de réception")
	assert.Contains(t, box.got[0].Subject, "Ouvrir le port 25")
	assert.Contains(t, box.got[0].Text, "Bonjour Awa")
}

func TestFlow_CustomerAndTeam(t *testing.T) {
	s, _, box, org, client, staff := fixture()
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Facture en double", Description: "Deux débits ce mois-ci."})
	require.NoError(t, err)
	id := v.Ticket.ID
	box.got = nil

	// Une autre organisation ne voit pas le ticket.
	_, err = s.Get(ctx, uuid.New(), id)
	assert.ErrorIs(t, err, domain.ErrTicketNotFound)

	v, err = s.AdminReply(ctx, id, staff, "Nous regardons, pouvez-vous envoyer la facture ?")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusWaitingCustomer, v.Ticket.Status)
	assert.Equal(t, domain.AuthorSupportAgent, v.Messages[0].AuthorRole)
	require.Len(t, box.got, 1, "le client est prévenu")
	assert.Equal(t, "awa@ex.sn", box.got[0].To)
	assert.Contains(t, box.got[0].HTML, "pouvez-vous envoyer la facture")

	v, err = s.Reply(ctx, org, id, client, "La voici.")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusInProgress, v.Ticket.Status, "réponse du client : à traiter")

	v, err = s.Resolve(ctx, org, id, client)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusResolved, v.Ticket.Status)
	assert.NotNil(t, v.Ticket.ClosedAt)

	v, err = s.Reopen(ctx, org, id, client)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusOpen, v.Ticket.Status)
	assert.Nil(t, v.Ticket.ClosedAt)

	box.got = nil
	closed := string(domain.StatusClosed)
	urgent := "URGENT"
	v, err = s.AdminUpdate(ctx, id, staff, UpdateInput{Status: &closed, Priority: &urgent, Tags: []string{"doublon"}})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusClosed, v.Ticket.Status)
	assert.Equal(t, domain.PriorityUrgent, v.Ticket.Priority)
	assert.Len(t, box.got, 1, "demande traitée")
	types := map[domain.EventType]int{}
	for _, e := range v.Events {
		types[e.Type]++
	}
	assert.Equal(t, 1, types[domain.EventPriorityChanged])
	assert.Equal(t, 1, types[domain.EventTagAdded])

	_, err = s.Reply(ctx, org, id, client, "Encore moi")
	assert.ErrorIs(t, err, domain.ErrTicketClosed)
	_, err = s.Reopen(ctx, org, id, client)
	assert.ErrorIs(t, err, domain.ErrTicketClosed)
}

func TestAssign(t *testing.T) {
	s, _, _, org, client, staff := fixture()
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Abus signalé", Description: "Spam depuis 1.2.3.4"})
	require.NoError(t, err)
	unknown := uuid.New()
	_, err = s.Assign(ctx, v.Ticket.ID, staff, &unknown)
	assert.ErrorIs(t, err, domain.ErrInvalidAssignee)
	v, err = s.Assign(ctx, v.Ticket.ID, staff, staff.UserID)
	require.NoError(t, err)
	assert.Equal(t, staff.UserID, v.Ticket.AssignedToUserID)
	last := v.Events[len(v.Events)-1]
	assert.Equal(t, domain.EventAssigned, last.Type)
	assert.Equal(t, staff.UserID.String(), last.Payload["assigned_to"])
}

func TestAttach(t *testing.T) {
	s, _, _, org, client, _ := fixture()
	ctx := context.Background()
	v, err := s.Create(ctx, org, client, CreateInput{Subject: "Capture d'erreur", Description: "Voir le fichier"})
	require.NoError(t, err)
	id := v.Ticket.ID

	html := "<html><body>x</body></html>"
	_, err = s.Attach(ctx, org, id, client, AttachInput{FileName: "a.pdf", Size: int64(len(html)), Content: strings.NewReader(html)})
	assert.ErrorIs(t, err, domain.ErrAttachmentType, "type reconnu au contenu, pas à l'extension")
	_, err = s.Attach(ctx, org, id, client, AttachInput{FileName: "a.pdf", Size: domain.AttachmentMaxSize + 1, Content: strings.NewReader("")})
	assert.ErrorIs(t, err, domain.ErrAttachmentTooLarge)

	body := "journal d'erreurs"
	v, err = s.Attach(ctx, org, id, client, AttachInput{FileName: "../erreurs.txt", Size: int64(len(body)), Content: strings.NewReader(body)})
	require.NoError(t, err)
	require.Len(t, v.Attachments, 1)
	a := v.Attachments[0]
	assert.Equal(t, "erreurs.txt", a.FileName)
	assert.Equal(t, "text/plain", a.MimeType)
	assert.Equal(t, AttachmentPath(org, id, a.ID), a.URL)

	_, _, err = s.OpenAttachment(ctx, func() *uuid.UUID { o := uuid.New(); return &o }(), id, a.ID)
	assert.ErrorIs(t, err, domain.ErrTicketNotFound, "autre organisation")
	obj, got, err := s.OpenAttachment(ctx, &org, id, a.ID)
	require.NoError(t, err)
	b, _ := io.ReadAll(obj)
	assert.Equal(t, body, string(b))
	assert.Equal(t, a.ID, got.ID)

	s.Files = nil
	_, err = s.Attach(ctx, org, id, client, AttachInput{FileName: "x.txt", Size: 1, Content: strings.NewReader("x")})
	assert.ErrorIs(t, err, domain.ErrAttachmentsUnavailable)
}

func TestListInput(t *testing.T) {
	_, err := ListInput{Status: "pending"}.filter()
	assert.ErrorIs(t, err, domain.ErrInvalidStatus)
	f, err := ListInput{Limit: 1000, Priority: "low"}.filter()
	require.NoError(t, err)
	assert.Equal(t, domain.MaxLimit, f.Limit)
	assert.Equal(t, domain.PriorityLow, f.Priority)
}

func TestNotifier_EnglishRecipient(t *testing.T) {
	_, store, box, org, _, staff := fixture()
	n := &MailNotifier{Store: store, Mail: box, ConsoleURL: "https://console.test", Sync: true}
	tk := &domain.Ticket{ID: uuid.New(), OrganizationID: org, Subject: "Port 25", CreatedByUserID: staff.UserID}
	n.Resolved(context.Background(), tk)
	require.Len(t, box.got, 1)
	assert.Equal(t, `Request handled: "Port 25"`, box.got[0].Subject)
	assert.Contains(t, box.got[0].Text, "https://console.test/support/"+tk.ID.String())
}
