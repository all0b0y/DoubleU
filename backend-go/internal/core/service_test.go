package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func testBoolPtr(value bool) *bool {
	return &value
}

func TestDecisionEngineContractCombinations(t *testing.T) {
	engine := NewDecisionEngine()
	decisions := []contracts.Decision{
		contracts.DecisionDraft,
		contracts.DecisionAutoReply,
		contracts.DecisionIgnore,
		contracts.DecisionObserve,
		contracts.DecisionAskOwner,
		contracts.DecisionErrorFallback,
	}
	modes := []contracts.ChatMode{
		contracts.ChatModeDisabled,
		contracts.ChatModeSilent,
		contracts.ChatModeManual,
		contracts.ChatModeSemiAuto,
		contracts.ChatModeAuto,
	}
	privacySets := []contracts.PrivacyFlags{
		{},
		{contracts.PrivacyNoMemory},
		{contracts.PrivacyNoTraining},
		{contracts.PrivacyPrivate},
		{contracts.PrivacyNoSummary},
	}
	risks := []contracts.RiskLevel{
		contracts.RiskLow,
		contracts.RiskMedium,
		contracts.RiskHigh,
		contracts.RiskCritical,
	}

	for _, proposed := range decisions {
		for _, mode := range modes {
			for _, privacy := range privacySets {
				for _, risk := range risks {
					settings := contracts.ChatSettings{TenantID: "t", ChatID: "c", Mode: mode, PrivacyFlags: privacy}
					response := contracts.AIProcessMessageResponse{
						Decision:     proposed,
						ResponseText: "ok",
						Confidence:   0.99,
						RiskLevel:    risk,
					}
					final, reason := engine.Decide(settings, response)
					if !final.IsValid() {
						t.Fatalf("invalid final decision for proposed=%s mode=%s privacy=%v risk=%s reason=%s", proposed, mode, privacy, risk, reason)
					}
					if final == contracts.DecisionAutoReply {
						if risk != contracts.RiskLow {
							t.Fatalf("auto_reply allowed for non-low risk: %s", risk)
						}
						if mode != contracts.ChatModeSemiAuto && mode != contracts.ChatModeAuto {
							t.Fatalf("auto_reply allowed for mode %s", mode)
						}
						if privacy.Contains(contracts.PrivacyPrivate) {
							t.Fatalf("auto_reply allowed for private chat")
						}
						// SEMI_AUTO still defers to the AI's auto-reply request; AUTO
						// does not — the owner accepted unsupervised replies, so a
						// low-risk message auto-sends regardless of the AI proposal.
						if mode == contracts.ChatModeSemiAuto && proposed != contracts.DecisionAutoReply {
							t.Fatalf("semi-auto auto_reply allowed without AI auto proposal")
						}
					}
					if risk == contracts.RiskCritical && proposed != contracts.DecisionIgnore && proposed != contracts.DecisionErrorFallback && mode != contracts.ChatModeDisabled && mode != contracts.ChatModeSilent {
						if final != contracts.DecisionAskOwner {
							t.Fatalf("critical risk should ask owner, got %s", final)
						}
					}
				}
			}
		}
	}
}

func TestHandleTelegramWebhookAutoReplyAndDuplicateAudit(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "hello",
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	first, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1001, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("first webhook: %v", err)
	}
	second, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1001, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("duplicate webhook: %v", err)
	}

	if first.Decision != contracts.DecisionAutoReply {
		t.Fatalf("first decision = %s", first.Decision)
	}
	if !second.Duplicate || second.Decision != contracts.DecisionIgnore {
		t.Fatalf("duplicate result = %+v", second)
	}
	if ai.calls != 1 {
		t.Fatalf("ai calls = %d", ai.calls)
	}
	if sender.calls != 1 {
		t.Fatalf("sender calls = %d", sender.calls)
	}
	if sender.richCalls != 0 {
		t.Fatalf("rich sender calls = %d, want 0", sender.richCalls)
	}
	if len(sender.sent) != 1 || sender.sent[0].ParseMode != "" {
		t.Fatalf("plain auto reply must not set parse_mode, sent = %+v", sender.sent)
	}
	audits := repository.Audits()
	if len(audits) != 2 {
		t.Fatalf("audit count = %d", len(audits))
	}
	if audits[1].Reason != "duplicate_receive" {
		t.Fatalf("duplicate audit reason = %s", audits[1].Reason)
	}
}

// TestHandleTelegramWebhookSendsAssignedAIRole verifies the role contract:
// the persona assigned to the chat reaches the Python AI service on every
// process-message request, and an unassigned chat sends no role at all.
func TestHandleTelegramWebhookSendsAssignedAIRole(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	role := repository.SaveAIRole(store.AIRoleRecord{
		TenantID:    "tenant-1",
		Name:        "Техподдержка",
		Description: "Вежливый сотрудник поддержки, отвечает от лица владельца",
		Constraints: "не обещать возвраты",
	})
	repository.AssignAIRoleToChat("tenant-1", "42", role.ID)
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "hello",
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	if _, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1003, "bc-1", 42, 7, "hi"), "tenant-1"); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	got := ai.lastRequest.AIRole
	if got == nil || got.Name != "Техподдержка" || got.Constraints != "не обещать возвраты" {
		t.Fatalf("ai_role in request = %+v", got)
	}

	// A chat without an assigned role must not carry a persona.
	repository.AssignAIRoleToChat("tenant-1", "42", 0)
	if _, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1004, "bc-1", 42, 8, "ещё вопрос"), "tenant-1"); err != nil {
		t.Fatalf("second webhook: %v", err)
	}
	if ai.lastRequest.AIRole != nil {
		t.Fatalf("expected no ai_role after clear, got %+v", ai.lastRequest.AIRole)
	}
}

func TestDeliverySettingsResolveDefaultOwnerRoleBotScopes(t *testing.T) {
	repository := store.NewRepository()
	service := newTestService(repository, &fakeAI{}, &fakeSender{})

	defaults := service.deliverySettingsFor(contracts.NormalizedTelegramEvent{TenantID: "tenant-1", ChatID: "42"})
	if !defaults.RichFormattingEnabled || defaults.StreamingEnabled {
		t.Fatalf("default delivery settings = %+v", defaults)
	}

	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:               "tenant-1",
		RichFormattingEnabled:  true,
		StreamingEnabled:       true,
		DefaultModeForNewChats: "manual",
		LearningEnabled:        true,
		LanguageForDrafts:      "auto",
	})
	role := repository.SaveAIRole(store.AIRoleRecord{
		TenantID:              "tenant-1",
		Name:                  "Техподдержка",
		Description:           "d",
		RichFormattingEnabled: testBoolPtr(false),
		StreamingEnabled:      testBoolPtr(false),
	})
	repository.AssignAIRoleToChat("tenant-1", "42", role.ID)

	roleScoped := service.deliverySettingsFor(contracts.NormalizedTelegramEvent{TenantID: "tenant-1", ChatID: "42"})
	if roleScoped.RichFormattingEnabled || roleScoped.StreamingEnabled {
		t.Fatalf("role-scoped delivery settings = %+v", roleScoped)
	}

	botChatID := contracts.BotChatID("555", "42")
	repository.SaveUserBot(store.UserBotRecord{
		TenantID:              "tenant-1",
		BotID:                 555,
		BotUsername:           "helper",
		Token:                 "secret",
		AIRoleID:              role.ID,
		RichFormattingEnabled: testBoolPtr(true),
		StreamingEnabled:      testBoolPtr(true),
	})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: botChatID, Transport: contracts.TransportBot})

	botScoped := service.deliverySettingsFor(contracts.NormalizedTelegramEvent{
		TenantID: "tenant-1",
		ChatID:   botChatID,
		BotID:    "555",
	})
	if !botScoped.RichFormattingEnabled || !botScoped.StreamingEnabled {
		t.Fatalf("bot-scoped delivery settings = %+v", botScoped)
	}
}

func TestHandleTelegramWebhookRichAutoReplyUsesRichSend(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "# План\n\n- A\n- B",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1006, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s", result.Decision)
	}
	if sender.richCalls != 1 || sender.calls != 0 {
		t.Fatalf("rich calls=%d plain calls=%d", sender.richCalls, sender.calls)
	}
	if len(sender.richDrafts) != 0 {
		t.Fatalf("rich drafts = %+v, want none", sender.richDrafts)
	}
	if got := sender.richSent[0].Text; got != "# План\n\n- A\n- B" {
		t.Fatalf("rich text = %q", got)
	}
}

