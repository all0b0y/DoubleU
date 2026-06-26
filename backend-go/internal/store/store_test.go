package store

import (
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
)

func testBoolPtr(value bool) *bool {
	return &value
}

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

// TestAIRoleCustomFieldsRoundTrip verifies custom fields survive save/update
// and come back attached to the role on every read path.
func TestAIRoleCustomFieldsRoundTrip(t *testing.T) {
	repo := NewRepository()
	fields := []contracts.AIRoleCustomField{
		{Title: "Часы работы", Description: "когда отвечаем", Context: "9-18 пн-пт"},
		{Title: "Доставка", Description: "условия"},
	}
	saved := repo.SaveAIRole(AIRoleRecord{TenantID: "t", Name: "Менеджер", Description: "d", CustomFields: fields})
	if saved.ID == 0 {
		t.Fatal("role not saved")
	}

	got, ok := repo.AIRoleByID("t", saved.ID)
	if !ok || len(got.CustomFields) != 2 || got.CustomFields[0].Title != "Часы работы" || got.CustomFields[0].Context != "9-18 пн-пт" {
		t.Fatalf("custom fields not round-tripped: %+v", got.CustomFields)
	}

	got.CustomFields = []contracts.AIRoleCustomField{{Title: "Только одно", Description: "осталось"}}
	if !repo.UpdateAIRole(got) {
		t.Fatal("update failed")
	}
	if after, _ := repo.AIRoleByID("t", saved.ID); len(after.CustomFields) != 1 || after.CustomFields[0].Title != "Только одно" {
		t.Fatalf("custom fields not updated: %+v", after.CustomFields)
	}
}

func TestAIRoleDeliverySettingsRoundTrip(t *testing.T) {
	repo := NewRepository()
	saved := repo.SaveAIRole(AIRoleRecord{
		TenantID:              "t",
		Name:                  "Менеджер",
		Description:           "d",
		RichFormattingEnabled: testBoolPtr(false),
		StreamingEnabled:      testBoolPtr(true),
	})
	if saved.ID == 0 {
		t.Fatal("role not saved")
	}
	got, ok := repo.AIRoleByID("t", saved.ID)
	if !ok || got.RichFormattingEnabled == nil || *got.RichFormattingEnabled || got.StreamingEnabled == nil || !*got.StreamingEnabled {
		t.Fatalf("delivery settings not round-tripped: %+v ok=%v", got, ok)
	}
	got.RichFormattingEnabled = nil
	got.StreamingEnabled = testBoolPtr(false)
	if !repo.UpdateAIRole(got) {
		t.Fatal("update failed")
	}
	after, _ := repo.AIRoleByID("t", saved.ID)
	if after.RichFormattingEnabled != nil || after.StreamingEnabled == nil || *after.StreamingEnabled {
		t.Fatalf("delivery settings not updated: %+v", after)
	}
}

func TestAIRoleModelNameRoundTrip(t *testing.T) {
	repo := NewRepository()
	saved := repo.SaveAIRole(AIRoleRecord{
		TenantID:    "t",
		Name:        "Менеджер",
		Description: "d",
		ModelTier:   "smart",
		ModelName:   "deepseek/model-b",
	})
	if saved.ID == 0 {
		t.Fatal("role not saved")
	}
	got, ok := repo.AIRoleByID("t", saved.ID)
	if !ok || got.ModelTier != "smart" || got.ModelName != "deepseek/model-b" {
		t.Fatalf("model selection not round-tripped: %+v ok=%v", got, ok)
	}
	repo.AssignAIRoleToChat("t", "c", saved.ID)
	assigned, ok := repo.AIRoleForChat("t", "c")
	if !ok || assigned.ModelName != "deepseek/model-b" {
		t.Fatalf("assigned role model name = %+v ok=%v", assigned, ok)
	}
	got.ModelName = "deepseek/model-c"
	if !repo.UpdateAIRole(got) {
		t.Fatal("update failed")
	}
	after, _ := repo.AIRoleByID("t", saved.ID)
	if after.ModelName != "deepseek/model-c" {
		t.Fatalf("model name not updated: %+v", after)
	}
	after.ModelName = ""
	if !repo.UpdateAIRole(after) {
		t.Fatal("clear failed")
	}
	cleared, _ := repo.AIRoleByID("t", saved.ID)
	if cleared.ModelName != "" {
		t.Fatalf("model name not cleared: %+v", cleared)
	}
}

