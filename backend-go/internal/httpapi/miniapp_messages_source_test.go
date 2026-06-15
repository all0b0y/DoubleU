package httpapi

import (
	"net/http"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// TestMessagesSourceAttribution verifies the Mini App messages API tags each
// owner message with its origin: replies sent through DoubleU (manual-send
// "out:" event ids or sent drafts) report source "app", while messages the
// owner typed straight to the bot in Telegram report source "telegram". Contact
// messages stay incoming with no source. This is what lets the owner tell
// personal use (talking to the bot directly) apart from app-driven replies.
func TestMessagesSourceAttribution(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: "42", DisplayName: "Иван"})
	now := time.Unix(1700000000, 0).UTC()

	save := func(eventID, sender, text string, offset int) {
		repo.SaveMessage(contracts.NormalizedTelegramEvent{
			EventID: eventID, TenantID: testTenant, ChatID: "42",
			SourceMessageID: eventID, SenderID: sender, MessageText: text, MessageType: "text",
			CreatedAt: now.Add(time.Duration(offset) * time.Minute),
		})
	}
	save("in-1", "999", "привет от клиента", 0)               // contact → incoming
	save("tg-1", "777", "личное сообщение боту", 1)           // owner, typed in Telegram
	save("out:manual:42:1", "777", "ответ из приложения", 2)  // owner, sent via the app

	server := NewServer(core.NewService(repo, nil, nil))
	res := miniappRequest(t, server, http.MethodGet, "/api/v1/chats/42/messages", "")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}

	var body struct {
		Messages []struct {
			Text      string `json:"text"`
			Direction string `json:"direction"`
			Source    string `json:"source"`
		} `json:"messages"`
	}
	decodeBody(t, res, &body)

	got := map[string]string{}
	for _, m := range body.Messages {
		got[m.Text] = m.Direction + "/" + m.Source
	}
	want := map[string]string{
		"привет от клиента":     "incoming/",
		"личное сообщение боту":  "outgoing/telegram",
		"ответ из приложения":   "outgoing/app",
	}
	for text, exp := range want {
		if got[text] != exp {
			t.Fatalf("message %q: got %q, want %q (all: %v)", text, got[text], exp, got)
		}
	}
}