func TestHandleBotMessageRichStreamingSendsDraftsThenFinal(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: contracts.BotChatID("555", "42"), Mode: contracts.ChatModeAuto})
	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:               "tenant:user:77",
		RichFormattingEnabled:  true,
		StreamingEnabled:       true,
		DefaultModeForNewChats: "manual",
		LearningEnabled:        true,
		LanguageForDrafts:      "auto",
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "## Финал\n\n- безопасный текст",
		TextFormat:   contracts.TextFormatRich,
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

	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 1012, 42, 7, "hi"))
	if err != nil {
		t.Fatalf("bot message: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s", result.Decision)
	}
	if botSender.richCalls != 1 || len(botSender.richDrafts) != 2 || botSender.calls != 0 {
		t.Fatalf("rich calls=%d rich drafts=%d plain calls=%d", botSender.richCalls, len(botSender.richDrafts), botSender.calls)
	}
	if botSender.richDrafts[0].markdown != richStreamingThinkingMarkdown {
		t.Fatalf("thinking markdown = %q", botSender.richDrafts[0].markdown)
	}
	if botSender.richDrafts[1].markdown != "## Финал\n\n- безопасный текст" {
		t.Fatalf("final draft markdown = %q", botSender.richDrafts[1].markdown)
	}
	if botSender.richDrafts[0].draftID == 0 || botSender.richDrafts[0].draftID != botSender.richDrafts[1].draftID {
		t.Fatalf("draft ids = %+v", botSender.richDrafts)
	}
	if botSender.richDrafts[0].chatID != "42" || botSender.richDrafts[1].chatID != "42" {
		t.Fatalf("draft chat ids = %+v", botSender.richDrafts)
	}
	if botSender.richSent[0].Text != "## Финал\n\n- безопасный текст" {
		t.Fatalf("final rich text = %q", botSender.richSent[0].Text)
	}

	_, err = service.HandleBotMessage(context.Background(), botEvent(t, 1012, 42, 7, "hi"))
	if err != nil {
		t.Fatalf("duplicate bot message: %v", err)
	}
	if botSender.richCalls != 1 || len(botSender.richDrafts) != 2 {
		t.Fatalf("duplicate must not send again: rich calls=%d drafts=%d", botSender.richCalls, len(botSender.richDrafts))
	}
}

func TestHandleBotMessageRichStreamingDraftFailureStillSendsFinalText(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: contracts.BotChatID("555", "42"), Mode: contracts.ChatModeAuto})
	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:               "tenant:user:77",
		RichFormattingEnabled:  true,
		StreamingEnabled:       true,
		DefaultModeForNewChats: "manual",
		LearningEnabled:        true,
		LanguageForDrafts:      "auto",
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "## Финал",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	controlSender := &fakeSender{}
	botSender := &fakeSender{richDraftErr: errors.New("draft failed")}
	service := NewService(repository, ai, controlSender,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: botSender}),
	)

	result, err := service.HandleBotMessage(context.Background(), botEvent(t, 1013, 42, 7, "hi"))
	if err != nil {
		t.Fatalf("bot message: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s", result.Decision)
	}
	if botSender.richCalls != 1 || len(botSender.richDrafts) != 1 || botSender.calls != 0 {
		t.Fatalf("rich calls=%d rich drafts=%d plain calls=%d", botSender.richCalls, len(botSender.richDrafts), botSender.calls)
	}
	if botSender.richDrafts[0].markdown != richStreamingThinkingMarkdown {
		t.Fatalf("draft markdown = %q", botSender.richDrafts[0].markdown)
	}
	if botSender.richSent[0].Text != "## Финал" {
		t.Fatalf("final rich text = %+v", botSender.richSent)
	}
}

func TestHandleTelegramWebhookRichStreamingNeverRunsForDigitalCopy(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:               "tenant-1",
		RichFormattingEnabled:  true,
		StreamingEnabled:       true,
		DefaultModeForNewChats: "manual",
		LearningEnabled:        true,
		LanguageForDrafts:      "auto",
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "## Rich but no role",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1014, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s", result.Decision)
	}
	if sender.richCalls != 1 || len(sender.richDrafts) != 0 {
		t.Fatalf("digital copy must use phase-1 rich send without streaming: rich calls=%d drafts=%d", sender.richCalls, len(sender.richDrafts))
	}
	if sender.richSent[0].Text != "## Rich but no role" {
		t.Fatalf("rich text = %q", sender.richSent[0].Text)
	}
}

func TestHandleTelegramWebhookRichSendFallsBackToPlain(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "Use <tag> & table",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{richErr: errors.New("rich unavailable")}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1007, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s", result.Decision)
	}
	if sender.richCalls != 1 || sender.calls != 1 {
		t.Fatalf("rich calls=%d fallback calls=%d", sender.richCalls, sender.calls)
	}
	if sender.sent[0].ParseMode != "" {
		t.Fatalf("fallback parse_mode = %q", sender.sent[0].ParseMode)
	}
	if sender.sent[0].Text != "Use <tag> & table" {
		t.Fatalf("fallback text = %q", sender.sent[0].Text)
	}
}

func TestHandleTelegramWebhookRichFormattingToggleOffForcesPlainSend(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:               "tenant-1",
		RichFormattingEnabled:  false,
		DefaultModeForNewChats: "manual",
		LearningEnabled:        true,
		LanguageForDrafts:      "auto",
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "# План\n\n- A\n- B",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1009, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("decision = %s", result.Decision)
	}
	if sender.richCalls != 0 || sender.calls != 1 {
		t.Fatalf("rich calls=%d plain calls=%d", sender.richCalls, sender.calls)
	}
	if sender.sent[0].ParseMode != "" || sender.sent[0].Text != "# План\n\n- A\n- B" {
		t.Fatalf("plain fallback send = %+v", sender.sent[0])
	}
}

func TestHandleTelegramWebhookRichDraftCardUsesRichSend(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: "bc-1",
		TenantID:     "tenant-1",
		UserID:       "77",
		UserChatID:   "77",
		CanReply:     true,
		IsEnabled:    true,
		UpdatedAt:    time.Unix(1700000000, 0).UTC(),
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "## Ответ\n\n| A | B |\n|---|---|",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1010, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionDraft {
		t.Fatalf("decision = %s", result.Decision)
	}
	if sender.richCalls != 1 || sender.calls != 0 {
		t.Fatalf("owner card rich calls=%d plain calls=%d", sender.richCalls, sender.calls)
	}
	card := sender.richSent[0]
	if card.ChatID != "77" || !strings.Contains(card.Text, "## Ответ") {
		t.Fatalf("rich owner card = %+v", card)
	}
	if markup, ok := card.ReplyMarkup.(telegram.InlineKeyboardMarkup); !ok || len(markup.InlineKeyboard) == 0 {
		t.Fatalf("rich owner card must preserve inline keyboard, markup=%+v", card.ReplyMarkup)
	}
	draft, ok := repository.Draft(result.DraftID)
	if !ok || draft.OwnerChatID != "77" || draft.OwnerMessageID == "" {
		t.Fatalf("draft owner card ids = %+v ok=%v", draft, ok)
	}
}

func TestHandleTelegramWebhookRichDraftCardToggleOffUsesPlainSend(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:               "tenant-1",
		RichFormattingEnabled:  false,
		DefaultModeForNewChats: "manual",
		LearningEnabled:        true,
		LanguageForDrafts:      "auto",
	})
	repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: "bc-1",
		TenantID:     "tenant-1",
		UserID:       "77",
		UserChatID:   "77",
		CanReply:     true,
		IsEnabled:    true,
		UpdatedAt:    time.Unix(1700000000, 0).UTC(),
	})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "## Ответ\n\n- A\n- B",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1011, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionDraft {
		t.Fatalf("decision = %s", result.Decision)
	}
	if sender.richCalls != 0 || sender.calls != 1 {
		t.Fatalf("owner card rich calls=%d plain calls=%d", sender.richCalls, sender.calls)
	}
	card := sender.sent[0]
	if card.ParseMode != "" || !strings.Contains(card.Text, "## Ответ") {
		t.Fatalf("plain owner card = %+v", card)
	}
	if markup, ok := card.ReplyMarkup.(telegram.InlineKeyboardMarkup); !ok || len(markup.InlineKeyboard) == 0 {
		t.Fatalf("plain owner card must preserve inline keyboard, markup=%+v", card.ReplyMarkup)
	}
}

