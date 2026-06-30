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
	save("in-1", "999", "привет от клиента", 0)              // contact → incoming
	save("tg-1", "777", "личное сообщение боту", 1)          // owner, typed in Telegram
	save("out:manual:42:1", "777", "ответ из приложения", 2) // owner, sent via the app

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
		"личное сообщение боту": "outgoing/telegram",
		"ответ из приложения":   "outgoing/app",
	}
	for text, exp := range want {
		if got[text] != exp {
			t.Fatalf("message %q: got %q, want %q (all: %v)", text, got[text], exp, got)
		}
	}
}

// TestMessagesDirectionBotTransport verifies that on bot transport the bot is a
// separate identity: a message the owner typed straight to their own bot is
// INCOMING (the owner is just a user of the bot), while only system-emitted
// "out:" replies are outgoing. This is the display side of letting the AI answer
// the owner's DMs to their own bot.
func TestMessagesDirectionBotTransport(t *testing.T) {
	repo := store.NewRepository()
	chatID := "bot:555:777"
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: chatID, DisplayName: "Я", Transport: contracts.TransportBot})
	now := time.Unix(1700000000, 0).UTC()

	save := func(eventID, sender, text string, offset int) {
		repo.SaveMessage(contracts.NormalizedTelegramEvent{
			EventID: eventID, TenantID: testTenant, ChatID: chatID,
			SourceMessageID: eventID, SenderID: sender, MessageText: text, MessageType: "text",
			CreatedAt: now.Add(time.Duration(offset) * time.Minute),
		})
	}
	// Owner (777) DMs their own bot — sender is the owner, but no "out:" prefix.
	save("in-owner", "777", "тест от владельца", 0)
	// The bot's own AI reply, persisted as a synthetic outgoing message.
	save("out:auto:1", "777", "ответ бота", 1)

	server := NewServer(core.NewService(repo, nil, nil))
	res := miniappRequest(t, server, http.MethodGet, "/api/v1/chats/"+chatID+"/messages", "")
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
		"тест от владельца": "incoming/",
		"ответ бота":        "outgoing/app",
	}
	for text, exp := range want {
		if got[text] != exp {
			t.Fatalf("message %q: got %q, want %q (all: %v)", text, got[text], exp, got)
		}
	}
}

func TestMessagesHistoryIncludesAllBotRoleContexts(t *testing.T) {
	repo := store.NewRepository()
	const botID int64 = 555
	chatID := contracts.BotChatID("555", "777")
	repo.SaveUserBot(store.UserBotRecord{TenantID: testTenant, BotID: botID, BotUsername: "helper_bot", Token: "secret"})
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: chatID, DisplayName: "Я", Transport: contracts.TransportBot})
	now := time.Unix(1700000000, 0).UTC()

	ctxA, err := repo.SetUserBotRoleWithContextPolicy(testTenant, botID, 10, store.RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("activate role A: %v", err)
	}
	repo.SaveMessageWithRoleContext(contracts.NormalizedTelegramEvent{
		EventID:         "role-a-message",
		TenantID:        testTenant,
		ChatID:          chatID,
		SourceMessageID: "role-a-message",
		SenderID:        "777",
		MessageText:     "сообщение из памяти роли A",
		MessageType:     "text",
		CreatedAt:       now,
		Transport:       contracts.TransportBot,
		BotID:           "555",
	}, ctxA.ID)

	ctxB, err := repo.SetUserBotRoleWithContextPolicy(testTenant, botID, 11, store.RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("activate role B: %v", err)
	}
	repo.SaveMessageWithRoleContext(contracts.NormalizedTelegramEvent{
		EventID:         "role-b-message",
		TenantID:        testTenant,
		ChatID:          chatID,
		SourceMessageID: "role-b-message",
		SenderID:        "777",
		MessageText:     "сообщение из памяти роли B",
		MessageType:     "text",
		CreatedAt:       now.Add(time.Minute),
		Transport:       contracts.TransportBot,
		BotID:           "555",
	}, ctxB.ID)

	server := NewServer(core.NewService(repo, nil, nil))
	res := miniappRequest(t, server, http.MethodGet, "/api/v1/chats/"+chatID+"/messages", "")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}

	var body struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	decodeBody(t, res, &body)

	got := map[string]bool{}
	for _, m := range body.Messages {
		got[m.Text] = true
	}
	for _, text := range []string{"сообщение из памяти роли A", "сообщение из памяти роли B"} {
		if !got[text] {
			t.Fatalf("history missing %q; got %+v", text, got)
		}
	}
}
