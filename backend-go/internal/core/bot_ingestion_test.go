package core

import (
	"context"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// fakeBotSenders resolves every tenant to one recorded sender (or nothing).
type fakeBotSenders struct {
	sender *fakeSender
}

func (f *fakeBotSenders) SenderFor(tenantID string) (telegram.Sender, bool) {
	if f.sender == nil {
		return nil, false
	}
	return f.sender, true
}

// botUpdatePayload is a raw getUpdates message update (no business fields).
func botUpdatePayload(updateID, chatID, messageID int64, text string) []byte {
	return []byte(`{
		"update_id": ` + intString(updateID) + `,
		"message": {
			"message_id": ` + intString(messageID) + `,
			"chat": {"id": ` + intString(chatID) + `, "type": "private", "first_name": "Контакт"},
			"from": {"id": ` + intString(chatID) + `},
			"text": "` + text + `",
			"date": 1700000000
		}
	}`)
}

// botEvent normalizes a raw bot update exactly the way the poller does.
func botEvent(t *testing.T, updateID, chatID, messageID int64, text string) contracts.NormalizedTelegramEvent {
	t.Helper()
	event, ok, err := telegram.NormalizeBotUpdate(botUpdatePayload(updateID, chatID, messageID, text), 555, "tenant:user:77", time.Unix(1700000000, 0).UTC())
	if err != nil || !ok {
		t.Fatalf("normalize bot update: ok=%v err=%v", ok, err)
	}
	return event
}

// TestHandleBotMessageCreatesDraft verifies the bot transport feeds the shared
// pipeline: the chat is auto-managed with transport=bot and a manual-mode
// message produces a draft delivered to the owner via the control bot.
func TestHandleBotMessageCreatesDraft(t *testing.T) {
	repository := store.NewRepository()
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "черновик ответа",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	controlSender := &fakeSender{}
	service := newTestService(repository, ai, controlSender)

	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9001, 42, 7, "привет"))
	if err != nil {
		t.Fatalf("bot message: %v", err)
	}
	if result.Decision != contracts.DecisionDraft || result.DraftID == "" {
		t.Fatalf("expected draft decision with id, got %+v", result)
	}
	if got := repository.ChatTransport("tenant:user:77", "42"); got != contracts.TransportBot {
		t.Fatalf("chat transport = %q, want bot", got)
	}
	if ai.calls != 1 {
		t.Fatalf("ai calls = %d, want 1", ai.calls)
	}
	// The draft card goes to the owner's control-bot DM (chat id = user id from the tenant key).
	if len(controlSender.sent) != 1 || controlSender.sent[0].ChatID != "77" {
		t.Fatalf("expected one owner card to chat 77, got %+v", controlSender.sent)
	}
}

// TestHandleBotMessageAutoReplyUsesBotSender verifies the contact-facing reply
// leaves through the tenant's own bot, not the control bot.
func TestHandleBotMessageAutoReplyUsesBotSender(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeAuto})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "автоответ",
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	controlSender := &fakeSender{}
	botSender := &fakeSender{}
	service := NewService(repository, ai, controlSender,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: botSender}),
	)

	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9002, 42, 8, "вопрос"))
	if err != nil {
		t.Fatalf("bot message: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s, want auto_reply", result.Decision)
	}
	if len(botSender.sent) != 1 || botSender.sent[0].ChatID != "42" {
		t.Fatalf("expected reply through the user bot to chat 42, got %+v", botSender.sent)
	}
	if botSender.sent[0].BusinessConnectionID != "" {
		t.Fatalf("bot transport reply must not carry business_connection_id")
	}
	if len(controlSender.sent) != 0 {
		t.Fatalf("control bot must not send the contact-facing reply, got %+v", controlSender.sent)
	}
}