func TestHandleTelegramWebhookDisabledAuditsWithoutAI(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeDisabled})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{}
	service := newTestService(repository, ai, &fakeSender{})

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1002, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionIgnore || result.Reason != "chat_disabled" {
		t.Fatalf("result = %+v", result)
	}
	if ai.calls != 0 {
		t.Fatalf("ai calls = %d", ai.calls)
	}
	audits := repository.Audits()
	if len(audits) != 1 || audits[0].Decision != contracts.DecisionIgnore {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestHandleTelegramWebhookNoMemoryDoesNotRetainMessage(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{
		TenantID:     "tenant-1",
		ChatID:       "42",
		Mode:         contracts.ChatModeManual,
		PrivacyFlags: contracts.PrivacyFlags{contracts.PrivacyNoMemory},
	})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "draft",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1003, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionDraft {
		t.Fatalf("decision = %s", result.Decision)
	}
	if _, ok := repository.Message("tenant-1", "42", "7"); ok {
		t.Fatalf("no_memory retained message content")
	}
}

func TestHandleTelegramWebhookAIErrorAuditsFallback(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	service := newTestService(repository, &fakeAI{err: errors.New("boom")}, &fakeSender{})

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1004, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if result.Decision != contracts.DecisionErrorFallback {
		t.Fatalf("decision = %s", result.Decision)
	}
	audits := repository.Audits()
	if len(audits) != 1 || !strings.HasPrefix(audits[0].Reason, "ai_process_error:") {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestApprovalApproveIsIdempotent(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "draft",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1005, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	payload := contracts.ApprovalCallbackPayload{
		Action:          contracts.ApprovalApprove,
		TenantID:        "tenant-1",
		ChatID:          "42",
		SourceMessageID: "7",
		DraftID:         inbound.DraftID,
		DecisionID:      inbound.Event.EventID,
		IdempotencyKey:  "callback-1",
	}
	first, err := service.HandleApprovalCallback(context.Background(), payload)
	if err != nil {
		t.Fatalf("first approval: %v", err)
	}
	second, err := service.HandleApprovalCallback(context.Background(), payload)
	if err != nil {
		t.Fatalf("duplicate approval: %v", err)
	}
	if first.Status != string(store.DraftSent) {
		t.Fatalf("first status = %s", first.Status)
	}
	if !second.Duplicate {
		t.Fatalf("second should be duplicate: %+v", second)
	}
	if sender.calls != 1 || repository.SendCount() != 1 {
		t.Fatalf("sender calls=%d send count=%d", sender.calls, repository.SendCount())
	}
}

func TestApprovalApproveUsesSavedRichTextFormat(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "## Ответ\n\n| A | B |\n|---|---|",
		TextFormat:   contracts.TextFormatRich,
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(1008, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	draft, ok := repository.Draft(inbound.DraftID)
	if !ok {
		t.Fatalf("draft not found")
	}
	if draft.TextFormat != contracts.TextFormatRich {
		t.Fatalf("draft text format = %q", draft.TextFormat)
	}
	_, err = service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
		Action:          contracts.ApprovalApprove,
		TenantID:        "tenant-1",
		ChatID:          "42",
		SourceMessageID: "7",
		DraftID:         inbound.DraftID,
		DecisionID:      inbound.Event.EventID,
		IdempotencyKey:  "callback-rich-approve",
	})
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if sender.richCalls != 1 || sender.calls != 0 {
		t.Fatalf("rich calls=%d plain calls=%d", sender.richCalls, sender.calls)
	}
	if sender.richSent[0].Text != draft.ResponseText {
		t.Fatalf("sent rich text = %q want %q", sender.richSent[0].Text, draft.ResponseText)
	}
}

func TestApprovalEditRecordsOriginalAsNegativeFeedback(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "ai original text",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(2005, "bc-1", 42, 7, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}

	edit := func(key, text string) {
		if _, err := service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
			Action:         contracts.ApprovalEdit,
			TenantID:       "tenant-1",
			ChatID:         "42",
			DraftID:        inbound.DraftID,
			EditedText:     text,
			IdempotencyKey: key,
		}); err != nil {
			t.Fatalf("edit %s: %v", key, err)
		}
	}
	edit("edit-1", "owner rewrite v1")
	edit("edit-2", "owner rewrite v2")

	feedback := repository.RecentFeedback("tenant-1", "42", 10)
	downs := make([]contracts.DraftFeedback, 0)
	for _, fb := range feedback {
		if fb.Rating == "down" {
			downs = append(downs, fb)
		}
	}
	// Only the FIRST edit captures the original AI text; the second edit rewrites
	// already-owner text and must not pollute the signal.
	if len(downs) != 1 {
		t.Fatalf("expected 1 down feedback, got %d: %+v", len(downs), downs)
	}
	if downs[0].ResponseText != "ai original text" {
		t.Fatalf("down feedback should capture original AI text, got %q", downs[0].ResponseText)
	}
}

func TestApprovalNoMemoryUpdatesChatSettings(t *testing.T) {
	repository := store.NewRepository()
	service := newTestService(repository, &fakeAI{}, &fakeSender{})

	_, err := service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
		Action:         contracts.ApprovalNoMemory,
		TenantID:       "tenant-1",
		ChatID:         "42",
		IdempotencyKey: "callback-no-memory",
	})
	if err != nil {
		t.Fatalf("no_memory callback: %v", err)
	}
	settings := repository.ChatSettings("tenant-1", "42")
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		t.Fatalf("no_memory was not enabled: %+v", settings)
	}
}

func TestHandleStartCommandSendsOnboarding(t *testing.T) {
	repository := store.NewRepository()
	sender := &fakeSender{}
	service := newTestService(repository, &fakeAI{}, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2001,
		"message": {"message_id": 11, "chat": {"id": 77}, "from": {"id": 77}, "text": "/start"}
	}`), "")
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	if result.Decision != contracts.DecisionObserve || result.Reason != "start_onboarding" {
		t.Fatalf("result = %+v", result)
	}
	if sender.calls != 1 || !strings.Contains(sender.sent[0].Text, "Telegram Chat Automation") {
		t.Fatalf("sent = %+v", sender.sent)
	}
	if audits := repository.Audits(); len(audits) != 1 {
		t.Fatalf("audit count = %d", len(audits))
	}
}

func TestHandleBusinessConnectionSavesStatus(t *testing.T) {
	repository := store.NewRepository()
	sender := &fakeSender{}
	service := newTestService(repository, &fakeAI{}, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2002,
		"business_connection": {
			"id": "bc-1",
			"user": {"id": 77},
			"user_chat_id": 88,
			"date": 1700000000,
			"can_reply": true,
			"is_enabled": true
		}
	}`), "")
	if err != nil {
		t.Fatalf("business connection: %v", err)
	}
	if result.Decision != contracts.DecisionObserve {
		t.Fatalf("result = %+v", result)
	}
	connection, ok := repository.BusinessConnection("bc-1")
	if !ok || connection.TenantID != "tenant:user:77" || !connection.CanReply {
		t.Fatalf("connection = %+v ok=%v", connection, ok)
	}
	if sender.calls != 1 || !strings.Contains(sender.sent[0].Text, "подключен") {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

func TestBusinessMessageUsesSavedConnectionTenant(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: "bc-1",
		TenantID:     "tenant:user:77",
		UserID:       "77",
		UserChatID:   "88",
		CanReply:     true,
		IsEnabled:    true,
	})
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "hello",
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(2003, "bc-1", 42, 7, "hi"), "")
	if err != nil {
		t.Fatalf("business message: %v", err)
	}
	if result.Event.TenantID != "tenant:user:77" || result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("result = %+v", result)
	}
}

