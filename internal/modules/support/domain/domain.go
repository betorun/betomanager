// Package domain : tickets de support (docs/modules/support.md). Même modèle
// que le support de SYK : ticket, messages, pièces jointes, journal
// d'événements, réponses types de l'équipe.
package domain

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Status string

const (
	StatusOpen            Status = "OPEN"
	StatusInProgress      Status = "IN_PROGRESS"
	StatusWaitingCustomer Status = "WAITING_CUSTOMER"
	StatusResolved        Status = "RESOLVED"
	StatusClosed          Status = "CLOSED"
)

type Priority string

const (
	PriorityLow    Priority = "LOW"
	PriorityMedium Priority = "MEDIUM"
	PriorityHigh   Priority = "HIGH"
	PriorityUrgent Priority = "URGENT"
)

type Category string

const (
	CategoryBilling   Category = "BILLING"
	CategoryTechnical Category = "TECHNICAL"
	CategoryAbuse     Category = "ABUSE"
	// CategoryRequest : ouverture du port 25, déblocage d'une adresse…
	CategoryRequest        Category = "REQUEST"
	CategoryBug            Category = "BUG"
	CategoryFeatureRequest Category = "FEATURE_REQUEST"
)

// AuthorRole : qui a écrit un message.
type AuthorRole string

const (
	AuthorCustomer     AuthorRole = "CUSTOMER"
	AuthorSupportAgent AuthorRole = "SUPPORT_AGENT"
	AuthorSystem       AuthorRole = "SYSTEM"
)

// EventType : ligne du journal d'un ticket.
type EventType string

const (
	EventTicketCreated   EventType = "TICKET_CREATED"
	EventStatusChanged   EventType = "STATUS_CHANGED"
	EventAssigned        EventType = "ASSIGNED"
	EventPriorityChanged EventType = "PRIORITY_CHANGED"
	EventTagAdded        EventType = "TAG_ADDED"
	EventTagRemoved      EventType = "TAG_REMOVED"
	EventCommentAdded    EventType = "COMMENT_ADDED"
)

// Bornes des textes et des fichiers.
const (
	SubjectMin      = 3
	SubjectMax      = 150
	BodyMax         = 10000
	MaxTags         = 10
	TagMax          = 40
	ResourceKindMax = 40
	// AttachmentMaxSize : 10 Mo par fichier.
	AttachmentMaxSize = 10 << 20
	// MaxAttachments : par ticket.
	MaxAttachments = 20
	FileNameMax    = 200
	// DefaultLimit / MaxLimit : pagination des listes.
	DefaultLimit = 20
	MaxLimit     = 100
)

