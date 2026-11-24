// Package service : cas d'usage du support (docs/modules/support.md). Côté
// client, tout se fait dans une organisation ; côté équipe, sur tous les
// tickets.
package service

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	betoerrors "github.com/thekrauss/beto-shared/pkg/errors"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/iam"
	"github.com/thekrauss/kubemanager/internal/modules/support/domain"
)

// Service : tickets (clients et équipe) et réponses types.
type Service struct {
	Store Store
	// Files nil : pièces jointes indisponibles.
	Files Files
	// Notify nil : aucun e-mail.
	Notify Notifier
	// Live nil : rien en direct (la console relit le ticket à intervalle).
	Live Publisher
	// Staff nil : back-office sans contexte, agents ni délais.
	Staff StaffStore
	// Wallet nil : contexte d'un ticket sans porte-monnaie.
	Wallet func(ctx context.Context, org uuid.UUID) (*BillingContext, error)
	Logger *zap.SugaredLogger
	Now    func() time.Time
}

// Actor : auteur d'une action (principal IAM ; UserID pour une personne).
type Actor struct {
	Principal string
	UserID    *uuid.UUID
}

// ActorOf : principal "user:<uuid>" → personne ; compte de service sinon.
func ActorOf(principal string) Actor {
	a := Actor{Principal: principal}
	if kind, id, err := iam.Principal(principal).Parse(); err == nil && kind == iam.KindUser {
		a.UserID = &id
	}
	return a
}

// TicketView : réponse d'un ticket, comme le support de SYK.
type TicketView struct {
	Ticket      *domain.Ticket      `json:"ticket"`
	Messages    []domain.Message    `json:"messages"`
	Attachments []domain.Attachment `json:"attachments"`
	Events      []domain.Event      `json:"events"`
}