func TestModeCommandUpdatesManagedChat(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "42",
		DisplayName: "Customer Alice",
		LastSeenAt:  time.Unix(1700000000, 0).UTC(),
	})
	sender := &fakeSender{}
	service := newTestService(repository, &fakeAI{}, sender)

	// manual mode is always unlocked (no context score required).
	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2004,
		"message": {"message_id": 12, "chat": {"id": 77}, "from": {"id": 77}, "text": "/mode_manual 1"}
	}`), "")
	if err != nil {
		t.Fatalf("mode command: %v", err)
	}
	if result.Reason != "mode_command_updated" {
		t.Fatalf("result = %+v", result)
	}
	if got := repository.ChatSettings("tenant:user:77", "42"); got.Mode != contracts.ChatModeManual {
		t.Fatalf("settings = %+v", got)
	}
	if sender.calls != 1 || !strings.Contains(sender.sent[0].Text, "Customer Alice") {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

func TestChatsCommandListsObservedChats(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "42",
		DisplayName: "Customer Alice",
		LastSeenAt:  time.Unix(1700000000, 0).UTC(),
	})
	sender := &fakeSender{}
	service := newTestService(repository, &fakeAI{}, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2006,
		"message": {"message_id": 13, "chat": {"id": 77}, "from": {"id": 77}, "text": "/chats"}
	}`), "")
	if err != nil {
		t.Fatalf("chats command: %v", err)
	}
	if result.Reason != "chats_command" {
		t.Fatalf("result = %+v", result)
	}
	if sender.calls != 1 || !strings.Contains(sender.sent[0].Text, "1. Customer Alice") {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

func TestChatsCommandListsUnboundBusinessChatFromRealWebhookFlow(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveUnclaimedChat(store.UnclaimedChatRecord{
		BusinessConnectionID: "bc-1",
		ChatID:               "42",
		DisplayName:          "Customer Alice",
		ChatType:             "private",
		LastMessageAt:        time.Unix(1700000000, 0).UTC(),
		Status:               "pending",
	})
	sender := &fakeSender{}
	service := newTestService(repository, &fakeAI{}, sender)

	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2008,
		"message": {"message_id": 14, "chat": {"id": 77}, "from": {"id": 77}, "text": "/chats"}
	}`), "")
	if err != nil {
		t.Fatalf("chats command: %v", err)
	}
	if result.Reason != "chats_command" {
		t.Fatalf("result = %+v", result)
	}
	if sender.calls != 1 || !strings.Contains(sender.sent[0].Text, "1. Customer Alice") {
		t.Fatalf("sent = %+v", sender.sent)
	}
}

func TestModeCommandBindsBusinessConnectionTenantForFutureMessages(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveUnclaimedChat(store.UnclaimedChatRecord{
		BusinessConnectionID: "bc-1",
		ChatID:               "42",
		DisplayName:          "Customer Alice",
		ChatType:             "private",
		LastMessageAt:        time.Unix(1700000000, 0).UTC(),
		Status:               "pending",
	})
	// Populate enough context to unlock auto mode (score >= 80).
	repository.SaveContactProfile(contracts.ContactProfile{
		TenantID: "tenant:user:77", ChatID: "42",
		Name: "Alice", Gender: "female", Role: "client", RelationshipType: "formal",
		Tone: "formal",
	})
	for i := 0; i < 3; i++ {
		repository.SaveSemanticFact(store.SemanticFactRecord{
			TenantID:   "tenant:user:77",
			ChatID:     "42",
			ScopeKind:  "chat",
			FactType:   "preference",
			Subject:    "contact",
			FactText:   fmt.Sprintf("fact %d about contact", i),
			Confidence: 0.8,
			Source:     "llm_extracted",
		})
	}
	sender := &fakeSender{}
	service := newTestService(repository, &fakeAI{}, sender)

	_, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2009,
		"message": {"message_id": 15, "chat": {"id": 77}, "from": {"id": 77}, "text": "/mode_auto 1"}
	}`), "")
	if err != nil {
		t.Fatalf("mode command: %v", err)
	}
	if tenantID, ok := repository.TenantForBusinessConnection("bc-1"); !ok || tenantID != "tenant:user:77" {
		t.Fatalf("tenant binding = %s ok=%v", tenantID, ok)
	}

	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "hello",
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	service = newTestService(repository, ai, sender)
	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(2010, "bc-1", 42, 16, "hi"), "")
	if err != nil {
		t.Fatalf("business message: %v", err)
	}
	if result.Event.TenantID != "tenant:user:77" || result.Decision != contracts.DecisionAutoReply {
		t.Fatalf("result = %+v", result)
	}
}

