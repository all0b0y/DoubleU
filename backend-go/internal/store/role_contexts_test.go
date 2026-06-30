package store

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
)

func TestBotRoleContextReuseAndNewPolicies(t *testing.T) {
	repo := NewRepository()
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret"})

	first, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 9, RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("reuse role context: %v", err)
	}
	if first.AIRoleID == nil || *first.AIRoleID != 9 {
		t.Fatalf("context role id = %v, want 9", first.AIRoleID)
	}
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.AIRoleID != 9 || bot.ActiveRoleContextID != first.ID {
		t.Fatalf("bot after first context = %+v, want role 9 active context %d", bot, first.ID)
	}

	reused, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 9, RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("reuse role context again: %v", err)
	}
	if reused.ID != first.ID {
		t.Fatalf("reused context id = %d, want %d", reused.ID, first.ID)
	}

	fresh, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 9, RoleContextPolicyNew)
	if err != nil {
		t.Fatalf("new role context: %v", err)
	}
	if fresh.ID == first.ID {
		t.Fatalf("new_role_context reused id %d", fresh.ID)
	}
	active, ok := repo.ActiveBotRoleContext("tenant:user:77", 555)
	if !ok || active.ID != fresh.ID {
		t.Fatalf("active context = %+v ok=%v, want %d", active, ok, fresh.ID)
	}

	latest, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 9, RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("reuse latest role context: %v", err)
	}
	if latest.ID != fresh.ID {
		t.Fatalf("reuse after new context id = %d, want latest %d", latest.ID, fresh.ID)
	}
}

func TestBotRoleContextNoRoleUsesNilRoleID(t *testing.T) {
	repo := NewRepository()
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret"})

	ctx, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 0, RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("set no-role context: %v", err)
	}
	if ctx.AIRoleID != nil {
		t.Fatalf("no-role context AIRoleID = %v, want nil", *ctx.AIRoleID)
	}
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.AIRoleID != 0 || bot.ActiveRoleContextID != ctx.ID {
		t.Fatalf("bot after no-role context = %+v, want role 0 active context %d", bot, ctx.ID)
	}
}

func TestSetUserBotRoleWithContextPolicyRequiresExplicitPolicy(t *testing.T) {
	repo := NewRepository()
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret"})

	if _, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 9, ""); !errors.Is(err, ErrRoleContextPolicyRequired) {
		t.Fatalf("missing policy error = %v, want ErrRoleContextPolicyRequired", err)
	}
	if _, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 555, 9, "reuse"); !errors.Is(err, ErrInvalidRoleContextPolicy) {
		t.Fatalf("invalid policy error = %v, want ErrInvalidRoleContextPolicy", err)
	}
	if _, err := repo.SetUserBotRoleWithContextPolicy("tenant:user:77", 777, 9, RoleContextPolicyReuse); !errors.Is(err, ErrUserBotNotFound) {
		t.Fatalf("missing bot error = %v, want ErrUserBotNotFound", err)
	}
}

func TestRoleContextTaggedMemoryRecords(t *testing.T) {
	repo := NewRepository()
	const tenantID = "tenant:user:77"
	const botID int64 = 555
	chatID := contracts.BotChatID("555", "42")
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	repo.SaveUserBot(UserBotRecord{TenantID: tenantID, BotID: botID, BotUsername: "helper_bot", Token: "secret"})
	ctxA, err := repo.SetUserBotRoleWithContextPolicy(tenantID, botID, 10, RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("activate role A: %v", err)
	}
	eventA := contracts.NormalizedTelegramEvent{
		EventID:         "event-a",
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: "msg-a",
		MessageText:     "role A message",
		CreatedAt:       now,
		Transport:       contracts.TransportBot,
		BotID:           "555",
	}
	repo.SaveMessageQuarantinedWithRoleContext(eventA, false, ctxA.ID)
	repo.UpsertActiveEpisode(ActiveEpisodeRecord{
		TenantID:      tenantID,
		ChatID:        chatID,
		StartedAt:     now,
		LastMessageAt: now,
		MessageCount:  1,
		RoleContextID: ctxA.ID,
	})
	repo.SaveDraft(DraftRecord{
		DraftID:         "draft-a",
		DecisionID:      "event-a",
		TenantID:        tenantID,
		ChatID:          chatID,
		SourceMessageID: "msg-a",
		ResponseText:    "reply A",
		Status:          DraftPending,
		CreatedAt:       now,
		UpdatedAt:       now,
		RoleContextID:   ctxA.ID,
	})
	repo.SaveEpisodeSummary(EpisodeSummaryRecord{
		TenantID:      tenantID,
		ChatID:        chatID,
		PeriodStart:   now,
		PeriodEnd:     now.Add(time.Minute),
		Content:       "role A summary",
		MessageCount:  1,
		RoleContextID: ctxA.ID,
	})
	first := repo.UpsertSemanticFact(SemanticFactRecord{
		TenantID:        tenantID,
		ScopeKind:       "chat",
		ChatID:          chatID,
		FactText:        "любит зеленый чай после обеда",
		FactType:        "preference",
		Subject:         "contact",
		Confidence:      0.8,
		Source:          "llm_extracted",
		SourceMessageID: "msg-a",
		RoleContextID:   ctxA.ID,
	}, nil)
	if first.Action != "inserted" {
		t.Fatalf("role A fact upsert = %+v, want inserted", first)
	}

	ctxB, err := repo.SetUserBotRoleWithContextPolicy(tenantID, botID, 11, RoleContextPolicyReuse)
	if err != nil {
		t.Fatalf("activate role B: %v", err)
	}
	eventB := eventA
	eventB.EventID = "event-b"
	eventB.SourceMessageID = "msg-b"
	eventB.MessageText = "role B message"
	eventB.CreatedAt = now.Add(time.Minute)
	repo.SaveMessageQuarantinedWithRoleContext(eventB, false, ctxB.ID)
	second := repo.UpsertSemanticFact(SemanticFactRecord{
		TenantID:        tenantID,
		ScopeKind:       "chat",
		ChatID:          chatID,
		FactText:        "любит зеленый чай после обеда",
		FactType:        "preference",
		Subject:         "contact",
		Confidence:      0.8,
		Source:          "llm_extracted",
		SourceMessageID: "msg-b",
		RoleContextID:   ctxB.ID,
	}, nil)
	if second.Action != "inserted" {
		t.Fatalf("same fact in role B upsert = %+v, want inserted in distinct role context", second)
	}

	msgA, ok := repo.Message(tenantID, chatID, "msg-a")
	if !ok || msgA.RoleContextID != ctxA.ID {
		t.Fatalf("role A message = %+v ok=%v, want context %d", msgA, ok, ctxA.ID)
	}
	msgB, ok := repo.Message(tenantID, chatID, "msg-b")
	if !ok || msgB.RoleContextID != ctxB.ID {
		t.Fatalf("role B message = %+v ok=%v, want context %d", msgB, ok, ctxB.ID)
	}
	if ep, ok := repo.ActiveEpisodeByChat(tenantID, chatID); !ok || ep.RoleContextID != ctxA.ID {
		t.Fatalf("active episode = %+v ok=%v, want context %d", ep, ok, ctxA.ID)
	}
	if draft, ok := repo.Draft("draft-a"); !ok || draft.RoleContextID != ctxA.ID {
		t.Fatalf("draft = %+v ok=%v, want context %d", draft, ok, ctxA.ID)
	}
	summaries := repo.EpisodeSummaries(tenantID, chatID, 10)
	if len(summaries) != 1 || summaries[0].RoleContextID != ctxA.ID {
		t.Fatalf("summaries = %+v, want one role A summary", summaries)
	}
	facts := repo.SemanticFacts(tenantID, chatID, 10)
	if len(facts) != 2 {
		t.Fatalf("facts = %+v, want same text stored once per role context", facts)
	}
	seen := map[int64]bool{}
	for _, fact := range facts {
		seen[fact.RoleContextID] = true
	}
	if !seen[ctxA.ID] || !seen[ctxB.ID] {
		t.Fatalf("fact contexts = %+v, want A=%d and B=%d", seen, ctxA.ID, ctxB.ID)
	}
}

