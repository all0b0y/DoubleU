package store

import (
	"testing"

	"doubleu/backend-go/internal/contracts"
)

// TestUserBotLifecycle covers registration, offset/status updates and removal
// of a tenant's user-supplied bot in memory mode.
func TestUserBotLifecycle(t *testing.T) {
	repo := NewRepository()

	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret"})
	bot, ok := repo.UserBotByTenant("tenant:user:77")
	if !ok || bot.Status != "active" || bot.BotID != 555 {
		t.Fatalf("unexpected saved bot: %+v ok=%v", bot, ok)
	}
	if got := len(repo.ActiveUserBots()); got != 1 {
		t.Fatalf("active bots = %d, want 1", got)
	}

	repo.UpdateUserBotOffset(555, 1042)
	if bot, _ = repo.UserBotByTenant("tenant:user:77"); bot.LastUpdateOffset != 1042 {
		t.Fatalf("offset = %d, want 1042", bot.LastUpdateOffset)
	}

	repo.UpdateUserBotStatus(555, "invalid")
	if got := len(repo.ActiveUserBots()); got != 0 {
		t.Fatalf("active bots after invalidation = %d, want 0", got)
	}

	repo.DeleteUserBot("tenant:user:77")
	if _, ok := repo.UserBotByTenant("tenant:user:77"); ok {
		t.Fatal("bot must be gone after delete")
	}
}

// TestTenantIngestionMode covers the per-tenant mode selection and the
// "unset means default" contract (empty result).
//
// TestSetUserBotRole covers binding a role to the bot and clearing it, and that
// the role survives a re-register (SaveUserBot must not clobber ai_role_id).
func TestSetUserBotRole(t *testing.T) {
	repo := NewRepository()
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret"})

	repo.SetUserBotRole("tenant:user:77", 9)
	if bot, _ := repo.UserBotByTenant("tenant:user:77"); bot.AIRoleID != 9 {
		t.Fatalf("bot role = %d, want 9", bot.AIRoleID)
	}

	// Re-registering the same bot (new offset) must preserve the chosen role.
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret2", LastUpdateOffset: 5})
	if bot, _ := repo.UserBotByTenant("tenant:user:77"); bot.AIRoleID != 9 {
		t.Fatalf("bot role after re-register = %d, want 9", bot.AIRoleID)
	}

	repo.SetUserBotRole("tenant:user:77", 0)
	if bot, _ := repo.UserBotByTenant("tenant:user:77"); bot.AIRoleID != 0 {
		t.Fatalf("bot role after clear = %d, want 0", bot.AIRoleID)
	}
}

func TestTenantIngestionMode(t *testing.T) {
	repo := NewRepository()
	if mode := repo.TenantIngestionMode("tenant:user:77"); mode != "" {
		t.Fatalf("unset mode = %q, want empty", mode)
	}
	repo.SetTenantIngestionMode("tenant:user:77", contracts.IngestionModeFull)
	if mode := repo.TenantIngestionMode("tenant:user:77"); mode != contracts.IngestionModeFull {
		t.Fatalf("mode = %q, want full", mode)
	}
}

// TestChatTransportFixedByFirstMessage verifies the reply-channel invariant:
// the transport recorded by the first SaveChat survives later saves.
func TestChatTransportFixedByFirstMessage(t *testing.T) {
	repo := NewRepository()
	repo.SaveChat(ChatRecord{TenantID: "tenant:user:77", ChatID: "42", Transport: contracts.TransportBot})
	// A later save without transport (legacy business caller) must not flip it.
	repo.SaveChat(ChatRecord{TenantID: "tenant:user:77", ChatID: "42"})
	if got := repo.ChatTransport("tenant:user:77", "42"); got != contracts.TransportBot {
		t.Fatalf("transport = %q, want bot", got)
	}
	if got := repo.ChatTransport("tenant:user:77", "missing"); got != "" {
		t.Fatalf("transport for unknown chat = %q, want empty", got)
	}
}
