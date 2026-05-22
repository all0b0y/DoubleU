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
	repository.SaveChat(store.ChatRecord{
		TenantID:             "tenant:bc-1",
		ChatID:               "42",
		DisplayName:          "Customer Alice",
		BusinessConnectionID: "bc-1",
		LastSeenAt:           time.Unix(2, 0).UTC(),
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
