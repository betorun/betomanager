package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "123456789:AAH-test_token_value_0123456789abcd"

// fakeTelegram : API des bots en mémoire ; garde la dernière requête.
func fakeTelegram(t *testing.T, handle func(method string, body map[string]any) (int, string)) (*WebhookService, *struct {
	path string
	body map[string]any
}) {
	t.Helper()
	last := &struct {
		path string
		body map[string]any
	}{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		last.path, last.body = r.URL.Path, map[string]any{}
		_ = json.Unmarshal(raw, &last.body)
		require.Equal(t, "", r.URL.RawQuery, "jamais de paramètre dans l'adresse appelée")
		prefix := "/bot" + testToken + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		code, out := handle(strings.TrimPrefix(r.URL.Path, prefix), last.body)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	old := TelegramAPI
	TelegramAPI = srv.URL
	t.Cleanup(func() { TelegramAPI = old })
	return &WebhookService{Sealer: plainSealer{}, Client: srv.Client()}, last
}

func TestTelegramChats_ListsRecentChatsOnce(t *testing.T) {
	s, _ := fakeTelegram(t, func(method string, _ map[string]any) (int, string) {
		switch method {
		case "getMe":
			return 200, `{"ok":true,"result":{"username":"ops_alerts_bot"}}`
		case "getUpdates":
			return 200, `{"ok":true,"result":[
				{"my_chat_member":{"chat":{"id":-1001,"type":"supergroup","title":"Équipe ops"}}},
				{"message":{"chat":{"id":42,"type":"private","first_name":"Awa","last_name":"Diallo"}}},
				{"message":{"chat":{"id":-1001,"type":"supergroup","title":"Équipe ops"}}}]}`
		}
		return 404, `{"ok":false,"error_code":404}`
	})
	out, err := s.TelegramChats(context.Background(), " "+testToken+" ")
	require.NoError(t, err)
	assert.Equal(t, "ops_alerts_bot", out.Bot)
	assert.Equal(t, []TelegramChat{{ID: "-1001", Title: "Équipe ops", Type: "supergroup"}, {ID: "42", Title: "Awa Diallo", Type: "private"}}, out.Chats)
}

func TestTelegramChats_Errors(t *testing.T) {
	s, _ := fakeTelegram(t, func(method string, _ map[string]any) (int, string) {
		if method == "getUpdates" {
			return 409, `{"ok":false,"error_code":409,"description":"Conflict: can't use getUpdates method while webhook is active"}`
		}
		return 200, `{"ok":true,"result":{"username":"b"}}`
	})
	_, err := s.TelegramChats(context.Background(), "pas-un-jeton")
	assert.ErrorIs(t, err, ErrTelegramToken)
	_, err = s.TelegramChats(context.Background(), "987654321:AAH-wrong_token_value_0123456789ab")
	assert.ErrorIs(t, err, ErrTelegramToken, "jeton refusé par Telegram")
	assert.NotContains(t, err.Error(), "987654321", "le jeton n'apparaît jamais dans une erreur")
	_, err = s.TelegramChats(context.Background(), testToken)
	assert.ErrorIs(t, err, ErrTelegramHooked)
}

func TestTelegramTarget_VerifiesChat(t *testing.T) {
	s, last := fakeTelegram(t, func(_ string, body map[string]any) (int, string) {
		if body["chat_id"] == "-1001" {
			return 200, `{"ok":true,"result":{"id":-1001,"type":"supergroup","title":"Équipe ops"}}`
		}
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	})
	u, label, err := s.telegramTarget(context.Background(), testToken, "-1001")
	require.NoError(t, err)
	assert.Equal(t, "/bot"+testToken+"/getChat", last.path)
	assert.Equal(t, "Telegram · Équipe ops", label, "jamais le jeton dans le libellé")
	assert.True(t, strings.HasSuffix(u, "/sendMessage?chat_id=-1001"))

	_, _, err = s.telegramTarget(context.Background(), testToken, "-2002")
	assert.ErrorIs(t, err, ErrTelegramChat)
	_, _, err = s.telegramTarget(context.Background(), testToken, "nimporte quoi")
	assert.ErrorIs(t, err, ErrTelegramChat)
}

func TestPost_Telegram(t *testing.T) {
	s, last := fakeTelegram(t, func(string, map[string]any) (int, string) { return 200, `{"ok":true,"result":{}}` })
	row := webhookRow{Webhook: Webhook{ID: uuid.New(), Kind: WebhookTelegram},
		URLSealed: []byte(TelegramAPI + "/bot" + testToken + "/sendMessage?chat_id=-1001")}
	ev := WebhookEvent{Type: "disk.100", Project: "boutique", Title: "Disque plein", Text: "a <b> & c",
		Link: "https://console/x?a=1&b=2", SentAt: time.Unix(1_800_000_000, 0)}
	require.Equal(t, "ok", s.post(context.Background(), row, ev))
	assert.Equal(t, "/bot"+testToken+"/sendMessage", last.path)
	assert.Equal(t, "-1001", last.body["chat_id"])
	assert.Equal(t, "HTML", last.body["parse_mode"])
	assert.Equal(t, "<b>Disque plein</b>\na &lt;b&gt; &amp; c\n<a href=\"https://console/x?a=1&amp;b=2\">boutique</a>", last.body["text"])
}