func TestBusinessMessageRegistersChatForOwnerSelection(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeDisabled})
	service := newTestService(repository, &fakeAI{}, &fakeSender{})

	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2007,
		"business_message": {
			"message_id": 7,
			"business_connection_id": "bc-1",
			"chat": {"id": 42, "type": "private"},
			"from": {"id": 99},
			"text": "hi",
			"date": 1700000000
		}
	}`), "tenant-1")
	if err != nil {
		t.Fatalf("business message: %v", err)
	}
	if result.Decision != contracts.DecisionIgnore {
		t.Fatalf("result = %+v", result)
	}
	chats := repository.ChatsForTenant("tenant-1")
	if len(chats) != 1 || !strings.HasPrefix(chats[0].DisplayName, "Собеседник ") {
		t.Fatalf("chats = %+v", chats)
	}
}

func TestOwnerBusinessMessageIsStoredWithoutAIReply(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: "bc-1",
		TenantID:     "tenant:user:77",
		UserID:       "77",
		CanReply:     true,
		IsEnabled:    true,
	})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42"})
	ai := &fakeAI{}
	service := newTestService(repository, ai, &fakeSender{})

	result, err := service.HandleTelegramWebhook(context.Background(), []byte(`{
		"update_id": 2011,
		"business_message": {
			"message_id": 17,
			"business_connection_id": "bc-1",
			"chat": {"id": 42, "type": "private", "first_name": "Alice"},
			"from": {"id": 77, "first_name": "Owner"},
			"text": "I will answer later",
			"date": 1700000000
		}
	}`), "")
	if err != nil {
		t.Fatalf("business message: %v", err)
	}
	if result.Decision != contracts.DecisionObserve || result.Reason != "owner_message_observed" {
		t.Fatalf("result = %+v", result)
	}
	if ai.calls != 0 {
		t.Fatalf("owner-authored message should not call ai")
	}
}

func TestBusinessConnectionCannotReplyBlocksAutoReply(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: "bc-1",
		TenantID:     "tenant:user:77",
		UserID:       "77",
		UserChatID:   "88",
		CanReply:     false,
		IsEnabled:    true,
	})
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:77", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionAutoReply,
		ResponseText: "hello",
		Confidence:   0.99,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	result, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(2005, "bc-1", 42, 7, "hi"), "")
	if err != nil {
		t.Fatalf("business message: %v", err)
	}
	if result.Decision != contracts.DecisionAskOwner || ai.calls != 0 {
		t.Fatalf("result = %+v ai calls=%d", result, ai.calls)
	}
}

// TestEndToEndSmokePipeline drives the full inbound pipeline in a single test:
//
//  1. business_connection webhook → tenant + connection registered.
//  2. business_message from unclaimed chat → unclaimed_chats + chats(status=unclaimed) +
//     messages stored; IsChatClaimed returns false.
//  3. UI claim → chats(status=managed) + chat_settings bootstrapped; IsChatClaimed returns true.
//  4. business_message in claimed chat → message + active_episode + AI draft created.
//  5. Read path → draft is pending in repository; semantic fact extracted from AI response.
func TestEndToEndSmokePipeline(t *testing.T) {
	repository := store.NewRepository()
	ai := &fakeAI{}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)
	ctx := context.Background()

	// ── Step 1: business_connection webhook ──────────────────────────────────
	// Owner connects the bot in Telegram Settings → Chat Automation.
	step1, err := service.HandleTelegramWebhook(ctx, []byte(`{
		"update_id": 3001,
		"business_connection": {
			"id": "bc-smoke",
			"user": {"id": 77},
			"user_chat_id": 77,
			"date": 1700000000,
			"can_reply": true,
			"is_enabled": true
		}
	}`), "")
	if err != nil {
		t.Fatalf("step 1 business_connection: %v", err)
	}
	if step1.Decision != contracts.DecisionObserve {
		t.Fatalf("step 1: expected observe, got %+v", step1)
	}
	conn, ok := repository.BusinessConnection("bc-smoke")
	if !ok || conn.TenantID != "tenant:user:77" || !conn.CanReply {
		t.Fatalf("step 1: connection not registered: %+v ok=%v", conn, ok)
	}

	// ── Step 2: first business_message from unclaimed chat ───────────────────
	// Tenant is resolved from the saved connection (tenantHint left empty).
	// Expected: staged in unclaimed_chats, chat row stored with status='unclaimed',
	// message body persisted for post-claim history, decision=ignore.
	step2, err := service.HandleTelegramWebhook(ctx, telegramPayload(3002, "bc-smoke", 42, 7, "Привет"), "")
	if err != nil {
		t.Fatalf("step 2 unclaimed message: %v", err)
	}
	if step2.Decision != contracts.DecisionIgnore || step2.Reason != "passive_processing" {
		t.Fatalf("step 2: expected passive_processing, got %+v", step2)
	}
	if repository.IsChatClaimed("tenant:user:77", "42") {
		t.Fatalf("step 2: chat must be unclaimed after first message")
	}
	// The chat must appear as a pending suggestion via UnclaimedChatsForOwner.
	suggested2 := repository.UnclaimedChatsForOwner("tenant:user:77")
	if len(suggested2) == 0 {
		t.Fatalf("step 2: unclaimed chat must appear in UnclaimedChatsForOwner")
	}
	// Message body must be persisted so history is available after the owner claims the chat.
	if _, ok := repository.Message("tenant:user:77", "42", "7"); !ok {
		t.Fatalf("step 2: message must be stored for unclaimed chat with known owner tenant")
	}

	// ── Step 3: UI claim ─────────────────────────────────────────────────────
	// Owner picks the chat from the suggestions list and claims it.
	// apiAddChats uses UnclaimedChatByConnection; here we simulate the same path.
	ucRecord, ok := repository.UnclaimedChatByConnection("bc-smoke", "42")
	if !ok {
		t.Fatalf("step 3: UnclaimedChatByConnection must find the pending chat")
	}
	chatToClaim := store.ChatRecord{
		ChatID:               ucRecord.ChatID,
		DisplayName:          ucRecord.DisplayName,
		ChatType:             ucRecord.ChatType,
		BusinessConnectionID: ucRecord.BusinessConnectionID,
	}
	repository.ClaimChatForTenant("tenant:user:77", chatToClaim)
	if !repository.IsChatClaimed("tenant:user:77", "42") {
		t.Fatalf("step 3: IsChatClaimed must return true after claim")
	}
	settings3 := repository.ChatSettings("tenant:user:77", "42")
	if settings3.Mode != contracts.ChatModeManual {
		t.Fatalf("step 3: expected manual mode after claim, got %s", settings3.Mode)
	}

	// ── Step 4: second business_message in claimed chat ──────────────────────
	// AI returns a draft; MemoryUsed carries a fact to extract.
	ai.response = contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "Добрый день! Чем могу помочь?",
		Confidence:   0.85,
		RiskLevel:    contracts.RiskLow,
		MemoryUsed:   []string{"предпочитает формальный стиль общения"},
	}
	step4, err := service.HandleTelegramWebhook(ctx, telegramPayload(3003, "bc-smoke", 42, 8, "Нужна консультация"), "")
	if err != nil {
		t.Fatalf("step 4 claimed message: %v", err)
	}
	if step4.Decision != contracts.DecisionDraft {
		t.Fatalf("step 4: expected draft decision, got %+v", step4)
	}
	if step4.DraftID == "" {
		t.Fatalf("step 4: draft_id missing from result")
	}
	// Runtime state: active episode must be open for this chat.
	if _, ok := repository.ActiveEpisodeByChat("tenant:user:77", "42"); !ok {
		t.Fatalf("step 4: active_episode must exist after claimed message processed")
	}

	// ── Step 5: read path ────────────────────────────────────────────────────
	// Draft is pending in the repository; the AI-extracted fact is stored
	// in the Semantic Layer and available to the next conversation context build.
	draft5, ok := repository.Draft(step4.DraftID)
	if !ok || draft5.Status != store.DraftPending {
		t.Fatalf("step 5: draft not found or wrong status: %+v ok=%v", draft5, ok)
	}
	if draft5.ResponseText != "Добрый день! Чем могу помочь?" {
		t.Fatalf("step 5: draft response text = %q", draft5.ResponseText)
	}
	facts5 := repository.SemanticFacts("tenant:user:77", "42", 10)
	if len(facts5) == 0 || !strings.Contains(facts5[0].FactText, "формальный") {
		t.Fatalf("step 5: expected extracted semantic fact, got %+v", facts5)
	}
}

func newTestService(repository *store.Repository, ai aiclient.Client, sender telegram.Sender) *Service {
	return NewService(repository, ai, sender,
		WithClock(func() time.Time {
			return time.Unix(1700000000, 0).UTC()
		}),
		// The legacy suite exercises the Business webhook path; "full" keeps
		// both channels active so those scenarios stay covered while the
		// production default is "bot".
		WithDefaultIngestionMode(contracts.IngestionModeFull),
	)
}

// fakeFeedbackSender records every owner-feedback score emitted by the
// service so Phase 4 tests can assert that approval/thumb actions attach a
// Langfuse score to the trace the draft was generated under.
type fakeFeedbackSender struct {
	mu    sync.Mutex
	calls []feedbackCall
}

type feedbackCall struct {
	traceID string
	rating  string
	comment string
}

func (f *fakeFeedbackSender) SendOwnerFeedback(_ context.Context, traceID, rating, comment string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, feedbackCall{traceID, rating, comment})
}

func TestOwnerApprovalSendsLangfuseScore(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "draft",
		Confidence:   0.9,
		RiskLevel:    contracts.RiskLow,
	}}
	feedback := &fakeFeedbackSender{}
	service := NewService(repository, ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithOwnerFeedbackSender(feedback),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
	)

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(2001, "bc-1", 42, 11, "hi"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	payload := contracts.ApprovalCallbackPayload{
		Action:          contracts.ApprovalApprove,
		TenantID:        "tenant-1",
		ChatID:          "42",
		SourceMessageID: "11",
		DraftID:         inbound.DraftID,
		DecisionID:      inbound.Event.EventID,
		IdempotencyKey:  "callback-feedback",
	}
	if _, err := service.HandleApprovalCallback(context.Background(), payload); err != nil {
		t.Fatalf("approval: %v", err)
	}

	if len(feedback.calls) != 1 {
		t.Fatalf("expected 1 owner feedback call, got %d", len(feedback.calls))
	}
	call := feedback.calls[0]
	if call.rating != "up" {
		t.Fatalf("expected rating 'up', got %q", call.rating)
	}
	draft, ok := repository.Draft(inbound.DraftID)
	if !ok {
		t.Fatalf("draft not found")
	}
	if call.traceID == "" || call.traceID != draft.TraceID {
		t.Fatalf("score trace_id %q != draft trace_id %q", call.traceID, draft.TraceID)
	}
}

// TestCriticalRiskWarningNeverReachesContact verifies the ARCH-1 contract:
// a critical-risk message produces an owner-facing warning card with no
// sendable text, and no approval callback can deliver anything to the contact.
func TestCriticalRiskWarningNeverReachesContact(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeAuto})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:   contracts.DecisionAskOwner,
		RiskLevel:  contracts.RiskCritical,
		Confidence: 0.35,
		OwnerNote:  "⚠️ Сообщение интимного характера. ИИ не формирует ответ — ответьте вручную.",
	}}
	sender := &fakeSender{}
	service := newTestService(repository, ai, sender)

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(7001, "bc-1", 42, 7, "критичное сообщение"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if inbound.Decision != contracts.DecisionAskOwner || inbound.DraftID == "" {
		t.Fatalf("inbound = %+v", inbound)
	}
	draft, ok := repository.Draft(inbound.DraftID)
	if !ok {
		t.Fatalf("warning draft not found")
	}
	if draft.DraftType != "warning" || strings.TrimSpace(draft.ResponseText) != "" {
		t.Fatalf("warning draft = type %q, text %q; want empty warning draft", draft.DraftType, draft.ResponseText)
	}

	// Every sending/mutating callback must fail; reject must succeed.
	for i, action := range []contracts.ApprovalAction{
		contracts.ApprovalApprove,
		contracts.ApprovalEdit,
		contracts.ApprovalRegenerate,
		contracts.ApprovalAIRefine,
		contracts.ApprovalMakeShorter,
	} {
		_, err := service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
			Action:         action,
			TenantID:       "tenant-1",
			ChatID:         "42",
			DraftID:        inbound.DraftID,
			EditedText:     "owner text",
			IdempotencyKey: "warn-cb-" + intString(int64(i)),
		})
		if err == nil {
			t.Fatalf("action %s on warning card must be rejected", action)
		}
	}
	if repository.SendCount() != 0 {
		t.Fatalf("warning text was sent to contact: send count = %d", repository.SendCount())
	}
	reject, err := service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
		Action:         contracts.ApprovalReject,
		TenantID:       "tenant-1",
		ChatID:         "42",
		DraftID:        inbound.DraftID,
		IdempotencyKey: "warn-cb-reject",
	})
	if err != nil || reject.Status != string(store.DraftRejected) {
		t.Fatalf("reject on warning card: status=%s err=%v", reject.Status, err)
	}
}

// TestChoiceQuestionUsesStructuredOptions verifies the ARCH-4 contract: when
// the AI labels the message choice_question and supplies options, the backend
// builds a question card from those options (not from response_text lines).
func TestChoiceQuestionUsesStructuredOptions(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "Могу в понедельник.",
		Intent:       "choice_question",
		Options:      []string{"Понедельник", "Вторник"},
		Confidence:   0.7,
		RiskLevel:    contracts.RiskLow,
	}}
	service := newTestService(repository, ai, &fakeSender{})

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(7002, "bc-1", 42, 7, "Понедельник или вторник?"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	draft, ok := repository.Draft(inbound.DraftID)
	if !ok {
		t.Fatalf("question draft not found")
	}
	if draft.DraftType != "question" || len(draft.SuggestedOptions) != 2 || draft.SuggestedOptions[0] != "Понедельник" {
		t.Fatalf("question draft = %+v", draft)
	}
}

// TestStructuredTopicUpdatesActiveEpisode verifies that the topic carried by
// the structured process-message response lands in the active episode (ARCH-4
// replacement for the per-message tag-topic call).
func TestStructuredTopicUpdatesActiveEpisode(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "draft",
		Confidence:   0.8,
		RiskLevel:    contracts.RiskLow,
		Topic:        "обсуждение сроков поставки",
	}}
	service := newTestService(repository, ai, &fakeSender{})

	if _, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(7003, "bc-1", 42, 7, "когда поставка?"), "tenant-1"); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	ep, ok := repository.ActiveEpisodeByChat("tenant-1", "42")
	if !ok {
		t.Fatalf("active episode missing")
	}
	if ep.CurrentTopic != "обсуждение сроков поставки" {
		t.Fatalf("current_topic = %q", ep.CurrentTopic)
	}
}

// slowAI delays each ProcessMessage call to simulate a slow LLM provider.
type slowAI struct {
	fakeAI
	delay time.Duration
}

func (f *slowAI) ProcessMessage(ctx context.Context, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	time.Sleep(f.delay)
	return f.fakeAI.ProcessMessage(ctx, request)
}

// TestEnqueueWebhookAcksFastWithSlowAI verifies the ARCH-7 contract: enqueueing
// a webhook returns immediately even when the AI takes seconds, and the update
// is still fully processed by the worker pool afterwards.
func TestEnqueueWebhookAcksFastWithSlowAI(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &slowAI{
		fakeAI: fakeAI{response: contracts.AIProcessMessageResponse{
			Decision:     contracts.DecisionDraft,
			ResponseText: "draft",
			Confidence:   0.9,
			RiskLevel:    contracts.RiskLow,
		}},
		delay: 1500 * time.Millisecond,
	}
	service := newTestService(repository, ai, &fakeSender{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.StartWebhookWorkers(ctx)

	start := time.Now()
	service.EnqueueTelegramWebhook(telegramPayload(8001, "bc-1", 42, 7, "привет"), "tenant-1")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("enqueue blocked for %s; webhook ACK must not wait for the AI", elapsed)
	}

	// The worker must still complete processing: a draft eventually appears.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if drafts := repository.DraftsForChat("tenant-1", "42"); len(drafts) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("queued webhook was never processed")
}

// TestConversationContextRespectsTokenBudget verifies the §8.1/MEM-9 contract:
// oversized contexts are trimmed deterministically (messages first), runtime
// header lines survive any budget, and the paused_hours flag is injected.
func TestConversationContextRespectsTokenBudget(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	long := strings.Repeat("очень длинное сообщение для переполнения бюджета токенов ", 30)
	base := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		repository.SaveMessage(contracts.NormalizedTelegramEvent{
			EventID:         fmt.Sprintf("e%d", i),
			TenantID:        "tenant-1",
			ChatID:          "42",
			SourceMessageID: fmt.Sprintf("m%d", i),
			MessageText:     long,
			CreatedAt:       base.Add(time.Duration(i) * time.Minute),
		})
	}
	service := newTestService(repository, &fakeAI{}, &fakeSender{})

	event := contracts.NormalizedTelegramEvent{
		TenantID: "tenant-1", ChatID: "42", SourceMessageID: "current", MessageText: "вопрос",
	}
	parts := service.conversationContext(context.Background(), event, "", 20)

	messageLines := 0
	pausedSeen := false
	for _, p := range parts {
		if strings.HasPrefix(p, "[2026-") {
			messageLines++
		}
		if strings.HasPrefix(p, "[RUNTIME: paused_hours=") {
			pausedSeen = true
		}
	}
	if messageLines > 10 {
		t.Fatalf("budget trimming did not cap messages: %d lines", messageLines)
	}
	if !pausedSeen {
		t.Fatalf("paused_hours runtime line missing from context")
	}
}

// TestProfileSuggestionLifecycle verifies the §4.5/MEM-4 contract: a staged
// suggestion never touches the profile by itself, accept applies exactly one
// field, reject applies nothing, and a duplicate pending suggestion is dropped.
func TestProfileSuggestionLifecycle(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:99", ChatID: "42"})

	id := repository.SaveProfileSuggestion(store.ProfileSuggestionRecord{
		TenantID: "tenant:user:99", ChatID: "42",
		FieldName: "relationship_type", SuggestedValue: "professional",
		Rationale: "обсуждают рабочие задачи",
	})
	if id == 0 {
		t.Fatalf("suggestion not staged")
	}
	// Duplicate pending suggestion for the same field is dropped.
	if dup := repository.SaveProfileSuggestion(store.ProfileSuggestionRecord{
		TenantID: "tenant:user:99", ChatID: "42",
		FieldName: "relationship_type", SuggestedValue: "professional",
	}); dup != 0 {
		t.Fatalf("duplicate pending suggestion staged: id=%d", dup)
	}
	// Whitelist: a non-profile field never reaches the table.
	if bad := repository.SaveProfileSuggestion(store.ProfileSuggestionRecord{
		TenantID: "tenant:user:99", ChatID: "42",
		FieldName: "notes; DROP TABLE", SuggestedValue: "x",
	}); bad != 0 {
		t.Fatalf("non-whitelisted field staged: id=%d", bad)
	}
	// Staging alone must not modify the profile.
	if profile, _ := repository.ContactProfile("tenant:user:99", "42"); profile.RelationshipType != "" {
		t.Fatalf("suggestion wrote to profile without accept: %+v", profile)
	}

	if err := repository.ResolveSuggestion("tenant:user:99", id, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	profile, _ := repository.ContactProfile("tenant:user:99", "42")
	if profile.RelationshipType != "professional" {
		t.Fatalf("accepted suggestion not applied: %+v", profile)
	}
	if got := len(repository.PendingSuggestions("tenant:user:99", "42")); got != 0 {
		t.Fatalf("pending after resolve = %d", got)
	}

	// Reject path: value is not applied.
	id2 := repository.SaveProfileSuggestion(store.ProfileSuggestionRecord{
		TenantID: "tenant:user:99", ChatID: "42",
		FieldName: "tone", SuggestedValue: "formal",
	})
	if err := repository.ResolveSuggestion("tenant:user:99", id2, false); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if profile, _ := repository.ContactProfile("tenant:user:99", "42"); profile.Tone != "" {
		t.Fatalf("rejected suggestion applied: %+v", profile)
	}
}

// TestForgetChatErasesMemoryButKeepsOtherChats verifies the §10.2/MEM-2
// contract end-to-end on the in-memory repository: after the confirmed forget
// flow, every memory artifact of the chat is gone while another chat's facts
// survive, and the destructive action never runs without confirmation.
func TestForgetChatErasesMemoryButKeepsOtherChats(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:99", ChatID: "42"})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:99", ChatID: "77"})
	repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID: "e1", TenantID: "tenant:user:99", ChatID: "42", SourceMessageID: "m1",
		MessageText: "секретная история", CreatedAt: time.Now(),
	})
	repository.UpsertSemanticFact(store.SemanticFactRecord{
		TenantID: "tenant:user:99", ScopeKind: "chat", ChatID: "42",
		FactText: "у него диабет", FactType: "health", Subject: "contact", Confidence: 0.8,
	}, nil)
	repository.UpsertSemanticFact(store.SemanticFactRecord{
		TenantID: "tenant:user:99", ScopeKind: "chat", ChatID: "77",
		FactText: "любит краткие ответы", FactType: "preference", Subject: "contact", Confidence: 0.8,
	}, nil)

	service := newTestService(repository, &fakeAI{}, &fakeSender{})

	// /forget only asks for confirmation — nothing is deleted yet.
	resp := service.botLayer.HandleCommand(telegram.BotCommandUpdate{
		Command: "/forget", Arguments: "1", SenderID: "99", ChatID: "99",
	}, "tenant:user:99")
	if resp.Reason != "forget_confirmation_requested" {
		t.Fatalf("reason = %s", resp.Reason)
	}
	if got := len(repository.SemanticFacts("tenant:user:99", "42", 10)); got != 1 {
		t.Fatalf("facts deleted before confirmation: %d", got)
	}

	if err := repository.ForgetChat("tenant:user:99", "42"); err != nil {
		t.Fatalf("forget chat: %v", err)
	}
	if got := len(repository.SemanticFacts("tenant:user:99", "42", 10)); got != 0 {
		t.Fatalf("facts survived forget: %d", got)
	}
	if got := len(repository.RecentMessages("tenant:user:99", "42", 10)); got != 0 {
		t.Fatalf("messages survived forget: %d", got)
	}
	if got := len(repository.SemanticFacts("tenant:user:99", "77", 10)); got != 1 {
		t.Fatalf("other chat's facts touched: %d", got)
	}
}

// TestForgetAllRequiresExactPhrase verifies that full erasure runs only on the
// verbatim confirmation phrase (§10.2).
func TestForgetAllRequiresExactPhrase(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant:user:99", ChatID: "42"})
	repository.UpsertSemanticFact(store.SemanticFactRecord{
		TenantID: "tenant:user:99", ScopeKind: "chat", ChatID: "42",
		FactText: "важный факт о собеседнике", FactType: "preference", Subject: "contact", Confidence: 0.8,
	}, nil)
	service := newTestService(repository, &fakeAI{}, &fakeSender{})

	service.botLayer.HandleCommand(telegram.BotCommandUpdate{
		Command: "/forget_all", SenderID: "99", ChatID: "99",
	}, "tenant:user:99")

	// Wrong phrase cancels.
	service.handleOwnerMessage(context.Background(), telegram.OwnerMessageUpdate{
		SenderID: "99", ChatID: "99", MessageID: "om1", Text: "удалить всё",
	})
	if got := len(repository.SemanticFacts("tenant:user:99", "42", 10)); got != 1 {
		t.Fatalf("data deleted on wrong phrase: %d facts", got)
	}

	// Exact phrase erases.
	service.botLayer.HandleCommand(telegram.BotCommandUpdate{
		Command: "/forget_all", SenderID: "99", ChatID: "99",
	}, "tenant:user:99")
	service.handleOwnerMessage(context.Background(), telegram.OwnerMessageUpdate{
		SenderID: "99", ChatID: "99", MessageID: "om2", Text: contracts.ForgetAllConfirmationPhrase,
	})
	if got := len(repository.SemanticFacts("tenant:user:99", "42", 10)); got != 0 {
		t.Fatalf("facts survived forget_all: %d", got)
	}
	if got := len(repository.ChatsForTenant("tenant:user:99")); got != 0 {
		t.Fatalf("chats survived forget_all: %d", got)
	}
}

// TestEpisodeRuntimeStateLifecycle verifies the §6.2/MEM-5 contract: a contact
// question awaiting a draft sets open_question/pending_owner_action, and an
// approved (sent) reply resets both. message_count is not inflated by the
// runtime write.
func TestEpisodeRuntimeStateLifecycle(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant-1", ChatID: "42", Mode: contracts.ChatModeManual})
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision:     contracts.DecisionDraft,
		ResponseText: "draft",
		Intent:       "question",
		Confidence:   0.8,
		RiskLevel:    contracts.RiskLow,
		Topic:        "сроки поставки",
	}}
	service := newTestService(repository, ai, &fakeSender{})

	inbound, err := service.HandleTelegramWebhook(context.Background(), telegramPayload(9001, "bc-1", 42, 7, "Когда будет поставка?"), "tenant-1")
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	ep, ok := repository.ActiveEpisodeByChat("tenant-1", "42")
	if !ok {
		t.Fatalf("active episode missing")
	}
	if ep.OpenQuestionMsgID != "7" {
		t.Fatalf("open_question_message_id = %q, want \"7\"", ep.OpenQuestionMsgID)
	}
	if ep.PendingOwnerAction == "" {
		t.Fatalf("pending_owner_action not set for a draft decision")
	}
	if ep.MessageCount != 1 {
		t.Fatalf("message_count inflated by runtime write: %d", ep.MessageCount)
	}

	// Approving the draft sends the reply and must reset the runtime state.
	if _, err := service.HandleApprovalCallback(context.Background(), contracts.ApprovalCallbackPayload{
		Action:         contracts.ApprovalApprove,
		TenantID:       "tenant-1",
		ChatID:         "42",
		DraftID:        inbound.DraftID,
		IdempotencyKey: "runtime-approve",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ep, _ = repository.ActiveEpisodeByChat("tenant-1", "42")
	if ep.OpenQuestionMsgID != "" || ep.PendingOwnerAction != "" {
		t.Fatalf("runtime state not cleared after send: %+v", ep)
	}
}

// summarizingAI is a fakeAI that also answers SummarizeChat with a fixed
// summary long enough to satisfy the episode_summaries content CHECK (≥50).
type summarizingAI struct {
	fakeAI
	summaries int
}

func (f *summarizingAI) SummarizeChat(ctx context.Context, request contracts.ChatSummaryRequest) (contracts.ChatSummaryResponse, error) {
	f.summaries++
	return contracts.ChatSummaryResponse{
		Summary: "Обсуждали сроки поставки и условия договора; собеседник ждёт счёт в пятницу.",
	}, nil
}

// TestSummarizeChatCaches verifies the on-demand summary is memoised: a second
// request over the same timeline reuses the cached result (no extra LLM call),
// while a new message invalidates the cache and forces a fresh summary.
func TestSummarizeChatCaches(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &summarizingAI{}
	service := newTestService(repository, ai, &fakeSender{})
	ctx := context.Background()

	save := func(id string) {
		repository.SaveMessage(contracts.NormalizedTelegramEvent{
			EventID: id, TenantID: "tenant-1", ChatID: "42",
			SourceMessageID: id, MessageText: "содержательное сообщение для сводки",
			CreatedAt: time.Now(),
		})
	}
	save("e1")
	save("e2")

	if _, _, err := service.SummarizeChat(ctx, "tenant-1", "42"); err != nil {
		t.Fatalf("first summarize: %v", err)
	}
	if _, _, err := service.SummarizeChat(ctx, "tenant-1", "42"); err != nil {
		t.Fatalf("second summarize: %v", err)
	}
	if ai.summaries != 1 {
		t.Fatalf("cached second request must not re-call the LLM: got %d calls, want 1", ai.summaries)
	}

	// A new message changes the timeline marker → cache miss → fresh summary.
	save("e3")
	if _, _, err := service.SummarizeChat(ctx, "tenant-1", "42"); err != nil {
		t.Fatalf("third summarize: %v", err)
	}
	if ai.summaries != 2 {
		t.Fatalf("new message must invalidate cache: got %d calls, want 2", ai.summaries)
	}
}

// TestEpisodeSummariesPerEpisodeIdempotency verifies the MEM-6 contract:
// two episodes closed on the same day produce two summaries with real episode
// bounds and provenance; re-closing the same episode adds nothing; the daily
// fallback does not duplicate covered windows.
func TestEpisodeSummariesPerEpisodeIdempotency(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{TenantID: "tenant-1", ChatID: "42"})
	ai := &summarizingAI{}
	service := newTestService(repository, ai, &fakeSender{})
	ctx := context.Background()

	day := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	seed := func(start time.Time, prefix string) {
		for i := 0; i < 3; i++ {
			repository.SaveMessage(contracts.NormalizedTelegramEvent{
				EventID: prefix + "-e" + intString(int64(i)), TenantID: "tenant-1", ChatID: "42",
				SourceMessageID: prefix + "-m" + intString(int64(i)),
				MessageText:     "содержательное сообщение для сводки",
				CreatedAt:       start.Add(time.Duration(i) * time.Minute),
			})
		}
	}
	morningStart, morningEnd := day.Add(10*time.Hour), day.Add(10*time.Hour+2*time.Minute)
	eveningStart, eveningEnd := day.Add(18*time.Hour), day.Add(18*time.Hour+2*time.Minute)
	seed(morningStart, "am")
	seed(eveningStart, "pm")

	service.summarizeEpisode(ctx, "tenant-1", "42", morningStart, morningEnd, "message_count")
	service.summarizeEpisode(ctx, "tenant-1", "42", eveningStart, eveningEnd, "silence_gap")
	if got := len(repository.EpisodeSummaries("tenant-1", "42", 10)); got != 2 {
		t.Fatalf("summaries after two episode closes = %d, want 2 (second episode of the day must not be lost)", got)
	}

	// Re-closing the same episode is idempotent (period overlap).
	service.summarizeEpisode(ctx, "tenant-1", "42", morningStart, morningEnd, "message_count")
	if got := len(repository.EpisodeSummaries("tenant-1", "42", 10)); got != 2 {
		t.Fatalf("duplicate episode close created a summary: %d", got)
	}

	// Daily fallback: everything fact-bearing is covered; the uncovered tail
	// has no messages → no duplicate.
	service.summarizeChatDay(ctx, "tenant-1", "42", day, "daily_summary")
	if got := len(repository.EpisodeSummaries("tenant-1", "42", 10)); got != 2 {
		t.Fatalf("daily fallback duplicated covered summaries: %d", got)
	}

	// Provenance: episode bounds and source IDs are real, not day bounds.
	for _, sm := range repository.EpisodeSummaries("tenant-1", "42", 10) {
		if sm.PeriodEnd.Sub(sm.PeriodStart) > time.Hour {
			t.Fatalf("summary period looks like a day, not an episode: %v..%v", sm.PeriodStart, sm.PeriodEnd)
		}
	}
}

// TestRedactHealthFactsRemovesPlainText verifies the §10.3/MEM-8 contract:
// health-fact texts never reach extraction_log.raw_output in the clear, while
// ordinary facts stay readable for diagnostics.
func TestRedactHealthFactsRemovesPlainText(t *testing.T) {
	raw := `[{"fact_text":"у него диабет второго типа","fact_type":"health"},` +
		`{"fact_text":"любит краткие ответы","fact_type":"preference"}]`
	facts := []aiclient.TypedFact{
		{FactText: "у него диабет второго типа", FactType: "health"},
		{FactText: "любит краткие ответы", FactType: "preference"},
	}

	got := redactHealthFacts(raw, facts, map[string]int64{"у него диабет второго типа": 42})

	if strings.Contains(got, "диабет") {
		t.Fatalf("health text leaked into raw_output: %s", got)
	}
	if !strings.Contains(got, "[health fact #42]") {
		t.Fatalf("fact_id marker missing: %s", got)
	}
	if !strings.Contains(got, "любит краткие ответы") {
		t.Fatalf("ordinary fact must stay readable: %s", got)
	}
}

// TestFinalizeFactOrderKeepsHardIncludesAndLimit verifies the ARCH-5 contract:
// taboo/health facts always survive final ordering, the result respects the
// limit, and without a query vector the pre-ranked (RRF) order is preserved.
func TestFinalizeFactOrderKeepsHardIncludesAndLimit(t *testing.T) {
	svc := newTestService(store.NewRepository(), &fakeAI{}, &fakeSender{})
	facts := make([]store.SemanticFactRecord, 0, 12)
	facts = append(facts, store.SemanticFactRecord{ID: 100, FactType: "taboo", FactText: "не обсуждать цену"})
	for i := int64(1); i <= 10; i++ {
		facts = append(facts, store.SemanticFactRecord{ID: i, FactType: "preference", FactText: "fact"})
	}

	out := svc.finalizeFactOrder("tenant-1", facts, nil, 8)

	if len(out) != 8 {
		t.Fatalf("len = %d, want 8", len(out))
	}
	if out[0].ID != 100 {
		t.Fatalf("hard-include fact displaced: first ID = %d", out[0].ID)
	}
	// Remaining slots keep RRF order 1..7.
	for i := int64(1); i <= 7; i++ {
		if out[i].ID != i {
			t.Fatalf("RRF order broken at %d: got ID %d", i, out[i].ID)
		}
	}
}

func telegramPayload(updateID int64, businessConnectionID string, chatID, messageID int64, text string) []byte {
	return []byte(`{
		"update_id": ` + intString(updateID) + `,
		"business_message": {
			"message_id": ` + intString(messageID) + `,
			"business_connection_id": "` + businessConnectionID + `",
			"chat": {"id": ` + intString(chatID) + `},
			"from": {"id": 99},
			"text": "` + text + `",
			"date": 1700000000
		}
	}`)
}

func intString(value int64) string {
	return strconv.FormatInt(value, 10)
}

type fakeAI struct {
	response    contracts.AIProcessMessageResponse
	err         error
	calls       int
	lastRequest contracts.AIProcessMessageRequest
}

func (f *fakeAI) ProcessMessage(ctx context.Context, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	f.calls++
	f.lastRequest = request
	return f.response, f.err
}

type fakeSender struct {
	calls        int
	richCalls    int
	sent         []telegram.OutboundMessage
	richSent     []telegram.OutboundMessage
	richErr      error
	richDrafts   []richDraftCall
	richDraftErr error
}

type richDraftCall struct {
	chatID   string
	draftID  int64
	markdown string
}

func (f *fakeSender) SendMessage(ctx context.Context, message telegram.OutboundMessage) error {
	f.calls++
	f.sent = append(f.sent, message)
	return nil
}

func (f *fakeSender) SendRichMessage(ctx context.Context, message telegram.OutboundMessage) (string, error) {
	f.richCalls++
	f.richSent = append(f.richSent, message)
	if f.richErr != nil {
		return "", f.richErr
	}
	return "rich:" + strconv.Itoa(f.richCalls), nil
}

func (f *fakeSender) SendRichMessageDraft(ctx context.Context, chatID string, draftID int64, markdown string) error {
	f.richDrafts = append(f.richDrafts, richDraftCall{chatID: chatID, draftID: draftID, markdown: markdown})
	return f.richDraftErr
}

func (f *fakeSender) AnswerCallbackQuery(ctx context.Context, queryID string) error {
	return nil
}

func (f *fakeSender) EditMessageReplyMarkup(ctx context.Context, chatID, messageID string, markup telegram.InlineKeyboardMarkup) error {
	return nil
}

func (f *fakeSender) EditMessageText(ctx context.Context, chatID, messageID, text string, markup telegram.InlineKeyboardMarkup) error {
	return nil
}

func (f *fakeSender) DeleteMessage(ctx context.Context, chatID, messageID string) error {
	return nil
}

// TestSaveTypedFactsAlwaysChatScoped verifies the per-chat memory invariant:
// every extracted fact — including owner-subject facts the Python service may
// hint as global — is persisted chat-scoped and never leaks into other chats.
func TestSaveTypedFactsAlwaysChatScoped(t *testing.T) {
	event := contracts.NormalizedTelegramEvent{
		TenantID:        "tenant:user:99",
		ChatID:          "chat-42",
		SourceMessageID: "m1",
	}

	repo := store.NewRepository()
	svc := newTestService(repo, &fakeAI{}, &fakeSender{})
	svc.saveTypedFacts(event, []aiclient.TypedFact{
		{FactText: "owner-bio-fact", FactType: "biographical", Subject: "owner",
			ScopeKind: "owner_global", Confidence: 0.95},
		{FactText: "любит краткие ответы", FactType: "preference", Subject: "contact",
			Confidence: 0.8},
	})

	chat := repo.SemanticFacts(event.TenantID, event.ChatID, 10)
	if len(chat) != 2 {
		t.Fatalf("expected 2 chat-scoped facts, got %d", len(chat))
	}
	for _, f := range chat {
		if f.ScopeKind != "chat" || f.ChatID != event.ChatID {
			t.Errorf("fact %q stored with scope %q chat %q; want chat scope in source chat",
				f.FactText, f.ScopeKind, f.ChatID)
		}
	}
	if got := len(repo.SemanticFacts(event.TenantID, "other-chat", 10)); got != 0 {
		t.Errorf("fact leaked into another chat: got %d facts there", got)
	}
}

// TestSaveTypedFactsOutcomeSkipCounting verifies the observability summary:
// facts rejected for length or context-contamination are counted as skipped so
// the extraction_log can report them, while valid facts are persisted.
func TestSaveTypedFactsOutcomeSkipCounting(t *testing.T) {
	event := contracts.NormalizedTelegramEvent{TenantID: "tenant:user:99", ChatID: "chat-42", SourceMessageID: "m1"}
	repo := store.NewRepository()
	svc := newTestService(repo, &fakeAI{}, &fakeSender{})

	outcome := svc.saveTypedFacts(event, []aiclient.TypedFact{
		{FactText: "хочу кофе по утрам", FactType: "preference", Subject: "contact", Confidence: 0.8},
		{FactText: "tiny", FactType: "preference", Subject: "contact", Confidence: 0.8},          // too short → skip
		{FactText: "[ФАКТ] context contamination leak", FactType: "preference", Confidence: 0.8}, // contaminated → skip
	})

	if outcome.Skipped != 2 {
		t.Errorf("expected 2 skipped facts, got %d", outcome.Skipped)
	}
	if got := len(repo.SemanticFacts(event.TenantID, event.ChatID, 10)); got != 1 {
		t.Errorf("expected 1 persisted fact, got %d", got)
	}
}
