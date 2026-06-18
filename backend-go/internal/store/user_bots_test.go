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
	bot, ok := repo.UserBotByID("tenant:user:77", 555)
	if !ok || bot.Status != "active" || bot.BotID != 555 {
		t.Fatalf("unexpected saved bot: %+v ok=%v", bot, ok)
	}
	if got := len(repo.ActiveUserBots()); got != 1 {
		t.Fatalf("active bots = %d, want 1", got)
	}

	repo.UpdateUserBotOffset(555, 1042)
	if bot, _ = repo.UserBotByID("tenant:user:77", 555); bot.LastUpdateOffset != 1042 {
		t.Fatalf("offset = %d, want 1042", bot.LastUpdateOffset)
	}

	repo.UpdateUserBotStatus(555, "invalid")
	if got := len(repo.ActiveUserBots()); got != 0 {
		t.Fatalf("active bots after invalidation = %d, want 0", got)
	}

	repo.DeleteUserBot("tenant:user:77", 555)
	if _, ok := repo.UserBotByID("tenant:user:77", 555); ok {
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

	repo.SetUserBotRole("tenant:user:77", 555, 9)
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.AIRoleID != 9 {
		t.Fatalf("bot role = %d, want 9", bot.AIRoleID)
	}
	repo.SetUserBotDeliverySettings("tenant:user:77", 555, testBoolPtr(false), testBoolPtr(true))
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.RichFormattingEnabled == nil || *bot.RichFormattingEnabled || bot.StreamingEnabled == nil || !*bot.StreamingEnabled {
		t.Fatalf("bot delivery settings = %+v", bot)
	}

	// Re-registering the same bot (new offset) must preserve the chosen role and scoped delivery settings.
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 555, BotUsername: "helper_bot", Token: "secret2", LastUpdateOffset: 5})
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.AIRoleID != 9 {
		t.Fatalf("bot role after re-register = %d, want 9", bot.AIRoleID)
	}
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.RichFormattingEnabled == nil || *bot.RichFormattingEnabled || bot.StreamingEnabled == nil || !*bot.StreamingEnabled {
		t.Fatalf("bot delivery settings after re-register = %+v", bot)
	}

	repo.SetUserBotRole("tenant:user:77", 555, 0)
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.AIRoleID != 0 {
		t.Fatalf("bot role after clear = %d, want 0", bot.AIRoleID)
	}
	repo.SetUserBotDeliverySettings("tenant:user:77", 555, nil, nil)
	if bot, _ := repo.UserBotByID("tenant:user:77", 555); bot.RichFormattingEnabled != nil || bot.StreamingEnabled != nil {
		t.Fatalf("bot delivery settings after clear = %+v", bot)
	}
}

// TestMultipleBotsPerTenant is the multi-bot store contract: a tenant owns
// several bots at once, each with an independent role, and removing one leaves
// the others intact.
func TestMultipleBotsPerTenant(t *testing.T) {
	repo := NewRepository()
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 111, BotUsername: "a", Token: "t1"})
	repo.SaveUserBot(UserBotRecord{TenantID: "tenant:user:77", BotID: 222, BotUsername: "b", Token: "t2"})

	if got := len(repo.UserBotsByTenant("tenant:user:77")); got != 2 {
		t.Fatalf("tenant bots = %d, want 2", got)
	}

	// Per-bot role is independent — setting one must not touch the other.
	repo.SetUserBotRole("tenant:user:77", 111, 9)
	if b, _ := repo.UserBotByID("tenant:user:77", 111); b.AIRoleID != 9 {
		t.Fatalf("bot 111 role = %d, want 9", b.AIRoleID)
	}
	if b, _ := repo.UserBotByID("tenant:user:77", 222); b.AIRoleID != 0 {
		t.Fatalf("bot 222 role = %d, want 0 (independent)", b.AIRoleID)
	}

	// Deleting one bot leaves the other registered.
	repo.DeleteUserBot("tenant:user:77", 111)
	if _, ok := repo.UserBotByID("tenant:user:77", 111); ok {
		t.Fatal("bot 111 must be gone after delete")
	}
	if _, ok := repo.UserBotByID("tenant:user:77", 222); !ok {
		t.Fatal("bot 222 must remain after deleting 111")
	}
	if got := len(repo.UserBotsByTenant("tenant:user:77")); got != 1 {
		t.Fatalf("remaining bots = %d, want 1", got)
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
