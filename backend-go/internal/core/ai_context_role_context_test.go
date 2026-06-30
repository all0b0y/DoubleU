package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

func TestConversationContextBotRoleContextIsolation(t *testing.T) {
	const tenantID = "tenant:user:77"
	const botID int64 = 555
	chatID := contracts.BotChatID("555", "42")
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: tenantID, BotID: botID, BotUsername: "helper_bot", Token: "secret"})
	repo.SaveChat(store.ChatRecord{TenantID: tenantID, ChatID: chatID, DisplayName: "Bot chat", Transport: contracts.TransportBot})
	svc := newTestService(repo, &fakeAI{}, &fakeSender{})

	ctxA, err := repo.SetUserBotRoleWithContextPolicy(tenantID, botID, 10, store.RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("activate role A: %v", err)
	}
	saveRoleContextMemory(t, repo, tenantID, chatID, "555", ctxA.ID, "role A", base)

	textA, recordsA := renderedConversationContext(t, svc, tenantID, chatID, "555", "current-a")
	assertContainsAll(t, textA,
		"role A visible message",
		"role A visible fact",
		"role A taboo boundary",
		"role A commitment",
		"role A episode summary",
		"role A pending draft",
		"role A active topic",
	)
	if len(recordsA) != 1 || recordsA[0].RoleContextID != ctxA.ID {
		t.Fatalf("role A records = %+v, want one record in context %d", recordsA, ctxA.ID)
	}

	ctxB, err := repo.SetUserBotRoleWithContextPolicy(tenantID, botID, 11, store.RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("activate role B: %v", err)
	}
	saveRoleContextMemory(t, repo, tenantID, chatID, "555", ctxB.ID, "role B", base.Add(time.Hour))

	textB, recordsB := renderedConversationContext(t, svc, tenantID, chatID, "555", "current-b")
	assertContainsAll(t, textB, "role B visible message", "role B visible fact", "role B pending draft")
	assertContainsNone(t, textB, "role A visible message", "role A visible fact", "role A pending draft", "role A taboo boundary")
	if len(recordsB) != 1 || recordsB[0].RoleContextID != ctxB.ID {
		t.Fatalf("role B records = %+v, want one record in context %d", recordsB, ctxB.ID)
	}

	reusedA, err := repo.SetUserBotRoleWithContextPolicy(tenantID, botID, 10, store.RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("reuse role A: %v", err)
	}
	if reusedA.ID != ctxA.ID {
		t.Fatalf("reused role A context = %d, want %d", reusedA.ID, ctxA.ID)
	}
	textReusedA, _ := renderedConversationContext(t, svc, tenantID, chatID, "555", "current-a-reuse")
	assertContainsAll(t, textReusedA, "role A visible message", "role A visible fact", "role A pending draft")
	assertContainsNone(t, textReusedA, "role B visible message", "role B visible fact", "role B pending draft")

	freshA, err := repo.SetUserBotRoleWithContextPolicy(tenantID, botID, 10, store.RoleContextPolicyNew)
	if err != nil {
		t.Fatalf("new role A context: %v", err)
	}
	if freshA.ID == ctxA.ID {
		t.Fatalf("new role A context reused old id %d", freshA.ID)
	}
	textFreshA, recordsFreshA := renderedConversationContext(t, svc, tenantID, chatID, "555", "current-a-new")
	assertContainsNone(t, textFreshA,
		"role A visible message", "role A visible fact", "role A pending draft",
		"role B visible message", "role B visible fact", "role B pending draft",
	)
	if len(recordsFreshA) != 0 {
		t.Fatalf("fresh role A records = %+v, want empty context", recordsFreshA)
	}
}

