package llm

import (
	"context"
	"encoding/json"
	"iter"
)

// Provider : un modèle de langage, ou une pile de décorateurs autour d'un modèle.
//
// Stream rend les événements dans l'ordre ; le dernier est toujours EventDone,
// qui porte la Response complète (texte entier, appels d'outils vérifiés,
// consommation). Une erreur arrête le flux : elle est rendue une fois, avec un
// Event vide. Interrompre la boucle (break) libère la connexion.
type Provider interface {
	Name() string
	Generate(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request) iter.Seq2[Event, error]
}

// Role : auteur d'un message. Le prompt système n'est pas un message
// (Request.System) : chaque fournisseur le place à sa façon.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message : un tour de conversation.
//   - RoleUser      : Text.
//   - RoleAssistant : Text et/ou ToolCalls.
//   - RoleTool      : ToolResult (résultat d'un appel du message précédent).
type Message struct {
	Role       Role
	Text       string
	ToolCalls  []ToolCall
	ToolResult *ToolResult
	// Native : message assistant tel que le fournisseur l'a produit (blocs
	// de réflexion compris). Renvoyé à l'identique au même fournisseur, qui
	// peut exiger un historique inchangé ; les autres l'ignorent et lisent
	// Text et ToolCalls.
	Native *Native
}

// Native : contenu propre à un fournisseur (Provider = Name() de
// l'adaptateur qui l'a produit). Data n'est lu que par cet adaptateur.
type Native struct {
	Provider string
	Data     any
}

// ToolCall : appel d'outil demandé par le modèle. Arguments est un objet JSON.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult : réponse de l'outil, renvoyée au modèle. Content est une donnée,
// jamais une consigne : l'appelant l'encadre comme telle.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Tool : outil proposé au modèle. Parameters est un schéma JSON de type objet.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolChoiceMode : liberté laissée au modèle.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "" // le modèle décide (défaut)
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool" // l'outil Name, obligatoirement
)

type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

// Caller : pour qui l'appel est fait. Sert au budget et aux journaux ; n'est
// JAMAIS transmis au fournisseur.
type Caller struct {
	OrgID  string
	UserID string
	TurnID string
}

// Request : un appel au modèle.
type Request struct {
	System     string
	Messages   []Message
	Tools      []Tool
	ToolChoice ToolChoice
	// MaxOutputTokens est obligatoire : il borne le coût réservé par Budget.
	MaxOutputTokens int
	// Temperature : nil = valeur du fournisseur.
	Temperature *float64
	Caller      Caller
}

// StopReason : pourquoi le modèle s'est arrêté.
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopToolUse       StopReason = "tool_use"
	StopMaxTokens     StopReason = "max_tokens"
	StopContentFilter StopReason = "content_filter"
	StopUnknown       StopReason = "unknown"
)

// Usage : jetons consommés. Known=false quand le fournisseur ne l'a pas dit
// (flux coupé) : Budget garde alors toute la réservation.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Known        bool
}

// Response : réponse complète du modèle (Message.Role == RoleAssistant).
type Response struct {
	Message  Message
	Stop     StopReason
	Usage    Usage
	Provider string
	Model    string
}

// EventKind : type d'événement d'un flux.
type EventKind string

const (
	EventText     EventKind = "text"      // Text : morceau de réponse
	EventToolCall EventKind = "tool_call" // ToolCall : appel complet et vérifié
	EventDone     EventKind = "done"      // Response : fin du flux
)

type Event struct {
	Kind     EventKind
	Text     string
	ToolCall *ToolCall
	Response *Response
}
