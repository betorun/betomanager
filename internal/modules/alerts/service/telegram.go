package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Telegram : le client apporte son bot (créé avec @BotFather) et l'ajoute à
// sa discussion ; la console retrouve la discussion (getUpdates) et
// l'enregistre. Le jeton est chiffré avec l'URL (comme l'URL Slack) et n'est
// jamais renvoyé ; il n'apparaît dans aucune erreur ni aucun journal.

// WebhookTelegram : sorte de webhook « Telegram ».
const WebhookTelegram = "telegram"

// TelegramAPI : adresse de l'API des bots (variable pour les essais).
var TelegramAPI = "https://api.telegram.org"

// Erreurs Telegram.
var (
	ErrTelegramToken   = errors.New("the Telegram bot token is invalid")
	ErrTelegramChat    = errors.New("the bot cannot write to this Telegram chat: add it to the chat, then try again")
	ErrTelegramHooked  = errors.New("this Telegram bot already sends its messages to another service: use a bot dedicated to alerts")
	ErrTelegramFailure = errors.New("Telegram could not be reached, try again")
)

var telegramTokenRE = regexp.MustCompile(`^[0-9]{5,15}:[A-Za-z0-9_-]{30,64}$`)

// chat_id : nombre (négatif pour un groupe) ou @nom d'un canal public.
var telegramChatRE = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$`)

// TelegramChatsRequest : jeton du bot dont on cherche les discussions.
type TelegramChatsRequest struct {
	BotToken string `json:"bot_token" binding:"required" description:"Jeton du bot donné par @BotFather"`
}

// TelegramChat : discussion où le bot a été ajouté ou a reçu un message.
type TelegramChat struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Type : private, group, supergroup ou channel.
	Type string `json:"type"`
}

// TelegramChats : le bot et ses discussions récentes.
type TelegramChats struct {
	Bot   string         `json:"bot"`
	Chats []TelegramChat `json:"chats"`
}

type tgChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

func (c tgChat) view() TelegramChat {
	title := c.Title
	if title == "" {
		title = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	if title == "" && c.Username != "" {
		title = "@" + c.Username
	}
	return TelegramChat{ID: strconv.FormatInt(c.ID, 10), Title: title, Type: c.Type}
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func (s *WebhookService) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return PublicHTTPClient()
}

// telegramCall : une méthode de l'API des bots. Les erreurs ne contiennent
// jamais l'adresse (elle porte le jeton).
func (s *WebhookService) telegramCall(ctx context.Context, token, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TelegramAPI+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return ErrTelegramFailure
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "betorun-alerts/1")
	res, err := s.client().Do(req)
	if err != nil {
		return ErrTelegramFailure
	}
	defer res.Body.Close()
	var r tgResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&r); err != nil {
		return ErrTelegramFailure
	}
	if !r.OK {
		switch r.ErrorCode {
		case http.StatusUnauthorized, http.StatusNotFound:
			return ErrTelegramToken
		case http.StatusConflict:
			return ErrTelegramHooked
		case http.StatusBadRequest, http.StatusForbidden:
			return ErrTelegramChat
		}
		return ErrTelegramFailure
	}
	if out != nil && json.Unmarshal(r.Result, out) != nil {
		return ErrTelegramFailure
	}
	return nil
}

// TelegramChats : nom du bot et discussions où il a été ajouté ou a reçu un
// message (24 h d'historique côté Telegram). Rien n'est enregistré.
func (s *WebhookService) TelegramChats(ctx context.Context, token string) (*TelegramChats, error) {
	token = strings.TrimSpace(token)
	if !telegramTokenRE.MatchString(token) {
		return nil, ErrTelegramToken
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := s.telegramCall(ctx, token, "getMe", map[string]any{}, &me); err != nil {
		return nil, err
	}
	var updates []struct {
		Message           *struct{ Chat tgChat } `json:"message"`
		ChannelPost       *struct{ Chat tgChat } `json:"channel_post"`
		MyChatMember      *struct{ Chat tgChat } `json:"my_chat_member"`
		EditedMessage     *struct{ Chat tgChat } `json:"edited_message"`
		EditedChannelPost *struct{ Chat tgChat } `json:"edited_channel_post"`
	}
	if err := s.telegramCall(ctx, token, "getUpdates", map[string]any{
		"limit": 100, "allowed_updates": []string{"message", "channel_post", "my_chat_member", "edited_message", "edited_channel_post"},
	}, &updates); err != nil {
		return nil, err
	}
	out := &TelegramChats{Bot: me.Username, Chats: []TelegramChat{}}
	seen := map[int64]bool{}
	add := func(c tgChat) {
		if c.ID == 0 || seen[c.ID] {
			return
		}
		seen[c.ID] = true
		out.Chats = append(out.Chats, c.view())
	}
	// Les plus récentes d'abord.
	for _, u := range slices.Backward(updates) {
		for _, m := range []*struct{ Chat tgChat }{u.MyChatMember, u.Message, u.ChannelPost, u.EditedMessage, u.EditedChannelPost} {
			if m != nil {
				add(m.Chat)
			}
		}
	}
	return out, nil
}

// telegramTarget : jeton et discussion vérifiés auprès de Telegram ; rend
// l'adresse à chiffrer et le libellé affiché (« Telegram · Équipe ops »).
func (s *WebhookService) telegramTarget(ctx context.Context, token, chatID string) (string, string, error) {
	token, chatID = strings.TrimSpace(token), strings.TrimSpace(chatID)
	if !telegramTokenRE.MatchString(token) {
		return "", "", ErrTelegramToken
	}
	if !telegramChatRE.MatchString(chatID) {
		return "", "", ErrTelegramChat
	}
	var chat tgChat
	if err := s.telegramCall(ctx, token, "getChat", map[string]any{"chat_id": chatID}, &chat); err != nil {
		return "", "", err
	}
	label := chat.view().Title
	if r := []rune(label); len(r) > 40 {
		label = string(r[:40]) + "…"
	}
	if label == "" {
		label = chatID
	}
	u := TelegramAPI + "/bot" + token + "/sendMessage?chat_id=" + url.QueryEscape(chatID)
	return u, "Telegram · " + label, nil
}

// telegramRequest : l'adresse enregistrée porte la discussion en paramètre ;
// elle passe dans le corps, l'adresse appelée n'a plus de paramètre.
func telegramRequest(raw string, ev WebhookEvent) (string, []byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, err
	}
	chat := u.Query().Get("chat_id")
	u.RawQuery = ""
	text := "<b>" + html.EscapeString(ev.Title) + "</b>\n" + html.EscapeString(ev.Text)
	if ev.Link != "" {
		text += "\n<a href=\"" + html.EscapeString(ev.Link) + "\">" + html.EscapeString(ev.Project) + "</a>"
	}
	body, _ := json.Marshal(map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true}})
	return u.String(), body, nil
}
