package core

import (
	"context"
	"strings"
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

func (f *fakeBotSenders) SenderFor(tenantID, botID string) (telegram.Sender, bool) {
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
	// The chat is stored under its bot-namespaced id.
	if got := repository.ChatTransport("tenant:user:77", contracts.BotChatID("555", "42")); got != contracts.TransportBot {
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
	// Auto mode is configured on the bot-namespaced chat id.
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: contracts.BotChatID("555", "42"), Mode: contracts.ChatModeAuto})
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

// A role configured to speak as an open bot must answer a low-risk DM even
// when the new bot-scoped chat still has the conservative manual default.
func TestBotPersonaManualDefaultAutoRepliesLowRisk(t *testing.T) {
	repository := store.NewRepository()
	role := repository.SaveAIRole(store.AIRoleRecord{
		TenantID:     "tenant:user:77",
		Name:         "Tutor",
		SpeakAsOwner: false,
	})
	repository.SaveUserBot(store.UserBotRecord{
		TenantID: "tenant:user:77",
		BotID:    555,
		Status:   "active",
		AIRoleID: role.ID,
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "Прямой ответ бота",
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

	result, err := service.HandleBotMessage(
		context.Background(), botEvent(t, 9003, 42, 9, "объясни тему"),
	)
	if err != nil {
		t.Fatalf("bot message: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply || result.Reason != "bot_persona_low_risk_send" {
		t.Fatalf("result = %+v, want bot persona auto reply", result)
	}
	if ai.lastRequest.ChatSettings.Mode != contracts.ChatModeAuto {
		t.Fatalf("AI effective mode = %q, want auto", ai.lastRequest.ChatSettings.Mode)
	}
	if len(botSender.sent) != 1 || botSender.sent[0].ChatID != "42" {
		t.Fatalf("bot replies = %+v, want one reply to chat 42", botSender.sent)
	}
	if len(controlSender.sent) != 0 {
		t.Fatalf("control bot must not receive a draft card: %+v", controlSender.sent)
	}
}

// TestTwoBotsSameUserAreIsolated is the multi-bot isolation contract: when a
// tenant's two bots both talk to the SAME Telegram user, the bot-namespaced
// chat ids keep their messages and drafts completely separate — no bleed.
func TestTwoBotsSameUserAreIsolated(t *testing.T) {
	repository := store.NewRepository()
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision: contracts.DecisionDraft, ResponseText: "ответ", Confidence: 0.9, RiskLevel: contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	at := time.Unix(1700000000, 0).UTC()
	ev1, _, _ := telegram.NormalizeBotUpdate(botUpdatePayload(9001, 42, 7, "к боту А"), 555, "tenant:user:77", at)
	ev2, _, _ := telegram.NormalizeBotUpdate(botUpdatePayload(9002, 42, 8, "к боту Б"), 666, "tenant:user:77", at)
	if _, err := service.HandleBotMessage(context.Background(), ev1); err != nil {
		t.Fatalf("bot A message: %v", err)
	}
	if _, err := service.HandleBotMessage(context.Background(), ev2); err != nil {
		t.Fatalf("bot B message: %v", err)
	}

	chatA := contracts.BotChatID("555", "42")
	chatB := contracts.BotChatID("666", "42")
	if chatA == chatB {
		t.Fatal("two bots talking to the same user must yield distinct chat ids")
	}

	if msgs := repository.RecentMessages("tenant:user:77", chatA, 10); len(msgs) != 1 || msgs[0].Event.MessageText != "к боту А" {
		t.Fatalf("chat A must hold only bot A's message, got %+v", msgs)
	}
	if msgs := repository.RecentMessages("tenant:user:77", chatB, 10); len(msgs) != 1 || msgs[0].Event.MessageText != "к боту Б" {
		t.Fatalf("chat B must hold only bot B's message, got %+v", msgs)
	}
	if d := repository.DraftsForChat("tenant:user:77", chatA); len(d) != 1 {
		t.Fatalf("chat A drafts = %d, want 1", len(d))
	}
	if d := repository.DraftsForChat("tenant:user:77", chatB); len(d) != 1 {
		t.Fatalf("chat B drafts = %d, want 1", len(d))
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

// TestOwnerDMToOwnBotGetsAIReply is the bot-transport counterpart to
// TestOwnerBusinessMessageIsStoredWithoutAIReply: when the owner DMs their OWN
// bot (the bot is a separate identity, so the owner is just a user testing it),
// the message must NOT be suppressed as owner-authored — the AI answers it like
// any contact. The owner's Telegram id (77) equals the chat/sender id here.
func TestOwnerDMToOwnBotGetsAIReply(t *testing.T) {
	repository := store.NewRepository()
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "черновик ответа",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	// from.id == chat.id == 77 == the owner's own Telegram id (tenant:user:77).
	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 9400, 77, 16, "привет, бот"))
	if err != nil {
		t.Fatalf("bot message: %v", err)
	}
	if result.Reason == "owner_message_observed" || result.Decision == contracts.DecisionObserve {
		t.Fatalf("owner DM to own bot must not be observed as owner-authored, got %+v", result)
	}
	if ai.calls != 1 {
		t.Fatalf("ai must answer the owner's DM to the bot, calls = %d", ai.calls)
	}
	if result.DraftID == "" {
		t.Fatalf("expected a reply draft for the owner's DM, got %+v", result)
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

// newQuestionnaireService wires a bot-transport service whose role enables the
// ask_questions tool and whose AI replies with a two-question questionnaire.
func newQuestionnaireService(t *testing.T) (*Service, *store.Repository, *fakeAI, *fakeSender) {
	t.Helper()
	repository := store.NewRepository()
	role := repository.SaveAIRole(store.AIRoleRecord{
		TenantID: "tenant:user:77", Name: "Interactive",
		ToolSettings: map[string]bool{"interaction.ask_questions": true},
	})
	repository.SaveUserBot(store.UserBotRecord{
		TenantID: "tenant:user:77", BotID: 555, Status: "active", AIRoleID: role.ID,
	})
	chatID := contracts.BotChatID("555", "42")
	repository.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: chatID, Mode: contracts.ChatModeManual,
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision: contracts.DecisionAutoReply, ResponseText: "Когда удобно?",
		Confidence: 0.9, RiskLevel: contracts.RiskLow, Intent: "choice_question",
		ResponsePlan: &contracts.ResponsePlan{Shape: "questions", Questions: []contracts.Question{
			{Prompt: "Когда удобно?", Options: []string{"Сегодня", "Завтра"}},
			{Prompt: "Какой бюджет?"},
		}},
	}}
	botSender := &fakeSender{}
	service := NewService(repository, ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: botSender}),
	)
	return service, repository, ai, botSender
}

// TestQuestionnaireCollectsAnswersThenRunsAITurn covers the full step-by-step
// flow: the first question is sent directly to the contact, a button answer
// advances to the next question, a free-text answer completes the form, and only
// then does a single AI turn run with both answers in its message.
func TestQuestionnaireCollectsAnswersThenRunsAITurn(t *testing.T) {
	service, repository, ai, botSender := newQuestionnaireService(t)
	chatID := contracts.BotChatID("555", "42")

	inbound, err := service.HandleBotMessage(context.Background(), botEvent(t, 9600, 42, 21, "Назначим звонок"))
	if err != nil || inbound.Reason != "form_started" {
		t.Fatalf("questionnaire start: %+v err=%v", inbound, err)
	}
	if ai.calls != 1 || len(botSender.sent) != 1 {
		t.Fatalf("first question: ai.calls=%d sends=%+v", ai.calls, botSender.sent)
	}
	form, ok := repository.ActiveInteractionFormAt("tenant:user:77", chatID, service.now())
	if !ok || form.CurrentIndex != 0 || len(form.Questions) != 2 {
		t.Fatalf("active form = %+v ok=%v", form, ok)
	}
	markup, ok := botSender.sent[0].ReplyMarkup.(telegram.InlineKeyboardMarkup)
	if !ok || len(markup.InlineKeyboard) != 3 { // 2 options + free-text button
		t.Fatalf("question 1 markup = %#v", botSender.sent[0].ReplyMarkup)
	}
	if markup.InlineKeyboard[2][0].Text != "✏️" {
		t.Fatalf("free-text button must be language-neutral, got %q", markup.InlineKeyboard[2][0].Text)
	}

	// Answer question 1 by tapping the second option ("Завтра").
	callback := telegram.CallbackQueryUpdate{
		UpdateID: 9601, QueryID: "query-1", Data: markup.InlineKeyboard[1][0].CallbackData,
		SenderID: "42", ChatID: "42", OriginalMsgID: "100",
		ReceiveIdempotencyKey: "9601:callback_query:query-1",
	}
	res, err := service.HandleBotCallbackQuery(context.Background(), callback, "tenant:user:77", "555")
	if err != nil || res.Reason != "form_question_sent" {
		t.Fatalf("answer q1: %+v err=%v", res, err)
	}
	if len(botSender.edits) != 1 || strings.Contains(botSender.edits[0].text, "Выбрано") {
		t.Fatalf("selected option edit must avoid hardcoded language, edits=%+v", botSender.edits)
	}
	if ai.calls != 1 || len(botSender.sent) != 2 {
		t.Fatalf("q1 answered should send q2 without an AI turn: ai.calls=%d sends=%d", ai.calls, len(botSender.sent))
	}

	// Answer question 2 by free text — this completes the form and runs the turn.
	ai.response = contracts.AIProcessMessageResponse{
		Decision: contracts.DecisionAutoReply, ResponseText: "Принято, до завтра.",
		Confidence: 0.99, RiskLevel: contracts.RiskLow, Intent: "acknowledgement",
	}
	done, err := service.HandleBotMessage(context.Background(), botEvent(t, 9602, 42, 22, "До 100 тысяч"))
	if err != nil {
		t.Fatalf("free-text answer: %v", err)
	}
	if ai.calls != 2 {
		t.Fatalf("completion must run exactly one AI turn, ai.calls=%d", ai.calls)
	}
	if !strings.Contains(ai.lastRequest.Event.MessageText, "Завтра") ||
		!strings.Contains(ai.lastRequest.Event.MessageText, "До 100 тысяч") {
		t.Fatalf("final turn message missing answers: %q", ai.lastRequest.Event.MessageText)
	}
	if _, stillActive := repository.ActiveInteractionFormAt("tenant:user:77", chatID, service.now()); stillActive {
		t.Fatalf("form should be completed after the last answer")
	}
	if done.Decision != contracts.DecisionAutoReply {
		t.Fatalf("completion turn decision = %+v", done)
	}
}

func TestQuestionnaireFreeTextPromptUsesQuestionText(t *testing.T) {
	service, _, _, botSender := newQuestionnaireService(t)

	if _, err := service.HandleBotMessage(context.Background(), botEvent(t, 9610, 42, 31, "Назначим звонок")); err != nil {
		t.Fatalf("start: %v", err)
	}
	markup := botSender.sent[0].ReplyMarkup.(telegram.InlineKeyboardMarkup)
	callback := telegram.CallbackQueryUpdate{
		UpdateID: 9611, QueryID: "query-free-text", Data: markup.InlineKeyboard[2][0].CallbackData,
		SenderID: "42", ChatID: "42", OriginalMsgID: "100",
		ReceiveIdempotencyKey: "9611:callback_query:query-free-text",
	}
	res, err := service.HandleBotCallbackQuery(context.Background(), callback, "tenant:user:77", "555")
	if err != nil || res.Reason != "form_free_text_requested" {
		t.Fatalf("free text tap = %+v err=%v", res, err)
	}
	if len(botSender.sent) != 2 || botSender.sent[1].Text != "✏️\nКогда удобно?" {
		t.Fatalf("free text prompt must reuse the question text, sent=%+v", botSender.sent)
	}
}

// TestQuestionnaireButtonIsIdempotent verifies a double-tap on the same option
// neither double-advances the form nor triggers a second collection step.
func TestQuestionnaireButtonIsIdempotent(t *testing.T) {
	service, repository, ai, botSender := newQuestionnaireService(t)
	chatID := contracts.BotChatID("555", "42")

	if _, err := service.HandleBotMessage(context.Background(), botEvent(t, 9600, 42, 21, "Когда?")); err != nil {
		t.Fatalf("start: %v", err)
	}
	markup := botSender.sent[0].ReplyMarkup.(telegram.InlineKeyboardMarkup)
	callback := telegram.CallbackQueryUpdate{
		UpdateID: 9601, QueryID: "query-1", Data: markup.InlineKeyboard[0][0].CallbackData,
		SenderID: "42", ChatID: "42", OriginalMsgID: "100",
		ReceiveIdempotencyKey: "9601:callback_query:query-1",
	}
	if _, err := service.HandleBotCallbackQuery(context.Background(), callback, "tenant:user:77", "555"); err != nil {
		t.Fatalf("first tap: %v", err)
	}
	form, _ := repository.ActiveInteractionFormAt("tenant:user:77", chatID, service.now())
	if form.CurrentIndex != 1 {
		t.Fatalf("after one answer CurrentIndex = %d, want 1", form.CurrentIndex)
	}

	callback.UpdateID = 9602
	callback.QueryID = "query-2"
	callback.ReceiveIdempotencyKey = "9602:callback_query:query-2"
	dup, err := service.HandleBotCallbackQuery(context.Background(), callback, "tenant:user:77", "555")
	if err != nil || dup.Reason != "duplicate_qform_callback" {
		t.Fatalf("double tap = %+v err=%v", dup, err)
	}
	form, _ = repository.ActiveInteractionFormAt("tenant:user:77", chatID, service.now())
	if form.CurrentIndex != 1 {
		t.Fatalf("double tap advanced the form to %d", form.CurrentIndex)
	}
	if ai.calls != 1 {
		t.Fatalf("double tap triggered an AI turn: %d", ai.calls)
	}
}
