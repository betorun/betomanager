// Package store : mémoire courte des conversations du Copilot (Redis).
//
// On ne garde que le texte : messages de la personne et réponses finales.
// Les appels d'outils et leurs résultats (journaux, configurations) ne sont
// pas conservés : relus au besoin, ils sont toujours à jour, et la mémoire ne
// devient pas un second stockage de données sensibles.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thekrauss/kubemanager/internal/infrastructure/llm"
)

const (
	// TTL : une conversation inactive est oubliée après ce délai.
	TTL = 24 * time.Hour
	// MaxMessages : messages gardés (les plus récents), réponses comprises.
	MaxMessages = 20
	// maxStoredText : un message très long est tronqué dans la mémoire.
	maxStoredText = 8 << 10
)

// Message : un message gardé.
type Message struct {
	Role string    `json:"role"` // "user" | "assistant"
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Store : conversations par personne. La clé contient l'identifiant de la
// personne : une conversation n'est jamais lisible par une autre.
type Store struct {
	rdb redis.UniversalClient
	now func() time.Time
}

func New(rdb redis.UniversalClient) *Store {
	return &Store{rdb: rdb, now: time.Now}
}

func key(userID, conversationID string) string {
	return "betorun:copilot:conv:" + userID + ":" + conversationID
}

// Load : messages d'une conversation (vide si inconnue ou expirée).
func (s *Store) Load(ctx context.Context, userID, conversationID string) ([]Message, error) {
	raw, err := s.rdb.Get(ctx, key(userID, conversationID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, nil // format inconnu : on repart de zéro
	}
	return msgs, nil
}

// Append : ajoute un échange (question, réponse) et garde les plus récents.
func (s *Store) Append(ctx context.Context, userID, conversationID, question, answer string) error {
	msgs, err := s.Load(ctx, userID, conversationID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	msgs = append(msgs, Message{Role: "user", Text: clip(question), At: now})
	if answer != "" {
		msgs = append(msgs, Message{Role: "assistant", Text: clip(answer), At: now})
	}
	if len(msgs) > MaxMessages {
		msgs = msgs[len(msgs)-MaxMessages:]
		if msgs[0].Role == "assistant" { // toujours commencer par une question
			msgs = msgs[1:]
		}
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, key(userID, conversationID), raw, TTL).Err()
}

// Clear : oublie une conversation.
func (s *Store) Clear(ctx context.Context, userID, conversationID string) error {
	return s.rdb.Del(ctx, key(userID, conversationID)).Err()
}

// History : messages au format llm.
func History(msgs []Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		role := llm.RoleUser
		if m.Role == "assistant" {
			role = llm.RoleAssistant
		}
		out = append(out, llm.Message{Role: role, Text: m.Text})
	}
	return out
}

func clip(s string) string {
	if len(s) <= maxStoredText {
		return s
	}
	cut := s[:maxStoredText]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && cut[len(cut)-1] >= 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