// Ticket : une demande d'une organisation. Description : la demande
// initiale (hors messages).
type Ticket struct {
	ID               uuid.UUID  `json:"id"`
	OrganizationID   uuid.UUID  `json:"organization_id"`
	ProjectID        *uuid.UUID `json:"project_id,omitempty"`
	ResourceKind     string     `json:"resource_kind,omitempty"`
	ResourceID       *uuid.UUID `json:"resource_id,omitempty"`
	CreatedBy        string     `json:"-"`
	CreatedByUserID  *uuid.UUID `json:"created_by_user_id,omitempty"`
	CreatedByEmail   string     `json:"created_by_email,omitempty"`
	Subject          string     `json:"subject"`
	Description      string     `json:"description"`
	Status           Status     `json:"status"`
	Priority         Priority   `json:"priority"`
	Category         Category   `json:"category,omitempty"`
	Tags             []string   `json:"tags"`
	AssignedToUserID *uuid.UUID `json:"assigned_to_user_id,omitempty"`
	LastActivityAt   time.Time  `json:"last_activity_at"`
	ClosedAt         *time.Time `json:"closed_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type Message struct {
	ID             uuid.UUID  `json:"id"`
	TicketID       uuid.UUID  `json:"ticket_id"`
	OrganizationID uuid.UUID  `json:"organization_id"`
	AuthorUserID   *uuid.UUID `json:"author_user_id,omitempty"`
	AuthorRole     AuthorRole `json:"author_role"`
	Body           string     `json:"body"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Attachment : fichier joint, téléchargé par l'API (URL relative à la racine
// de l'API, côté client).
type Attachment struct {
	ID        uuid.UUID  `json:"id"`
	TicketID  uuid.UUID  `json:"ticket_id"`
	MessageID *uuid.UUID `json:"message_id,omitempty"`
	URL       string     `json:"url"`
	FileName  string     `json:"file_name"`
	MimeType  string     `json:"mime_type"`
	Size      int64      `json:"size"`
	ObjectKey string     `json:"-"`
	CreatedBy string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type Event struct {
	ID             uuid.UUID         `json:"id"`
	TicketID       uuid.UUID         `json:"ticket_id"`
	OrganizationID uuid.UUID         `json:"organization_id"`
	Type           EventType         `json:"type"`
	ActorUserID    *uuid.UUID        `json:"actor_user_id,omitempty"`
	Payload        map[string]string `json:"payload,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

// Macro : réponse type de l'équipe.
type Macro struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Category  Category  `json:"category,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Summary : compteurs du menu du back-office.
type Summary struct {
	Open            int64 `json:"open"`
	InProgress      int64 `json:"in_progress"`
	WaitingCustomer int64 `json:"waiting_customer"`
	// HighPriority : tickets à traiter (OPEN, IN_PROGRESS) en HIGH ou URGENT.
	HighPriority int64 `json:"high_priority"`
}

// Filter : liste des tickets. OrganizationID nil : toutes (équipe).
type Filter struct {
	OrganizationID *uuid.UUID
	Status         Status
	Category       Category
	Priority       Priority
	Search         string
	// StaffOrder : à traiter d'abord, puis priorité, plus ancienne activité d'abord.
	StaffOrder bool
	Limit      int
	Offset     int
}

// Erreurs (texte en anglais, traduction dans core/i18n/catalog/support.json).
var (
	ErrTicketNotFound         = errors.New("support ticket not found")
	ErrInvalidSubject         = errors.New("the subject must be 3 to 150 characters long")
	ErrInvalidDescription     = errors.New("the description must be 1 to 10,000 characters long")
	ErrInvalidBody            = errors.New("the message must be 1 to 10,000 characters long")
	ErrInvalidStatus          = errors.New("unknown ticket status")
	ErrInvalidPriority        = errors.New("unknown ticket priority")
	ErrInvalidCategory        = errors.New("unknown ticket category")
	ErrInvalidTags            = errors.New("at most 10 tags of 40 characters each")
	ErrInvalidResource        = errors.New("resource_kind and resource_id go together")
	ErrProjectNotInOrg        = errors.New("this project does not belong to the organization")
	ErrTicketClosed           = errors.New("this ticket is closed and cannot be reopened; open a new one")
	ErrInvalidAssignee        = errors.New("unknown support agent")
	ErrMacroNotFound          = errors.New("support macro not found")
	ErrInvalidMacro           = errors.New("a macro needs a title (1 to 150 characters) and a body")
	ErrAttachmentNotFound     = errors.New("attachment not found")
	ErrAttachmentTooLarge     = errors.New("the file exceeds 10 MB")
	ErrAttachmentType         = errors.New("file type not allowed (PDF, JPEG, PNG, WebP or text)")
	ErrAttachmentLimit        = errors.New("this ticket already has 20 attachments")
	ErrAttachmentMissing      = errors.New(`missing "file" field`)
	ErrAttachmentsUnavailable = errors.New("attachments are not available for now")
	ErrMessageNotOnTicket     = errors.New("this message does not belong to the ticket")
)

func ParseStatus(s string) (Status, error) {
	switch v := Status(strings.ToUpper(strings.TrimSpace(s))); v {
	case StatusOpen, StatusInProgress, StatusWaitingCustomer, StatusResolved, StatusClosed:
		return v, nil
	}
	return "", ErrInvalidStatus
}

func ParsePriority(s string) (Priority, error) {
	switch v := Priority(strings.ToUpper(strings.TrimSpace(s))); v {
	case PriorityLow, PriorityMedium, PriorityHigh, PriorityUrgent:
		return v, nil
	}
	return "", ErrInvalidPriority
}

// ParseCategory : "" reste "" (sans catégorie).
func ParseCategory(s string) (Category, error) {
	switch v := Category(strings.ToUpper(strings.TrimSpace(s))); v {
	case "", CategoryBilling, CategoryTechnical, CategoryAbuse, CategoryRequest, CategoryBug, CategoryFeatureRequest:
		return v, nil
	}
	return "", ErrInvalidCategory
}

func CleanSubject(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if n := utf8.RuneCountInString(s); n < SubjectMin || n > SubjectMax {
		return "", ErrInvalidSubject
	}
	return s, nil
}

// CleanBody : texte brut, 1 à 10 000 caractères ; err est rendue si vide ou trop long.
func CleanBody(s string, err error) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > BodyMax || !utf8.ValidString(s) {
		return "", err
	}
	return s, nil
}

// CleanTags : minuscules, sans doublon, triées.
func CleanTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		t = strings.ToLower(strings.Join(strings.Fields(t), " "))
		if t == "" || seen[t] {
			continue
		}
		if utf8.RuneCountInString(t) > TagMax {
			return nil, ErrInvalidTags
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, ErrInvalidTags
	}
	sort.Strings(out)
	return out, nil
}

// TagDiff : étiquettes ajoutées et retirées.
func TagDiff(before, after []string) (added, removed []string) {
	in := func(list []string, t string) bool {
		for _, x := range list {
			if x == t {
				return true
			}
		}
		return false
	}
	for _, t := range after {
		if !in(before, t) {
			added = append(added, t)
		}
	}
	for _, t := range before {
		if !in(after, t) {
			removed = append(removed, t)
		}
	}
	return added, removed
}

// Done : résolu ou fermé.
func (s Status) Done() bool { return s == StatusResolved || s == StatusClosed }

// Active : à traiter par l'équipe.
func (s Status) Active() bool { return s == StatusOpen || s == StatusInProgress }

// AfterCustomerMessage : statut après une réponse du client. Fermé : refus ;
// résolu : rouvert ; en attente du client : repasse à traiter.
func AfterCustomerMessage(s Status) (Status, error) {
	switch s {
	case StatusClosed:
		return s, ErrTicketClosed
	case StatusResolved:
		return StatusOpen, nil
	case StatusWaitingCustomer, "":
		return StatusInProgress, nil
	}
	return s, nil
}

// AfterAgentMessage : une réponse de l'équipe attend le client.
func AfterAgentMessage(Status) Status { return StatusWaitingCustomer }

// Reopen : le client relance un ticket résolu ; fermé est définitif ; un
// ticket actif ne change pas (changed false).
func Reopen(s Status) (next Status, changed bool, err error) {
	switch s {
	case StatusClosed:
		return s, false, ErrTicketClosed
	case StatusResolved:
		return StatusOpen, true, nil
	}
	return s, false, nil
}

// SetStatus : change le statut et la date de clôture qui va avec.
func (t *Ticket) SetStatus(s Status, now time.Time) {
	t.Status = s
	if s.Done() {
		if t.ClosedAt == nil {
			t.ClosedAt = &now
		}
		return
	}
	t.ClosedAt = nil
}

// AllowedMIME : types acceptés en pièce jointe (contenu reconnu, pas l'extension).
var allowedMIME = map[string]bool{
	"application/pdf": true, "image/jpeg": true, "image/png": true, "image/webp": true, "text/plain": true,
}

// DetectMIME : type du contenu (premiers octets) s'il est accepté.
func DetectMIME(head []byte) (string, error) {
	mime := http.DetectContentType(head)
	mime, _, _ = strings.Cut(mime, ";")
	mime = strings.TrimSpace(mime)
	if !allowedMIME[mime] {
		return "", ErrAttachmentType
	}
	return mime, nil
}

// CleanFileName : nom affiché et proposé au téléchargement (sans chemin ni
// caractères de contrôle ou guillemets).
func CleanFileName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == ';' {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimSpace(b.String())
	if name == "" || name == "." || name == ".." {
		name = "fichier"
	}
	if utf8.RuneCountInString(name) > FileNameMax {
		name = string([]rune(name)[:FileNameMax])
	}
	return name
}

// Page : limite 20 par défaut, 100 au plus.
func Page(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
