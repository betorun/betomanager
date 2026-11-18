package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// toolNameRe : noms acceptés par tous les fournisseurs compatibles (OpenAI,
// Anthropic, Gemini, vLLM).
var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Validate : la requête est-elle bien formée ? Appelé par chaque adaptateur
// avant tout envoi ; une erreur est de type KindInvalidRequest.
func (r Request) Validate() error {
	if r.MaxOutputTokens <= 0 {
		return invalid("MaxOutputTokens est obligatoire (il borne le coût)")
	}
	if len(r.Messages) == 0 {
		return invalid("conversation vide")
	}
	seen := make(map[string]bool, len(r.Tools))
	for _, t := range r.Tools {
		if !toolNameRe.MatchString(t.Name) {
			return invalid(fmt.Sprintf("nom d'outil invalide %q", t.Name))
		}
		if seen[t.Name] {
			return invalid(fmt.Sprintf("outil en double %q", t.Name))
		}
		seen[t.Name] = true
		if !isJSONObject(t.Parameters) {
			return invalid(fmt.Sprintf("outil %q : Parameters doit être un schéma JSON de type objet", t.Name))
		}
	}
	switch r.ToolChoice.Mode {
	case ToolChoiceAuto, ToolChoiceNone:
	case ToolChoiceRequired:
		if len(r.Tools) == 0 {
			return invalid("ToolChoiceRequired sans outil")
		}
	case ToolChoiceTool:
		if !seen[r.ToolChoice.Name] {
			return invalid(fmt.Sprintf("ToolChoice vise un outil absent %q", r.ToolChoice.Name))
		}
	default:
		return invalid(fmt.Sprintf("ToolChoice inconnu %q", r.ToolChoice.Mode))
	}

	// Chaque résultat d'outil répond à un appel du message assistant qui le
	// précède (les fournisseurs refusent sinon, avec un message obscur).
	pending := map[string]bool{}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleUser:
			pending = map[string]bool{}
		case RoleAssistant:
			pending = map[string]bool{}
			for _, c := range m.ToolCalls {
				if c.ID == "" || c.Name == "" {
					return invalid(fmt.Sprintf("message %d : appel d'outil sans identifiant ou sans nom", i))
				}
				pending[c.ID] = true
			}
		case RoleTool:
			if m.ToolResult == nil || !pending[m.ToolResult.CallID] {
				return invalid(fmt.Sprintf("message %d : résultat d'outil sans appel correspondant", i))
			}
			delete(pending, m.ToolResult.CallID)
		default:
			return invalid(fmt.Sprintf("message %d : rôle inconnu %q", i, m.Role))
		}
	}
	return nil
}

// CheckToolCalls : les appels rendus par le modèle sont-ils exploitables ?
// Nom non vide et déclaré dans la requête, arguments en objet JSON. Les
// adaptateurs l'appliquent avant de rendre une réponse : en aval, un ToolCall
// est toujours sûr à router. Des arguments vides deviennent « {} ».
func CheckToolCalls(provider string, req Request, calls []ToolCall) error {
	declared := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		declared[t.Name] = true
	}
	for i := range calls {
		c := &calls[i]
		if c.Name == "" {
			return malformed(provider, "appel d'outil sans nom")
		}
		if !declared[c.Name] {
			return malformed(provider, fmt.Sprintf("appel d'un outil non proposé %q", c.Name))
		}
		if len(bytes.TrimSpace(c.Arguments)) == 0 {
			c.Arguments = json.RawMessage(`{}`)
		}
		if !isJSONObject(c.Arguments) {
			return malformed(provider, fmt.Sprintf("outil %q : arguments qui ne sont pas un objet JSON", c.Name))
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil && v != nil
}

func invalid(msg string) *Error {
	return &Error{Kind: KindInvalidRequest, Message: msg}
}

func malformed(provider, msg string) *Error {
	return &Error{Kind: KindMalformedOutput, Provider: provider, Message: msg}
}
