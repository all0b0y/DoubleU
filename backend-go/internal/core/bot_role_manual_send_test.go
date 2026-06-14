package core

import (
	"context"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// TestResolveRolePrefersBotRole verifies the bot's role governs every
// bot-transport chat and overrides any per-chat assignment ("и никак иначе"),
// while non-bot chats keep the legacy per-chat role.
func TestResolveRolePrefersBotRole(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42", Transport: contracts.TransportBot})
	botRole := repo.SaveAIRole(store.AIRoleRecord{TenantID: "tenant:user:77", Name: "BotRole", Description: "bd"})
	chatRole := repo.SaveAIRole(store.AIRoleRecord{TenantID: "tenant:user:77", Name: "ChatRole", Description: "cd"})
	// A stale per-chat assignment must be ignored for bot chats.
	repo.AssignAIRoleToChat("tenant:user:77", "42", chatRole.ID)
	repo.SaveUserBot(store.UserBotRecord{TenantID: "tenant:user:77", BotID: 555, Token: "t"})
	repo.SetUserBotRole("tenant:user:77", botRole.ID)

	svc := newTestService(repo, &fakeAI{}, &fakeSender{})

	role := svc.aiRoleFor("tenant:user:77", "42")
	if role == nil || role.Name != "BotRole" {
		t.Fatalf("bot chat must use the bot role, got %+v", role)
	}

	// Bot without a role → default persona, never the per-chat role.
	repo.SetUserBotRole("tenant:user:77", 0)
	if role := svc.aiRoleFor("tenant:user:77", "42"); role != nil {
		t.Fatalf("bot chat with no bot role must have no role, got %+v", role)
	}

	// A non-bot (business) chat keeps the per-chat assignment.
	repo.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "99", Transport: contracts.TransportBusiness})
	repo.AssignAIRoleToChat("tenant:user:77", "99", chatRole.ID)
	if role := svc.aiRoleFor("tenant:user:77", "99"); role == nil || role.Name != "ChatRole" {
		t.Fatalf("business chat must use its per-chat role, got %+v", role)
	}
}

// TestManualSendStoresOwnerMessageAndDiscardsDrafts verifies a manual owner
// reply goes out through the bot, is stored as an owner-authored message (AI
// context), and clears any pending draft in that chat.
func TestManualSendStoresOwnerMessageAndDiscardsDrafts(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42", Transport: contracts.TransportBot})
	repo.SaveDraft(store.DraftRecord{
		DraftID:  "d1",
		TenantID: "tenant:user:77",
		ChatID:   "42",
		Status:   store.DraftPending,
	})

	botSender := &fakeSender{}
	var events []string
	svc := NewService(repo, &fakeAI{}, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: botSender}),
		WithNotifier(func(_ string, eventType string, _ any) { events = append(events, eventType) }),
	)

	if err := svc.ManualSend(context.Background(), "tenant:user:77", "42", "ручной ответ"); err != nil {
		t.Fatalf("manual send: %v", err)
	}

	if len(botSender.sent) != 1 || botSender.sent[0].ChatID != "42" || botSender.sent[0].Text != "ручной ответ" {
		t.Fatalf("expected one bot send of the manual text, got %+v", botSender.sent)
	}

	msgs := repo.RecentMessages("tenant:user:77", "42", 10)
	found := false
	for _, m := range msgs {
		if m.Event.MessageText == "ручной ответ" && m.Event.SenderID == "77" {
			found = true
		}
	}
	if !found {
		t.Fatalf("manual reply must be stored as an owner message, got %+v", msgs)
	}

	for _, d := range repo.DraftsForChat("tenant:user:77", "42") {
		if d.DraftID == "d1" && d.Status != store.DraftRejected {
			t.Fatalf("pending draft must be discarded, status = %q", d.Status)
		}
	}

	if len(events) == 0 || events[len(events)-1] != "new_message" {
		t.Fatalf("expected a new_message notification, got %v", events)
	}
}