// TicketList : une page de tickets et le total.
type TicketList struct {
	Items []domain.Ticket `json:"items"`
	Total int64           `json:"total"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// ── Client ──────────────────────────────────────────────────────────────

// CreateInput : nouvelle demande.
type CreateInput struct {
	Subject      string     `json:"subject" binding:"required" description:"Sujet de la demande (3 à 150 caractères)"`
	Description  string     `json:"description" binding:"required" description:"Demande détaillée, texte brut (10 000 caractères au plus)"`
	Priority     string     `json:"priority,omitempty" enum:"LOW,MEDIUM,HIGH,URGENT" default:"MEDIUM" description:"Priorité proposée (l'équipe peut la changer)"`
	Category     string     `json:"category,omitempty" enum:"BILLING,TECHNICAL,ABUSE,REQUEST,BUG,FEATURE_REQUEST" description:"Catégorie (REQUEST : ouverture du port 25, déblocage d'une adresse…)"`
	Tags         []string   `json:"tags,omitempty" description:"Étiquettes libres (10 au plus)"`
	ProjectID    *uuid.UUID `json:"project_id,omitempty" description:"Projet concerné"`
	ResourceKind string     `json:"resource_kind,omitempty" description:"Type de la ressource concernée (ex: vps)"`
	ResourceID   *uuid.UUID `json:"resource_id,omitempty" description:"Identifiant de la ressource concernée"`
}

func (s *Service) Create(ctx context.Context, org uuid.UUID, by Actor, in CreateInput) (*TicketView, error) {
	subject, err := domain.CleanSubject(in.Subject)
	if err != nil {
		return nil, err
	}
	desc, err := domain.CleanBody(in.Description, domain.ErrInvalidDescription)
	if err != nil {
		return nil, err
	}
	priority := domain.PriorityMedium
	if strings.TrimSpace(in.Priority) != "" {
		if priority, err = domain.ParsePriority(in.Priority); err != nil {
			return nil, err
		}
	}
	category, err := domain.ParseCategory(in.Category)
	if err != nil {
		return nil, err
	}
	tags, err := domain.CleanTags(in.Tags)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(in.ResourceKind))
	if (kind == "") != (in.ResourceID == nil) || utf8.RuneCountInString(kind) > domain.ResourceKindMax {
		return nil, domain.ErrInvalidResource
	}
	if kind != "" {
		// Seules les ressources de l'organisation : un ticket ne doit jamais
		// pointer l'équipe vers le serveur d'un autre client.
		ok, err := s.Store.ResourceInOrganization(ctx, kind, *in.ResourceID, org)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, domain.ErrInvalidResource
		}
	}
	if in.ProjectID != nil {
		ok, err := s.Store.ProjectInOrganization(ctx, *in.ProjectID, org)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, domain.ErrProjectNotInOrg
		}
	}
	now := s.now()
	t := &domain.Ticket{ID: uuid.New(), OrganizationID: org, ProjectID: in.ProjectID, ResourceKind: kind, ResourceID: in.ResourceID,
		CreatedBy: by.Principal, CreatedByUserID: by.UserID, Subject: subject, Description: desc,
		Status: domain.StatusOpen, Priority: priority, Category: category, Tags: tags,
		LastActivityAt: now, CreatedAt: now, UpdatedAt: now}
	if by.UserID != nil {
		if cs, err := s.Store.Contacts(ctx, []uuid.UUID{*by.UserID}); err == nil && len(cs) == 1 {
			t.CreatedByEmail = cs[0].Email
		}
	}
	ev := s.event(t, domain.EventTicketCreated, by, map[string]string{"status": string(t.Status), "priority": string(t.Priority)})
	if err := s.Store.CreateTicket(ctx, t, ev); err != nil {
		return nil, err
	}
	if s.Notify != nil {
		s.Notify.Received(ctx, t)
	}
	return s.view(ctx, t)
}

// ListInput : filtres d'une liste, pagination limit/offset.
type ListInput struct {
	Status   string `query:"status" enum:"OPEN,IN_PROGRESS,WAITING_CUSTOMER,RESOLVED,CLOSED" description:"Statut"`
	Category string `query:"category" enum:"BILLING,TECHNICAL,ABUSE,REQUEST,BUG,FEATURE_REQUEST" description:"Catégorie"`
	Priority string `query:"priority" enum:"LOW,MEDIUM,HIGH,URGENT" description:"Priorité"`
	Search   string `query:"search" description:"Recherche dans le sujet, la description et l'e-mail de l'auteur"`
	Limit    int    `query:"limit" default:"20" description:"Taille de la page (100 au plus)"`
	Offset   int    `query:"offset" default:"0" description:"Décalage"`
}

func (in ListInput) filter() (domain.Filter, error) {
	var f domain.Filter
	var err error
	if strings.TrimSpace(in.Status) != "" {
		if f.Status, err = domain.ParseStatus(in.Status); err != nil {
			return f, err
		}
	}
	if f.Category, err = domain.ParseCategory(in.Category); err != nil {
		return f, err
	}
	if strings.TrimSpace(in.Priority) != "" {
		if f.Priority, err = domain.ParsePriority(in.Priority); err != nil {
			return f, err
		}
	}
	f.Search = strings.TrimSpace(in.Search)
	if utf8.RuneCountInString(f.Search) > domain.SubjectMax {
		f.Search = string([]rune(f.Search)[:domain.SubjectMax])
	}
	f.Limit, f.Offset = domain.Page(in.Limit, in.Offset)
	return f, nil
}

// List : tickets de l'organisation, activité la plus récente d'abord.
func (s *Service) List(ctx context.Context, org uuid.UUID, in ListInput) (*TicketList, error) {
	f, err := in.filter()
	if err != nil {
		return nil, err
	}
	f.OrganizationID = &org
	return s.list(ctx, f)
}

func (s *Service) list(ctx context.Context, f domain.Filter) (*TicketList, error) {
	items, total, err := s.Store.ListTickets(ctx, f)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []domain.Ticket{}
	}
	return &TicketList{Items: items, Total: total}, nil
}

// orgTicket : le ticket s'il est dans l'organisation (sinon introuvable).
func (s *Service) orgTicket(ctx context.Context, org, id uuid.UUID) (*domain.Ticket, error) {
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.OrganizationID != org {
		return nil, domain.ErrTicketNotFound
	}
	return t, nil
}

func (s *Service) Get(ctx context.Context, org, id uuid.UUID) (*TicketView, error) {
	t, err := s.orgTicket(ctx, org, id)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, t)
}

// Reply : réponse du client. Résolu : rouvert ; fermé : refusé.
func (s *Service) Reply(ctx context.Context, org, id uuid.UUID, by Actor, body string) (*TicketView, error) {
	body, err := domain.CleanBody(body, domain.ErrInvalidBody)
	if err != nil {
		return nil, err
	}
	t, err := s.orgTicket(ctx, org, id)
	if err != nil {
		return nil, err
	}
	next, err := domain.AfterCustomerMessage(t.Status)
	if err != nil {
		return nil, err
	}
	return s.addMessage(ctx, t, by, domain.AuthorCustomer, body, next)
}

// Resolve : le client marque sa demande résolue (sans effet si déjà terminée).
func (s *Service) Resolve(ctx context.Context, org, id uuid.UUID, by Actor) (*TicketView, error) {
	t, err := s.orgTicket(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if t.Status.Done() {
		return s.view(ctx, t)
	}
	return s.changeStatus(ctx, t, by, domain.StatusResolved, map[string]string{"by": "customer"})
}

// Reopen : le client relance un ticket résolu ; fermé : refusé.
func (s *Service) Reopen(ctx context.Context, org, id uuid.UUID, by Actor) (*TicketView, error) {
	t, err := s.orgTicket(ctx, org, id)
	if err != nil {
		return nil, err
	}
	next, changed, err := domain.Reopen(t.Status)
	if err != nil {
		return nil, err
	}
	if !changed {
		return s.view(ctx, t)
	}
	return s.changeStatus(ctx, t, by, next, map[string]string{"by": "customer", "action": "reopen"})
}

// AttachInput : fichier envoyé par le client (contenu lu en flux).
type AttachInput struct {
	FileName  string
	Size      int64
	Content   io.Reader
	MessageID *uuid.UUID
}

// Attach : pièce jointe du client (10 Mo, PDF, images, texte).
func (s *Service) Attach(ctx context.Context, org, id uuid.UUID, by Actor, in AttachInput) (*TicketView, error) {
	if s.Files == nil {
		return nil, domain.ErrAttachmentsUnavailable
	}
	if in.Size <= 0 || in.Size > domain.AttachmentMaxSize {
		if in.Size <= 0 {
			return nil, domain.ErrAttachmentMissing
		}
		return nil, domain.ErrAttachmentTooLarge
	}
	t, err := s.orgTicket(ctx, org, id)
	if err != nil {
		return nil, err
	}
	if t.Status == domain.StatusClosed {
		return nil, domain.ErrTicketClosed
	}
	existing, err := s.Store.Attachments(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= domain.MaxAttachments {
		return nil, domain.ErrAttachmentLimit
	}
	if in.MessageID != nil {
		ok, err := s.Store.MessageOnTicket(ctx, t.ID, *in.MessageID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, domain.ErrMessageNotOnTicket
		}
	}
	br := bufio.NewReaderSize(in.Content, 512)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	mime, err := domain.DetectMIME(head)
	if err != nil {
		return nil, err
	}
	now := s.now()
	a := &domain.Attachment{ID: uuid.New(), TicketID: t.ID, MessageID: in.MessageID, FileName: domain.CleanFileName(in.FileName),
		MimeType: mime, Size: in.Size, CreatedBy: by.Principal, CreatedAt: now, UpdatedAt: now}
	a.ObjectKey = "support/" + t.OrganizationID.String() + "/" + t.ID.String() + "/" + a.ID.String()
	if err := s.Files.Put(ctx, a.ObjectKey, io.LimitReader(br, in.Size), in.Size, mime); err != nil {
		return nil, err
	}
	if err := s.Store.AddAttachment(ctx, a, domain.MaxAttachments); err != nil {
		// Objet déposé mais non enregistré : effacé (même requête annulée).
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = s.Files.Delete(dctx, a.ObjectKey)
		cancel()
		return nil, err
	}
	s.publish(ctx, t, LiveAttachment, "")
	return s.view(ctx, t)
}

// OpenAttachment : contenu d'une pièce jointe ; org nil : équipe (tous les tickets).
func (s *Service) OpenAttachment(ctx context.Context, org *uuid.UUID, ticket, id uuid.UUID) (io.ReadSeekCloser, *domain.Attachment, error) {
	if s.Files == nil {
		return nil, nil, domain.ErrAttachmentsUnavailable
	}
	var err error
	if org != nil {
		_, err = s.orgTicket(ctx, *org, ticket)
	} else {
		_, err = s.Store.Ticket(ctx, ticket)
	}
	if err != nil {
		return nil, nil, err
	}
	a, err := s.Store.Attachment(ctx, ticket, id)
	if err != nil {
		return nil, nil, err
	}
	obj, _, err := s.Files.Get(ctx, a.ObjectKey)
	if err != nil {
		return nil, nil, err
	}
	return obj, a, nil
}

// ── Équipe ──────────────────────────────────────────────────────────────

// AdminListInput : file de l'équipe, filtrable par organisation.
type AdminListInput struct {
	ListInput
	OrganizationID string `query:"organization_id" description:"Restreindre aux tickets d'une organisation"`
	Assigned       string `query:"assigned" description:"Agent en charge : me, none ou identifiant d'un agent"`
}

// AdminList : tickets à traiter d'abord (OPEN, IN_PROGRESS), puis par
// priorité, la plus ancienne activité d'abord.
func (s *Service) AdminList(ctx context.Context, in AdminListInput, me *uuid.UUID) (*StaffTicketList, error) {
	f, err := in.filter()
	if err != nil {
		return nil, err
	}
	if raw := strings.TrimSpace(in.OrganizationID); raw != "" {
		org, err := uuid.Parse(raw)
		if err != nil {
			return nil, betoerrors.New(betoerrors.CodeInvalidInput, "invalid organization id")
		}
		f.OrganizationID = &org
	}
	if err := assignedFilter(in.Assigned, me, &f); err != nil {
		return nil, err
	}
	f.StaffOrder = true
	list, err := s.list(ctx, f)
	if err != nil {
		return nil, err
	}
	items, err := s.staffTickets(ctx, list.Items)
	if err != nil {
		return nil, err
	}
	return &StaffTicketList{Items: items, Total: list.Total}, nil
}

func (s *Service) AdminGet(ctx context.Context, id uuid.UUID) (*StaffTicketView, error) {
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.staffView(ctx, t)
}

// AdminReply : réponse de l'équipe ; le ticket attend le client, qui est prévenu.
func (s *Service) AdminReply(ctx context.Context, id uuid.UUID, by Actor, body string) (*StaffTicketView, error) {
	body, err := domain.CleanBody(body, domain.ErrInvalidBody)
	if err != nil {
		return nil, err
	}
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := s.addMessage(ctx, t, by, domain.AuthorSupportAgent, body, domain.AfterAgentMessage(t.Status))
	if err != nil {
		return nil, err
	}
	if s.Notify != nil {
		msg := v.Messages[len(v.Messages)-1]
		s.Notify.AgentReplied(ctx, v.Ticket, &msg, customerRecipients(v.Ticket, v.Messages))
	}
	return s.staffView(ctx, v.Ticket)
}

// customerRecipients : créateur et personnes de l'organisation qui ont écrit.
func customerRecipients(t *domain.Ticket, msgs []domain.Message) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	add := func(id *uuid.UUID) {
		if id != nil && !seen[*id] {
			seen[*id] = true
			out = append(out, *id)
		}
	}
	add(t.CreatedByUserID)
	for _, m := range msgs {
		if m.AuthorRole == domain.AuthorCustomer {
			add(m.AuthorUserID)
		}
	}
	return out
}

// UpdateInput : champs changés par l'équipe (absents : inchangés).
type UpdateInput struct {
	Status   *string  `json:"status,omitempty" enum:"OPEN,IN_PROGRESS,WAITING_CUSTOMER,RESOLVED,CLOSED" description:"Statut"`
	Priority *string  `json:"priority,omitempty" enum:"LOW,MEDIUM,HIGH,URGENT" description:"Priorité"`
	Category *string  `json:"category,omitempty" enum:"BILLING,TECHNICAL,ABUSE,REQUEST,BUG,FEATURE_REQUEST" description:"Catégorie (vide : aucune)"`
	Tags     []string `json:"tags,omitempty" description:"Étiquettes (remplacent les actuelles)"`
}

// AdminUpdate : statut, priorité, catégorie, étiquettes ; un événement par changement.
func (s *Service) AdminUpdate(ctx context.Context, id uuid.UUID, by Actor, in UpdateInput) (*StaffTicketView, error) {
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var events []domain.Event
	resolved := false
	if in.Status != nil {
		st, err := domain.ParseStatus(*in.Status)
		if err != nil {
			return nil, err
		}
		if st != t.Status {
			events = append(events, s.event(t, domain.EventStatusChanged, by, map[string]string{"status": string(st), "from": string(t.Status)}))
			resolved = st.Done() && !t.Status.Done()
			t.SetStatus(st, now)
		}
	}
	if in.Priority != nil {
		p, err := domain.ParsePriority(*in.Priority)
		if err != nil {
			return nil, err
		}
		if p != t.Priority {
			events = append(events, s.event(t, domain.EventPriorityChanged, by, map[string]string{"priority": string(p), "from": string(t.Priority)}))
			t.Priority = p
		}
	}
	if in.Category != nil {
		c, err := domain.ParseCategory(*in.Category)
		if err != nil {
			return nil, err
		}
		t.Category = c
	}
	if in.Tags != nil {
		tags, err := domain.CleanTags(in.Tags)
		if err != nil {
			return nil, err
		}
		added, removed := domain.TagDiff(t.Tags, tags)
		for _, tag := range added {
			events = append(events, s.event(t, domain.EventTagAdded, by, map[string]string{"tag": tag}))
		}
		for _, tag := range removed {
			events = append(events, s.event(t, domain.EventTagRemoved, by, map[string]string{"tag": tag}))
		}
		t.Tags = tags
	}
	t.LastActivityAt, t.UpdatedAt = now, now
	if err := s.Store.SaveTicket(ctx, t, nil, events); err != nil {
		return nil, err
	}
	s.publish(ctx, t, LiveStatus, "")
	if resolved && s.Notify != nil {
		s.Notify.Resolved(ctx, t)
	}
	return s.staffView(ctx, t)
}

// AssignInput : agent en charge (null : personne).
type AssignInput struct {
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id" description:"Agent en charge (null : retirer)"`
}