func TestMigration117BackfillsBotMemoryWithoutDeletingHistory(t *testing.T) {
	up, err := os.ReadFile("../../../infra/migrations/117_bot_role_contexts.sql")
	if err != nil {
		t.Fatalf("read migration 117 up: %v", err)
	}
	down, err := os.ReadFile("../../../infra/migrations/117_bot_role_contexts.down.sql")
	if err != nil {
		t.Fatalf("read migration 117 down: %v", err)
	}
	upSQL := string(up)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS public.bot_role_contexts",
		"ADD COLUMN IF NOT EXISTS active_role_context_id",
		"ADD COLUMN IF NOT EXISTS role_context_id",
		"brc.ai_role_id IS NULL AND ub.ai_role_id IS NULL",
		"c.chat_id LIKE ('bot:' || ub.bot_id::text || ':%')",
		"UPDATE public.messages m",
		"UPDATE public.semantic_facts sf",
		"UPDATE public.episode_summaries es",
		"UPDATE public.active_episodes ae",
		"UPDATE public.drafts d",
	} {
		if !strings.Contains(upSQL, required) {
			t.Fatalf("migration 117 up missing %q", required)
		}
	}
	for _, forbidden := range []string{"DELETE FROM public.messages", "DELETE FROM public.semantic_facts", "DELETE FROM public.drafts"} {
		if strings.Contains(upSQL, forbidden) {
			t.Fatalf("migration 117 must not delete history, found %q", forbidden)
		}
	}
	downSQL := string(down)
	if !strings.Contains(downSQL, "DROP TABLE IF EXISTS public.bot_role_contexts") ||
		!strings.Contains(downSQL, "DROP COLUMN IF EXISTS role_context_id") ||
		!strings.Contains(downSQL, "DROP COLUMN IF EXISTS active_role_context_id") {
		t.Fatalf("migration 117 down is not idempotent: %s", downSQL)
	}
}

func TestMigration118ScopesSemanticFactUpsertByRoleContext(t *testing.T) {
	up, err := os.ReadFile("../../../infra/migrations/118_role_context_semantic_fact_upsert.sql")
	if err != nil {
		t.Fatalf("read migration 118 up: %v", err)
	}
	down, err := os.ReadFile("../../../infra/migrations/118_role_context_semantic_fact_upsert.down.sql")
	if err != nil {
		t.Fatalf("read migration 118 down: %v", err)
	}
	upSQL := string(up)
	for _, required := range []string{
		"COALESCE(role_context_id, 0::bigint)",
		"p_role_context_id bigint",
		"COALESCE(sf.role_context_id, 0) = COALESCE(p_role_context_id, 0)",
		"role_context_id, fact_text",
		"p_role_context_id, p_fact_text",
	} {
		if !strings.Contains(upSQL, required) {
			t.Fatalf("migration 118 up missing %q", required)
		}
	}
	downSQL := string(down)
	if strings.Contains(downSQL, "p_role_context_id bigint") ||
		!strings.Contains(downSQL, "COALESCE(chat_id, ''::text),\n        fact_type") {
		t.Fatalf("migration 118 down does not restore legacy signature/index")
	}
}
