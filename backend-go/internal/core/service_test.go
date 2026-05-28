package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

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
						if proposed != contracts.DecisionAutoReply {
							t.Fatalf("auto_reply allowed without AI auto proposal")
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
	audits := repository.Audits()
	if len(audits) != 2 {
		t.Fatalf("audit count = %d", len(audits))
	}
	if audits[1].Reason != "duplicate_receive" {
		t.Fatalf("duplicate audit reason = %s", audits[1].Reason)
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
	return NewService(repository, ai, sender, WithClock(func() time.Time {
		return time.Unix(1700000000, 0).UTC()
	}))
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
	response contracts.AIProcessMessageResponse
	err      error
	calls    int
}

func (f *fakeAI) ProcessMessage(ctx context.Context, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	f.calls++
	return f.response, f.err
}

type fakeSender struct {
	calls int
	sent  []telegram.OutboundMessage
}

func (f *fakeSender) SendMessage(ctx context.Context, message telegram.OutboundMessage) error {
	f.calls++
	f.sent = append(f.sent, message)
	return nil
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