func TestConversationContextNonBotRemainsUnscoped(t *testing.T) {
	const tenantID = "tenant:user:77"
	const chatID = "42"
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: tenantID, ChatID: chatID, DisplayName: "Business chat", Transport: contracts.TransportBusiness})
	repo.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:         "business-old",
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: "business-old",
		SenderID:        "contact",
		MessageText:     "business legacy visible message",
		MessageType:     "text",
		CreatedAt:       base,
		Transport:       contracts.TransportBusiness,
	})
	repo.UpsertSemanticFact(store.SemanticFactRecord{
		TenantID:        tenantID,
		ScopeKind:       "chat",
		ChatID:          chatID,
		FactText:        "business legacy visible fact",
		FactType:        "preference",
		Subject:         "contact",
		Confidence:      0.8,
		Source:          "llm_extracted",
		SourceMessageID: "business-old",
	}, nil)

	svc := newTestService(repo, &fakeAI{}, &fakeSender{})
	parts, records := svc.conversationContextAndMessages(context.Background(), contracts.NormalizedTelegramEvent{
		EventID:         "business-current",
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: "business-current",
		SenderID:        "contact",
		MessageText:     "current business message",
		MessageType:     "text",
		CreatedAt:       base.Add(time.Minute),
		Transport:       contracts.TransportBusiness,
	}, "owner", 20)
	text := strings.Join(parts, "\n")
	assertContainsAll(t, text, "business legacy visible message", "business legacy visible fact")
	if len(records) != 1 {
		t.Fatalf("business records = %+v, want legacy broad message read", records)
	}
}

func saveRoleContextMemory(t *testing.T, repo *store.Repository, tenantID, chatID, botID string, roleContextID int64, prefix string, at time.Time) {
	t.Helper()
	expiresAt := time.Now().Add(24 * time.Hour)
	repo.SaveMessageWithRoleContext(contracts.NormalizedTelegramEvent{
		EventID:         prefix + "-message",
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: prefix + "-message",
		SenderID:        "contact",
		MessageText:     prefix + " visible message",
		MessageType:     "text",
		CreatedAt:       at,
		Transport:       contracts.TransportBot,
		BotID:           botID,
	}, roleContextID)
	for _, fact := range []store.SemanticFactRecord{
		{
			FactText: prefix + " visible fact",
			FactType: "preference",
		},
		{
			FactText:   prefix + " taboo boundary",
			FactType:   "taboo",
			Confidence: 1,
		},
		{
			FactText:  prefix + " commitment",
			FactType:  "commitment",
			ExpiresAt: &expiresAt,
		},
	} {
		if fact.Confidence == 0 {
			fact.Confidence = 0.8
		}
		fact.TenantID = tenantID
		fact.ScopeKind = "chat"
		fact.ChatID = chatID
		fact.Subject = "contact"
		fact.Source = "llm_extracted"
		fact.SourceMessageID = prefix + "-message"
		fact.RoleContextID = roleContextID
		repo.UpsertSemanticFact(fact, nil)
	}
	repo.SaveEpisodeSummary(store.EpisodeSummaryRecord{
		TenantID:      tenantID,
		ChatID:        chatID,
		PeriodStart:   at.Add(-time.Hour),
		PeriodEnd:     at.Add(-30 * time.Minute),
		Content:       prefix + " episode summary",
		MessageCount:  3,
		RoleContextID: roleContextID,
	})
	repo.SaveDraft(store.DraftRecord{
		DraftID:           "draft:" + prefix,
		DecisionID:        prefix + "-decision",
		TenantID:          tenantID,
		ChatID:            chatID,
		SourceMessageID:   prefix + "-message",
		SourceMessageText: prefix + " source",
		ResponseText:      prefix + " pending draft",
		Status:            store.DraftPending,
		CreatedAt:         at,
		UpdatedAt:         at,
		RoleContextID:     roleContextID,
	})
	repo.UpsertActiveEpisode(store.ActiveEpisodeRecord{
		TenantID:      tenantID,
		ChatID:        chatID,
		StartedAt:     at,
		LastMessageAt: at,
		MessageCount:  1,
		CurrentTopic:  prefix + " active topic",
		RoleContextID: roleContextID,
	})
}

func renderedConversationContext(t *testing.T, svc *Service, tenantID, chatID, botID, sourceMessageID string) (string, []store.MessageRecord) {
	t.Helper()
	parts, records := svc.conversationContextAndMessages(context.Background(), contracts.NormalizedTelegramEvent{
		EventID:         sourceMessageID,
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: sourceMessageID,
		SenderID:        "contact",
		MessageText:     "current role scoped message",
		MessageType:     "text",
		CreatedAt:       time.Date(2026, 6, 30, 13, 0, 0, 0, time.UTC),
		Transport:       contracts.TransportBot,
		BotID:           botID,
	}, "owner", 20)
	return strings.Join(parts, "\n"), records
}

func assertContainsAll(t *testing.T, text string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			t.Fatalf("context missing %q\ncontext:\n%s", needle, text)
		}
	}
}

func assertContainsNone(t *testing.T, text string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			t.Fatalf("context unexpectedly contains %q\ncontext:\n%s", needle, text)
		}
	}
}