func TestAIRoleToolSettingsRoundTrip(t *testing.T) {
	repo := NewRepository()
	saved := repo.SaveAIRole(AIRoleRecord{
		TenantID: "t",
		Name:     "Менеджер",
		ToolSettings: map[string]bool{
			"memory.search_facts": false,
		},
	})
	if saved.ID == 0 {
		t.Fatal("role not saved")
	}
	saved.ToolSettings["memory.search_facts"] = true
	got, ok := repo.AIRoleByID("t", saved.ID)
	if !ok || got.ToolSettings["memory.search_facts"] {
		t.Fatalf("tool settings not round-tripped independently: %+v ok=%v", got.ToolSettings, ok)
	}
	if !repo.UpdateAIRoleToolSettings("t", saved.ID, map[string]bool{"memory.save_fact": false}) {
		t.Fatal("tool settings update failed")
	}
	after, _ := repo.AIRoleByID("t", saved.ID)
	if len(after.ToolSettings) != 1 || after.ToolSettings["memory.save_fact"] {
		t.Fatalf("tool settings not replaced: %+v", after.ToolSettings)
	}
	encoded := encodeRoleToolSettings(after.ToolSettings)
	decoded := decodeRoleToolSettings(encoded)
	if len(decoded) != 1 || decoded["memory.save_fact"] {
		t.Fatalf("tool settings jsonb round-trip failed: %s -> %+v", encoded, decoded)
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

	draft := DraftRecord{DraftID: "d1", TenantID: "t", ChatID: "c", SourceMessageID: "m", TextFormat: contracts.TextFormatRich, Status: DraftPending, CreatedAt: time.Now()}
	repository.SaveDraft(draft)
	if got, ok := repository.Draft("d1"); !ok || got.Status != DraftPending || got.TextFormat != contracts.TextFormatRich {
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

	repository.SaveUserSettings(contracts.UserSettings{
		TenantID:              "tenant:user:77",
		RichFormattingEnabled: false,
	})
	if got, ok := repository.UserSettings("tenant:user:77"); !ok || got.RichFormattingEnabled {
		t.Fatalf("user settings rich toggle = %+v ok=%v", got, ok)
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

	// Before claim the chat is surfaced as unclaimed in ChatsForTenant so /chats
	// and /mode_* can act on it; it must not yet be marked as claimed.
	if repository.IsChatClaimed("tenant:user:77", "42") {
		t.Fatalf("unclaimed chat must not be marked claimed before ClaimChatForTenant")
	}
	// The chat is visible via UnclaimedChatByConnection (the path used by apiAddChats).
	uc, ok := repository.UnclaimedChatByConnection("bc-1", "42")
	if !ok || uc.BusinessConnectionID != "bc-1" {
		t.Fatalf("UnclaimedChatByConnection = %+v ok=%v", uc, ok)
	}
	chatToAdd := ChatRecord{
		ChatID:               uc.ChatID,
		DisplayName:          uc.DisplayName,
		ChatType:             uc.ChatType,
		BusinessConnectionID: uc.BusinessConnectionID,
	}
	claimed := repository.ClaimChatForTenant("tenant:user:77", chatToAdd)
	if claimed.TenantID != "tenant:user:77" {
		t.Fatalf("claimed chat = %+v", claimed)
	}
	if tenantID, ok := repository.TenantForBusinessConnection("bc-1"); !ok || tenantID != "tenant:user:77" {
		t.Fatalf("tenant binding = %s ok=%v", tenantID, ok)
	}
}

// TestUpsertSemanticFactInMemoryHashDedup verifies the MEM-1 fallback contract:
// without a DB, two upserts of the same normalised text yield one stored fact —
// the second call reports an occurrence instead of inserting a duplicate.
func TestUpsertSemanticFactInMemoryHashDedup(t *testing.T) {
	repo := NewRepository()
	fact := SemanticFactRecord{
		TenantID: "t", ScopeKind: "chat", ChatID: "c",
		FactText: "Предпочитает звонки после обеда",
		FactType: "preference", Subject: "contact", Confidence: 0.8,
	}

	first := repo.UpsertSemanticFact(fact, nil)
	if first.Action != "inserted" || first.FactID == 0 {
		t.Fatalf("first upsert = %+v, want inserted with ID", first)
	}
	// Same fact, different spacing/case → same normalised key.
	fact.FactText = "предпочитает  звонки после обеда"
	second := repo.UpsertSemanticFact(fact, nil)
	if second.Action != "occurrence_added" {
		t.Fatalf("second upsert = %+v, want occurrence_added", second)
	}
	if got := len(repo.SemanticFacts("t", "c", 10)); got != 1 {
		t.Fatalf("stored facts = %d, want 1", got)
	}

	// Different fact_type with the same text must NOT dedup (§5.6).
	fact.FactType = "commitment"
	third := repo.UpsertSemanticFact(fact, nil)
	if third.Action != "inserted" {
		t.Fatalf("third upsert = %+v, want inserted (different fact_type)", third)
	}
}

// TestRecentMessagesUsesPerChatIndex verifies the ARCH-13 in-memory index:
// messages from other chats do not leak in and chronological order holds.
func TestRecentMessagesUsesPerChatIndex(t *testing.T) {
	repo := NewRepository()
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		repo.SaveMessage(contracts.NormalizedTelegramEvent{
			EventID: "e" + string(rune('a'+i)), TenantID: "t", ChatID: "c",
			SourceMessageID: "m" + string(rune('a'+i)),
			MessageText:     "msg", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	repo.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID: "other", TenantID: "t", ChatID: "other-chat",
		SourceMessageID: "x", MessageText: "leak", CreatedAt: base,
	})

	got := repo.RecentMessages("t", "c", 3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Event.CreatedAt.Before(got[i-1].Event.CreatedAt) {
			t.Fatalf("messages out of order")
		}
	}
	for _, m := range got {
		if m.Event.ChatID != "c" {
			t.Fatalf("message from another chat leaked: %+v", m.Event)
		}
	}
}
