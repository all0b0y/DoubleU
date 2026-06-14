package bot

import (
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func TestHandleChatsCommandShowsObservedChats(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "42",
		DisplayName: "Customer Alice",
		LastSeenAt:  time.Unix(2, 0).UTC(),
	})
	layer := NewLayer(repository)

	response := layer.HandleCommand(telegram.BotCommandUpdate{
		Command:  "/chats",
		SenderID: "77",
	}, "tenant:user:77")

	if response.Reason != "chats_command" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(response.Text, "1. Customer Alice") || !strings.Contains(response.Text, "/mode_auto 1") {
		t.Fatalf("text = %s", response.Text)
	}
	if response.ReplyMarkup == nil {
		t.Fatalf("reply markup is missing")
	}
}

func TestHandleModeCommandUsesListIndex(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "42",
		DisplayName: "Customer Alice",
		LastSeenAt:  time.Unix(2, 0).UTC(),
	})
	layer := NewLayer(repository)

	// manual is always unlocked; tests mode selection mechanics not locking.
	response := layer.HandleCommand(telegram.BotCommandUpdate{
		Command:   "/mode_manual",
		Arguments: "1",
		SenderID:  "77",
	}, "tenant:user:77")

	if response.Reason != "mode_command_updated" {
		t.Fatalf("response = %+v", response)
	}
	if settings := repository.ChatSettings("tenant:user:77", "42"); settings.Mode != contracts.ChatModeManual {
		t.Fatalf("settings = %+v", settings)
	}
	if tenantID, ok := repository.TenantForBusinessConnection("bc-1"); ok && tenantID != "tenant:user:77" {
		t.Fatalf("unexpected binding = %s", tenantID)
	}
}

func TestHandleModeCommandClaimsUnboundBusinessChat(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveUnclaimedChat(store.UnclaimedChatRecord{
		BusinessConnectionID: "bc-1",
		ChatID:               "42",
		DisplayName:          "Customer Alice",
		ChatType:             "private",
		LastMessageAt:        time.Unix(2, 0).UTC(),
		Status:               "pending",
	})
	layer := NewLayer(repository)

	// manual is always unlocked; tests tenant binding mechanics not locking.
	response := layer.HandleCommand(telegram.BotCommandUpdate{
		Command:   "/mode_manual",
		Arguments: "1",
		SenderID:  "77",
	}, "tenant:user:77")

	if response.Reason != "mode_command_updated" {
		t.Fatalf("response = %+v", response)
	}
	if settings := repository.ChatSettings("tenant:user:77", "42"); settings.Mode != contracts.ChatModeManual {
		t.Fatalf("settings = %+v", settings)
	}
	if tenantID, ok := repository.TenantForBusinessConnection("bc-1"); !ok || tenantID != "tenant:user:77" {
		t.Fatalf("tenant binding = %s ok=%v", tenantID, ok)
	}
}

// TestRoleLifecycleViaCommands walks the full owner flow: create a role,
// list it, assign it to a chat, inspect it, then clear it.
func TestRoleLifecycleViaCommands(t *testing.T) {
	repository := store.NewRepository()
	repository.SaveChat(store.ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "42",
		DisplayName: "Customer Alice",
		LastSeenAt:  time.Unix(2, 0).UTC(),
	})
	layer := NewLayer(repository)
	command := func(cmd, args string) CommandResponse {
		return layer.HandleCommand(telegram.BotCommandUpdate{
			Command: cmd, Arguments: args, SenderID: "77",
		}, "tenant:user:77")
	}

	if resp := command("/role_create", "Менеджер | Личный менеджер, отвечает клиентам | вежливо, кратко | договориться о встрече | не обещать скидки"); resp.Reason != "role_create_saved" {
		t.Fatalf("role_create response = %+v", resp)
	}
	if resp := command("/roles", ""); resp.Reason != "roles_list" || !strings.Contains(resp.Text, "Менеджер") {
		t.Fatalf("roles response = %+v", resp)
	}
	if resp := command("/role_assign", "1 Менеджер"); resp.Reason != "role_assigned" {
		t.Fatalf("role_assign response = %+v", resp)
	}
	role, ok := repository.AIRoleForChat("tenant:user:77", "42")
	if !ok || role.Name != "Менеджер" || role.Constraints != "не обещать скидки" {
		t.Fatalf("assigned role = %+v ok=%v", role, ok)
	}
	if resp := command("/role", "1"); resp.Reason != "role_command" || !strings.Contains(resp.Text, "не обещать скидки") {
		t.Fatalf("role response = %+v", resp)
	}
	if resp := command("/role_clear", "1"); resp.Reason != "role_cleared" {
		t.Fatalf("role_clear response = %+v", resp)
	}
	if _, ok := repository.AIRoleForChat("tenant:user:77", "42"); ok {
		t.Fatal("role still assigned after /role_clear")
	}
}

// TestRoleCreateRequiresNameAndDescription verifies the usage hint is returned
// when the owner omits the mandatory fields.
func TestRoleCreateRequiresNameAndDescription(t *testing.T) {
	layer := NewLayer(store.NewRepository())

	response := layer.HandleCommand(telegram.BotCommandUpdate{
		Command:   "/role_create",
		Arguments: "ТолькоИмя",
		SenderID:  "77",
	}, "tenant:user:77")

	if response.Reason != "role_create_bad_args" || !strings.Contains(response.Text, "/role_create") {
		t.Fatalf("response = %+v", response)
	}
}

func TestHandleModeCommandExplainsEmptyLimitedAccess(t *testing.T) {
	layer := NewLayer(store.NewRepository())

	response := layer.HandleCommand(telegram.BotCommandUpdate{
		Command:   "/mode_auto",
		Arguments: "1",
		SenderID:  "77",
	}, "tenant:user:77")

	if response.Reason != "mode_command_unknown_chat" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(response.Text, "ограниченный доступ") && !strings.Contains(response.Text, "Chat Automation") {
		t.Fatalf("text does not guide limited access: %s", response.Text)
	}
}