func (s *Service) Assign(ctx context.Context, id uuid.UUID, by Actor, to *uuid.UUID) (*StaffTicketView, error) {
	t, err := s.Store.Ticket(ctx, id)
	if err != nil {
		return nil, err
	}
	if to != nil {
		// Back-office complet : seulement un membre de l'équipe.
		exists := s.Store.UserExists
		if s.Staff != nil {
			exists = s.Staff.IsAgent
		}
		ok, err := exists(ctx, *to)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, domain.ErrInvalidAssignee
		}
	}
	assigned := ""
	if to != nil {
		assigned = to.String()
	}
	t.AssignedToUserID = to
	now := s.now()
	t.LastActivityAt, t.UpdatedAt = now, now
	ev := s.event(t, domain.EventAssigned, by, map[string]string{"assigned_to": assigned})
	if err := s.Store.SaveTicket(ctx, t, nil, []domain.Event{ev}); err != nil {
		return nil, err
	}
	s.publish(ctx, t, LiveAssign, "")
	return s.staffView(ctx, t)
}

func (s *Service) Summary(ctx context.Context, me *uuid.UUID) (domain.Summary, error) {
	return s.Store.Summary(ctx, me)
}

// ── Réponses types ──────────────────────────────────────────────────────

// MacroInput : réponse type de l'équipe.
type MacroInput struct {
	Title    string `json:"title" binding:"required" description:"Titre"`
	Body     string `json:"body" binding:"required" description:"Texte de la réponse"`
	Category string `json:"category,omitempty" enum:"BILLING,TECHNICAL,ABUSE,REQUEST,BUG,FEATURE_REQUEST" description:"Catégorie"`
}

