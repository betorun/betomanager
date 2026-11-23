// Package domain : documents juridiques versionnés (conditions générales de
// vente des VPS, politique d'usage acceptable) et leur acceptation par les
// organisations (docs/modules/legal.md).
//
// Une version passe de brouillon à publiée ; publiée, elle ne change plus.
// La version en vigueur d'un document est sa dernière version publiée. Une
// organisation accepte une version précise : une nouvelle version publiée
// doit être acceptée de nouveau.
package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sortes de documents.
const (
	KindTermsVPS = "terms_vps" // conditions générales de vente des serveurs VPS
	KindAUP      = "aup"       // politique d'usage acceptable
)

// Kinds : toutes les sortes, dans l'ordre d'affichage.
var Kinds = []string{KindTermsVPS, KindAUP}

// ValidKind : sorte connue.
func ValidKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// États d'une version.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// Langues des textes.
const (
	LangFR = "fr"
	LangEN = "en"
)

// Limites d'un texte.
const (
	MaxTitle = 200
	MaxBody  = 200_000
)

var (
	ErrUnknownKind       = errors.New("unknown legal document")
	ErrDocumentNotFound  = errors.New("legal document version not found")
	ErrNotDraft          = errors.New("a published version cannot be changed")
	ErrDraftExists       = errors.New("a draft already exists for this document")
	ErrInvalidDocument   = errors.New("title and text are required in French and in English")
	ErrDocumentTooLong   = errors.New("legal document title or text is too long")
	ErrNotCurrent        = errors.New("only the version in force can be accepted")
	ErrTermsNotAccepted  = errors.New("the terms in force must be accepted first")
	ErrNothingToCopy     = errors.New("nothing to copy: write the title and text of the first version")
	ErrInvalidLegalQuery = errors.New("invalid version or language")
)

// Document : une version d'un document juridique (clé : kind + version).
type Document struct {
	Kind        string     `gorm:"primaryKey" json:"kind"`
	Version     int        `gorm:"primaryKey;autoIncrement:false" json:"version"`
	TitleFR     string     `json:"title_fr"`
	TitleEN     string     `json:"title_en"`
	BodyFR      string     `json:"body_fr"`
	BodyEN      string     `json:"body_en"`
	Status      string     `json:"status"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedBy   string     `json:"updated_by"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PublishedBy string     `json:"published_by,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

func (Document) TableName() string { return "legal_documents" }

// Published : version publiée (immuable).
func (d *Document) Published() bool { return d.Status == StatusPublished }

// Text : le texte dans une langue (français par défaut).
func (d *Document) Text(lang string) (title, body string) {
	if lang == LangEN {
		return d.TitleEN, d.BodyEN
	}
	return d.TitleFR, d.BodyFR
}

// Content : ce que l'équipe rédige.
type Content struct {
	TitleFR string `json:"title_fr"`
	TitleEN string `json:"title_en"`
	BodyFR  string `json:"body_fr"`
	BodyEN  string `json:"body_en"`
}

// Normalize : espaces de tête et de fin retirés.
func (c Content) Normalize() Content {
	return Content{TitleFR: strings.TrimSpace(c.TitleFR), TitleEN: strings.TrimSpace(c.TitleEN),
		BodyFR: strings.TrimSpace(c.BodyFR), BodyEN: strings.TrimSpace(c.BodyEN)}
}

// Empty : rien n'est rédigé.
func (c Content) Empty() bool {
	n := c.Normalize()
	return n.TitleFR == "" && n.TitleEN == "" && n.BodyFR == "" && n.BodyEN == ""
}

// Validate : les deux langues sont rédigées, dans les limites.
func (c Content) Validate() error {
	n := c.Normalize()
	if n.TitleFR == "" || n.TitleEN == "" || n.BodyFR == "" || n.BodyEN == "" {
		return ErrInvalidDocument
	}
	if len(n.TitleFR) > MaxTitle || len(n.TitleEN) > MaxTitle || len(n.BodyFR) > MaxBody || len(n.BodyEN) > MaxBody {
		return ErrDocumentTooLong
	}
	return nil
}

// ContentOf : le texte d'une version.
func ContentOf(d *Document) Content {
	return Content{TitleFR: d.TitleFR, TitleEN: d.TitleEN, BodyFR: d.BodyFR, BodyEN: d.BodyEN}
}

// Acceptance : une organisation accepte une version d'un document.
type Acceptance struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid" json:"organization_id"`
	Kind           string    `json:"kind"`
	Version        int       `json:"version"`
	Principal      string    `json:"principal"`
	ClientIP       string    `json:"client_ip,omitempty"`
	AcceptedAt     time.Time `json:"accepted_at"`
}

func (Acceptance) TableName() string { return "legal_acceptances" }

// NeedsAcceptance : une version est en vigueur et l'organisation n'a pas
// accepté celle-là (ou une plus récente). Sans version en vigueur, rien à accepter.
func NeedsAcceptance(current, accepted int) bool {
	return current > 0 && accepted < current
}

// ParseLang : « fr » ou « en » ; vide : fallback.
func ParseLang(lang, fallback string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "":
		return fallback, true
	case LangFR:
		return LangFR, true
	case LangEN:
		return LangEN, true
	}
	return "", false
}
