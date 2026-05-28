package store

import (
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
)

func TestRepositoryStateAndIdempotency(t *testing.T) {
	repository := NewRepository()
	settings := contracts.ChatSettings{TenantID: "t", ChatID: "c", Mode: contracts.ChatModeAuto}
	repository.SaveChatSettings(settings)
	if got := repository.ChatSettings("t", "c"); got.Mode != contracts.ChatModeAuto {
		t.Fatalf("settings = %+v", got)
	}
	if got := repository.ChatSettings("missing", "c"); got.Mode != contracts.ChatModeManual {
		t.Fatalf("default settings = %+v", got)
	}

	if repository.MarkReceive("r1") || !repository.MarkReceive("r1") {
		t.Fatalf("receive idempotency failed")
	}
	if repository.MarkCallback("c1") || !repository.MarkCallback("c1") {
		t.Fatalf("callback idempotency failed")
	}
	if repository.MarkSend(SendRecord{Key: "s1"}) || !repository.MarkSend(SendRecord{Key: "s1"}) {
		t.Fatalf("send idempotency failed")
	}
	if repository.SendCount() != 1 {
		t.Fatalf("send count = %d", repository.SendCount())
	}
}

func TestRepositoryMessagesAuditsAndDrafts(t *testing.T) {
	repository := NewRepository()
	event := contracts.NormalizedTelegramEvent{EventID: "e1", TenantID: "t", ChatID: "c", SourceMessageID: "m"}
	repository.AppendState("t", "c", "m", contracts.StateReceived)
	if _, ok := repository.Message("t", "c", "m"); ok {
		t.Fatalf("message content should not exist before SaveMessage")
	}
	repository.SaveMessage(event)
	repository.AppendState("t", "c", "m", contracts.StateNormalized)
	message, ok := repository.Message("t", "c", "m")
	if !ok || len(message.States) != 2 {
		t.Fatalf("message = %+v ok=%v", message, ok)
	}

	audit := contracts.AuditEvent{AuditID: "a1", TenantID: "t", ChatID: "c", SourceMessageID: "m", Decision: contracts.DecisionDraft}
	repository.AppendAudit(audit)
	if audits := repository.Audits(); len(audits) != 1 || audits[0].AuditID != "a1" {
		t.Fatalf("audits = %+v", audits)
	}

	draft := DraftRecord{DraftID: "d1", TenantID: "t", ChatID: "c", SourceMessageID: "m", Status: DraftPending, CreatedAt: time.Now()}
	repository.SaveDraft(draft)
	if got, ok := repository.Draft("d1"); !ok || got.Status != DraftPending {
		t.Fatalf("draft = %+v ok=%v", got, ok)
	}
	if got, ok := repository.DraftByMessage("t", "c", "m"); !ok || got.DraftID != "d1" {
		t.Fatalf("draft by message = %+v ok=%v", got, ok)
	}
	draft.Status = DraftRejected
	repository.UpdateDraft(draft)
	if got, _ := repository.Draft("d1"); got.Status != DraftRejected {
		t.Fatalf("updated draft = %+v", got)
	}
}

func TestRepositoryBusinessConnectionAndTenantSettings(t *testing.T) {
	repository := NewRepository()
	repository.SaveBusinessConnection(BusinessConnectionRecord{
		ConnectionID: "bc-1",
		TenantID:     "tenant:user:77",
		UserID:       "77",
		UserChatID:   "88",
		CanReply:     true,
		IsEnabled:    true,
	})
	if got, ok := repository.BusinessConnection("bc-1"); !ok || got.TenantID != "tenant:user:77" {
		t.Fatalf("business connection = %+v ok=%v", got, ok)
	}
	if got, ok := repository.BusinessConnectionByUser("77"); !ok || got.ConnectionID != "bc-1" {
		t.Fatalf("business connection by user = %+v ok=%v", got, ok)
	}

	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "chat-a", Mode: contracts.ChatModeManual})
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "tenant:user:77", ChatID: "chat-b", Mode: contracts.ChatModeAuto})
	repository.SaveChatSettings(contracts.ChatSettings{TenantID: "other", ChatID: "chat-c", Mode: contracts.ChatModeAuto})
	if got := repository.ChatSettingsForTenant("tenant:user:77"); len(got) != 2 {
		t.Fatalf("settings count = %d", len(got))
	}
}

func TestRepositoryObservedChatsResolveByIDAndListIndex(t *testing.T) {
	repository := NewRepository()
	repository.SaveChat(ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "old",
		DisplayName: "Old Chat",
		LastSeenAt:  time.Unix(1, 0).UTC(),
	})
	repository.SaveChat(ChatRecord{
		TenantID:    "tenant:user:77",
		ChatID:      "new",
		DisplayName: "New Chat",
		LastSeenAt:  time.Unix(2, 0).UTC(),
	})
	repository.SaveChat(ChatRecord{
		TenantID:    "other",
		ChatID:      "other-chat",
		DisplayName: "Other",
		LastSeenAt:  time.Unix(3, 0).UTC(),
	})

	chats := repository.ChatsForTenant("tenant:user:77")
	if len(chats) != 2 || chats[0].ChatID != "new" || chats[1].ChatID != "old" {
		t.Fatalf("chats = %+v", chats)
	}
	if chat, ok := repository.ChatBySelector("tenant:user:77", "old"); !ok || chat.ChatID != "old" {
		t.Fatalf("chat by id = %+v ok=%v", chat, ok)
	}
	if chat, ok := repository.ChatBySelector("tenant:user:77", "1"); !ok || chat.ChatID != "new" {
		t.Fatalf("chat by index = %+v ok=%v", chat, ok)
	}
	if _, ok := repository.ChatBySelector("tenant:user:77", "3"); ok {
		t.Fatalf("unexpected chat for out-of-range selector")
	}
}

func TestRepositoryOwnerCanClaimUnboundBusinessChat(t *testing.T) {
	repository := NewRepository()
	repository.SaveUnclaimedChat(UnclaimedChatRecord{
		BusinessConnectionID: "bc-1",
		ChatID:               "42",
		DisplayName:          "Customer Alice",
		ChatType:             "private",
		LastMessageAt:        time.Unix(2, 0).UTC(),
		Status:               "pending",
	})

	chats := repository.ChatsForTenant("tenant:user:77")
	if len(chats) != 1 || chats[0].ChatID != "42" {
		t.Fatalf("fallback chats = %+v", chats)
	}
	chat, ok := repository.ChatBySelector("tenant:user:77", "1")
	if !ok || chat.BusinessConnectionID != "bc-1" {
		t.Fatalf("selected chat = %+v ok=%v", chat, ok)
	}
	claimed := repository.ClaimChatForTenant("tenant:user:77", chat)
	if claimed.TenantID != "tenant:user:77" {
		t.Fatalf("claimed chat = %+v", claimed)
	}
	if tenantID, ok := repository.TenantForBusinessConnection("bc-1"); !ok || tenantID != "tenant:user:77" {
		t.Fatalf("tenant binding = %s ok=%v", tenantID, ok)
	}
}