func (in MacroInput) clean() (*domain.Macro, error) {
	title := strings.Join(strings.Fields(in.Title), " ")
	if title == "" || utf8.RuneCountInString(title) > domain.SubjectMax {
		return nil, domain.ErrInvalidMacro
	}
	body, err := domain.CleanBody(in.Body, domain.ErrInvalidMacro)
	if err != nil {
		return nil, err
	}
	cat, err := domain.ParseCategory(in.Category)
	if err != nil {
		return nil, err
	}
	return &domain.Macro{Title: title, Body: body, Category: cat}, nil
}

func (s *Service) Macros(ctx context.Context) ([]domain.Macro, error) {
	list, err := s.Store.Macros(ctx)
	if list == nil {
		list = []domain.Macro{}
	}
	return list, err
}

func (s *Service) CreateMacro(ctx context.Context, in MacroInput) (*domain.Macro, error) {
	m, err := in.clean()
	if err != nil {
		return nil, err
	}
	now := s.now()
	m.ID, m.CreatedAt, m.UpdatedAt = uuid.New(), now, now
	if err := s.Store.CreateMacro(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) UpdateMacro(ctx context.Context, id uuid.UUID, in MacroInput) (*domain.Macro, error) {
	m, err := in.clean()
	if err != nil {
		return nil, err
	}
	m.ID, m.UpdatedAt = id, s.now()
	if err := s.Store.UpdateMacro(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) DeleteMacro(ctx context.Context, id uuid.UUID) error {
	return s.Store.DeleteMacro(ctx, id)
}

// ── Commun ──────────────────────────────────────────────────────────────

func (s *Service) addMessage(ctx context.Context, t *domain.Ticket, by Actor, role domain.AuthorRole, body string, next domain.Status) (*TicketView, error) {
	now := s.now()
	msg := &domain.Message{ID: uuid.New(), TicketID: t.ID, OrganizationID: t.OrganizationID, AuthorUserID: by.UserID,
		AuthorRole: role, Body: body, CreatedAt: now, UpdatedAt: now}
	events := []domain.Event{s.event(t, domain.EventCommentAdded, by, map[string]string{"author_role": string(role)})}
	if next != t.Status {
		events = append(events, s.event(t, domain.EventStatusChanged, by, map[string]string{"status": string(next), "from": string(t.Status)}))
		t.SetStatus(next, now)
	}
	t.LastActivityAt, t.UpdatedAt = now, now
	if err := s.Store.SaveTicket(ctx, t, msg, events); err != nil {
		return nil, err
	}
	s.publish(ctx, t, LiveMessage, role)
	return s.view(ctx, t)
}

func (s *Service) changeStatus(ctx context.Context, t *domain.Ticket, by Actor, next domain.Status, extra map[string]string) (*TicketView, error) {
	now := s.now()
	payload := map[string]string{"status": string(next), "from": string(t.Status)}
	for k, v := range extra {
		payload[k] = v
	}
	ev := s.event(t, domain.EventStatusChanged, by, payload)
	t.SetStatus(next, now)
	t.LastActivityAt, t.UpdatedAt = now, now
	if err := s.Store.SaveTicket(ctx, t, nil, []domain.Event{ev}); err != nil {
		return nil, err
	}
	s.publish(ctx, t, LiveStatus, "")
	return s.view(ctx, t)
}

func (s *Service) event(t *domain.Ticket, typ domain.EventType, by Actor, payload map[string]string) domain.Event {
	return domain.Event{ID: uuid.New(), TicketID: t.ID, OrganizationID: t.OrganizationID, Type: typ,
		ActorUserID: by.UserID, Payload: payload, CreatedAt: s.now()}
}

// view : ticket tel que le client le voit (sans les notes internes de
// l'équipe ni leur trace dans le journal).
func (s *Service) view(ctx context.Context, t *domain.Ticket) (*TicketView, error) {
	v, err := s.fullView(ctx, t)
	if err != nil {
		return nil, err
	}
	msgs := v.Messages[:0:0]
	for _, m := range v.Messages {
		if !m.Internal {
			msgs = append(msgs, m)
		}
	}
	events := v.Events[:0:0]
	for _, e := range v.Events {
		if e.Type != domain.EventNoteAdded {
			events = append(events, e)
		}
	}
	v.Messages, v.Events = msgs, events
	return v, nil
}

// fullView : ticket, messages (notes internes comprises), pièces jointes et journal.
func (s *Service) fullView(ctx context.Context, t *domain.Ticket) (*TicketView, error) {
	msgs, err := s.Store.Messages(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	atts, err := s.Store.Attachments(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	events, err := s.Store.Events(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if msgs == nil {
		msgs = []domain.Message{}
	}
	if atts == nil {
		atts = []domain.Attachment{}
	}
	if events == nil {
		events = []domain.Event{}
	}
	for i := range atts {
		atts[i].URL = AttachmentPath(t.OrganizationID, t.ID, atts[i].ID)
	}
	if t.Tags == nil {
		t.Tags = []string{}
	}
	return &TicketView{Ticket: t, Messages: msgs, Attachments: atts, Events: events}, nil
}

// AttachmentPath : téléchargement côté client, relatif à la racine de l'API
// (l'équipe passe par /admin/support/tickets/{id}/attachments/{attachmentID}).
func AttachmentPath(org, ticket, id uuid.UUID) string {
	return "/organizations/" + org.String() + "/support/tickets/" + ticket.String() + "/attachments/" + id.String()
}
