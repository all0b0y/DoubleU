package store

import (
	"errors"
	"os"
	"strings"
	"testing"
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