// TestIngestionModeGating verifies the channel switch: "bot" silences the
// Business webhook path, "personal" silences the bot path, "full" allows both.
func TestIngestionModeGating(t *testing.T) {
	draftAI := func() *fakeAI {
		return &fakeAI{response: contracts.AIProcessMessageResponse{
			Decision:     contracts.DecisionDraft,
			ResponseText: "ответ",
			Confidence:   0.9,
			RiskLevel:    contracts.RiskLow,
		}}
	}

	t.Run("bot mode skips business messages", func(t *testing.T) {
		repository := store.NewRepository()
		repository.SetTenantIngestionMode("tenant:user:77", contracts.IngestionModeBot)
		repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeManual})
		repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42"})
		ai := draftAI()
		service := newTestService(repository, ai, &fakeSender{})

		result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(9100, "bc-1", 42, 9, "hi"), "tenant:user:77")
		if err != nil {
			t.Fatalf("webhook: %v", err)
		}
		if result.Reason != "ingestion_mode_bot" || result.Decision != contracts.DecisionIgnore {
			t.Fatalf("expected ingestion_mode_bot ignore, got %+v", result)
		}
		if ai.calls != 0 {
			t.Fatalf("ai must not be called, got %d calls", ai.calls)
		}
	})

	t.Run("personal mode skips bot messages", func(t *testing.T) {
		repository := store.NewRepository()
		repository.SetTenantIngestionMode("tenant:user:77", contracts.IngestionModePersonal)
		ai := draftAI()
		service := newTestService(repository, ai, &fakeSender{})

		result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9101, 42, 10, "hi"))
		if err != nil {
			t.Fatalf("bot message: %v", err)
		}
		if result.Reason != "ingestion_mode_personal" || result.Decision != contracts.DecisionIgnore {
			t.Fatalf("expected ingestion_mode_personal ignore, got %+v", result)
		}
		if ai.calls != 0 {
			t.Fatalf("ai must not be called, got %d calls", ai.calls)
		}
	})

	t.Run("full mode processes both channels", func(t *testing.T) {
		repository := store.NewRepository()
		repository.SetTenantIngestionMode("tenant:user:77", contracts.IngestionModeFull)
		repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeManual})
		repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42"})
		ai := draftAI()
		service := newTestService(repository, ai, &fakeSender{})

		if result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9102, 42, 11, "hi")); err != nil || result.Decision != contracts.DecisionDraft {
			t.Fatalf("bot path in full mode: %+v err=%v", result, err)
		}
		if result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(9103, "bc-1", 42, 12, "hi"), "tenant:user:77"); err != nil || result.Decision != contracts.DecisionDraft {
			t.Fatalf("business path in full mode: %+v err=%v", result, err)
		}
		if ai.calls != 2 {
			t.Fatalf("ai calls = %d, want 2", ai.calls)
		}
	})
}

// TestBotMessageDuplicateScopedByBot verifies the bot receive idempotency key:
// the same update_id from the same bot is a duplicate, while another bot's
// update with an identical update_id is processed independently.
func TestBotMessageDuplicateScopedByBot(t *testing.T) {
	repository := store.NewRepository()
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "ответ",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	first, err := service.HandleBotMessage(context.Background(), botEvent(t, 9200, 42, 13, "раз"))
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	second, err := service.HandleBotMessage(context.Background(), botEvent(t, 9200, 42, 13, "раз"))
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if !second.Duplicate || second.Reason != "duplicate_receive" {
		t.Fatalf("expected duplicate_receive, got %+v", second)
	}

	otherBot, ok, err := telegram.NormalizeBotUpdate(botUpdatePayload(9200, 43, 14, "два"), 777, "tenant:user:88", time.Unix(1700000000, 0).UTC())
	if err != nil || !ok {
		t.Fatalf("normalize other bot: ok=%v err=%v", ok, err)
	}
	third, err := service.HandleBotMessage(context.Background(), otherBot)
	if err != nil {
		t.Fatalf("other bot: %v", err)
	}
	if third.Duplicate {
		t.Fatalf("same update_id from another bot must not be a duplicate: %+v", third)
	}
}

// TestApprovalApproveSendsThroughBotTransport verifies the owner's approve on
// a bot-transport draft routes the reply through the user bot even though the
// callback path reconstructs a minimal event without transport metadata.
func TestApprovalApproveSendsThroughBotTransport(t *testing.T) {
	repository := store.NewRepository()
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "черновик",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	controlSender := &fakeSender{}
	botSender := &fakeSender{}
	service := NewService(repository, ai, controlSender,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: botSender}),
	)

	inbound, err := service.HandleBotMessage(context.Background(), botEvent(t, 9300, 42, 15, "вопрос"))
	if err != nil || inbound.DraftID == "" {
		t.Fatalf("bot message: %+v err=%v", inbound, err)
	}
	_, err = service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
		Action:          contracts.ApprovalApprove,
		TenantID:        "tenant:user:77",
		ChatID:          "42",
		SourceMessageID: "15",
		DraftID:         inbound.DraftID,
		DecisionID:      inbound.Event.EventID,
		IdempotencyKey:  "approve:9300",
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(botSender.sent) != 1 || botSender.sent[0].ChatID != "42" {
		t.Fatalf("approved reply must go through the user bot to chat 42, got %+v", botSender.sent)
	}
}
