package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// miniappRequest performs an authenticated Mini App API call. With an empty
// bot token the HMAC check is skipped, so `user_id=…` initData authenticates
// as tenant:user:<id> — the same dev fallback the frontend client uses.
func miniappRequest(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("{}")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "tma user_id=777")
	res := httptest.NewRecorder()
	server.ServeHTTP(res, req)
	return res
}

func decodeBody(t *testing.T, res *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(res.Body.Bytes(), out); err != nil {
		t.Fatalf("decode body %q: %v", res.Body.String(), err)
	}
}

const testTenant = "tenant:user:777"

func TestRoleLifecycle(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: "42", DisplayName: "Иван"})
	server := NewServer(core.NewService(repo, nil, nil))

	// Create.
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"Менеджер","description":"Вежливый менеджер по продажам","speaking_style":"коротко","goals":"закрыть сделку","constraints":"не давать скидки"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", res.Code, res.Body.String())
	}
	var created roleJSON
	decodeBody(t, res, &created)
	if created.ID == 0 || created.Name != "Менеджер" {
		t.Fatalf("created role = %+v", created)
	}
	if created.RichFormattingEnabled != nil || created.StreamingEnabled != nil {
		t.Fatalf("new role must inherit delivery settings by default: %+v", created)
	}

	// Duplicate name → 409.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles", `{"name":"менеджер","description":"дубль"}`)
	if res.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d", res.Code)
	}

	// Missing description → 400.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles", `{"name":"Пустой"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d", res.Code)
	}

	// List.
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/roles", "")
	var list struct {
		Roles []roleJSON `json:"roles"`
	}
	decodeBody(t, res, &list)
	if len(list.Roles) != 1 {
		t.Fatalf("roles = %+v, want 1", list.Roles)
	}

	// Rename via update (SaveAIRole would have created a second role here).
	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d", created.ID),
		`{"name":"Старший менеджер","description":"Вежливый менеджер","speaking_style":"коротко"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", res.Code, res.Body.String())
	}
	if roles := repo.AIRoles(testTenant); len(roles) != 1 || roles[0].Name != "Старший менеджер" {
		t.Fatalf("after rename roles = %+v", roles)
	}

	// Assign to chat → settings expose the role.
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/chats/42/role",
		fmt.Sprintf(`{"role_id":%d}`, created.ID))
	if res.Code != http.StatusOK {
		t.Fatalf("assign status = %d body=%s", res.Code, res.Body.String())
	}
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/chats/42/settings", "")
	var settings struct {
		AIRole *struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"ai_role"`
	}
	decodeBody(t, res, &settings)
	if settings.AIRole == nil || settings.AIRole.ID != created.ID {
		t.Fatalf("settings ai_role = %+v", settings.AIRole)
	}

	// Chats list carries the role name for the 🎭 chip.
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/chats", "")
	if !strings.Contains(res.Body.String(), `"ai_role_name":"Старший менеджер"`) {
		t.Fatalf("chats body = %s", res.Body.String())
	}

	// Assigning a missing role → 404.
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/chats/42/role", `{"role_id":999}`)
	if res.Code != http.StatusNotFound {
		t.Fatalf("assign missing role status = %d", res.Code)
	}

	// Delete → role gone and assignment cleared.
	res = miniappRequest(t, server, http.MethodDelete, fmt.Sprintf("/api/v1/roles/%d", created.ID), "")
	if res.Code != http.StatusOK {
		t.Fatalf("delete status = %d", res.Code)
	}
	if roles := repo.AIRoles(testTenant); len(roles) != 0 {
		t.Fatalf("roles after delete = %+v", roles)
	}
	if _, ok := repo.AIRoleForChat(testTenant, "42"); ok {
		t.Fatalf("chat still has a role after role deletion")
	}
}

func TestRoleDeliverySettingsRoundTrip(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"Поддержка","description":"Отвечает по FAQ","rich_formatting_enabled":false,"streaming_enabled":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", res.Code, res.Body.String())
	}
	var created roleJSON
	decodeBody(t, res, &created)
	if created.RichFormattingEnabled == nil || *created.RichFormattingEnabled || created.StreamingEnabled == nil || !*created.StreamingEnabled {
		t.Fatalf("created delivery settings = %+v", created)
	}

	// Old clients that omit new fields must not clobber already saved delivery settings.
	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d", created.ID),
		`{"name":"Поддержка","description":"Отвечает по FAQ","speaking_style":"коротко"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", res.Code, res.Body.String())
	}
	role, ok := repo.AIRoleByID(testTenant, created.ID)
	if !ok || role.RichFormattingEnabled == nil || *role.RichFormattingEnabled || role.StreamingEnabled == nil || !*role.StreamingEnabled {
		t.Fatalf("delivery settings after legacy update = %+v ok=%v", role, ok)
	}

	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d", created.ID),
		`{"name":"Поддержка","description":"Отвечает по FAQ","rich_formatting_enabled":null,"streaming_enabled":false}`)
	if res.Code != http.StatusOK {
		t.Fatalf("clear status = %d body=%s", res.Code, res.Body.String())
	}
	decodeBody(t, res, &created)
	if created.RichFormattingEnabled != nil || created.StreamingEnabled == nil || *created.StreamingEnabled {
		t.Fatalf("delivery settings after clear/update = %+v", created)
	}
}

func TestRoleContextRoundTrip(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"Магазин","description":"Менеджер магазина","context":"Прайс: товар А — 100р, товар Б — 250р"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", res.Code, res.Body.String())
	}
	var created roleJSON
	decodeBody(t, res, &created)
	if created.Context != "Прайс: товар А — 100р, товар Б — 250р" {
		t.Fatalf("context not persisted: %q", created.Context)
	}

	// List reflects the stored context.
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/roles", "")
	if !strings.Contains(res.Body.String(), "товар Б — 250р") {
		t.Fatalf("list missing context: %s", res.Body.String())
	}

	// Oversized context is rejected.
	big := strings.Repeat("a", 20001)
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"Большой","description":"x","context":"`+big+`"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("oversized context status = %d", res.Code)
	}
}

func TestSuggestionAcceptAppliesProfileField(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: "42", DisplayName: "Иван"})
	id := repo.SaveProfileSuggestion(store.ProfileSuggestionRecord{
		TenantID: testTenant, ChatID: "42",
		FieldName: "tone", SuggestedValue: "дружеский", Rationale: "много шуток",
	})
	if id == 0 {
		t.Fatalf("failed to seed suggestion")
	}
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodGet, "/api/v1/suggestions", "")
	body := res.Body.String()
	if !strings.Contains(body, `"field_label":"тон"`) || !strings.Contains(body, `"chat_display_name":"Иван"`) {
		t.Fatalf("suggestions body = %s", body)
	}

	res = miniappRequest(t, server, http.MethodPost, fmt.Sprintf("/api/v1/suggestions/%d/action", id), `{"action":"accept"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("accept status = %d body=%s", res.Code, res.Body.String())
	}
	profile, _ := repo.ContactProfile(testTenant, "42")
	if profile.Tone != "дружеский" {
		t.Fatalf("profile tone = %q, want applied suggestion", profile.Tone)
	}
	if pending := repo.PendingSuggestions(testTenant, ""); len(pending) != 0 {
		t.Fatalf("pending after accept = %+v", pending)
	}

	// Acting twice on the same suggestion → 404 (no longer pending).
	res = miniappRequest(t, server, http.MethodPost, fmt.Sprintf("/api/v1/suggestions/%d/action", id), `{"action":"reject"}`)
	if res.Code != http.StatusNotFound {
		t.Fatalf("second action status = %d", res.Code)
	}
}

func TestFactsListAndDelete(t *testing.T) {
	repo := store.NewRepository()
	factID := repo.SaveSemanticFact(store.SemanticFactRecord{
		TenantID: testTenant, ChatID: "42", ScopeKind: "chat",
		FactText: "любит кофе", FactType: "preference", Subject: "contact",
		Confidence: 0.9, Source: "llm_extracted", CreatedAt: time.Now(),
	})
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodGet, "/api/v1/chats/42/facts", "")
	if !strings.Contains(res.Body.String(), "любит кофе") {
		t.Fatalf("facts body = %s", res.Body.String())
	}

	res = miniappRequest(t, server, http.MethodDelete, fmt.Sprintf("/api/v1/facts/%d", factID), "")
	if res.Code != http.StatusOK {
		t.Fatalf("delete fact status = %d body=%s", res.Code, res.Body.String())
	}
	if facts := repo.SemanticFacts(testTenant, "42", 10); len(facts) != 0 {
		t.Fatalf("facts after delete = %+v", facts)
	}
}

func TestForgetChatKeepsChatRow(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: "42", DisplayName: "Иван"})
	repo.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID: "e1", TenantID: testTenant, ChatID: "42",
		SourceMessageID: "m1", MessageText: "привет", CreatedAt: time.Now(),
	})
	repo.SaveSemanticFact(store.SemanticFactRecord{
		TenantID: testTenant, ChatID: "42", ScopeKind: "chat",
		FactText: "любит кофе", FactType: "preference", Subject: "contact",
		Confidence: 0.9, Source: "llm_extracted", CreatedAt: time.Now(),
	})
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodPost, "/api/v1/chats/42/forget", "")
	if res.Code != http.StatusOK {
		t.Fatalf("forget status = %d body=%s", res.Code, res.Body.String())
	}
	if facts := repo.SemanticFacts(testTenant, "42", 10); len(facts) != 0 {
		t.Fatalf("facts survived forget: %+v", facts)
	}
	if msgs := repo.RecentMessages(testTenant, "42", 10); len(msgs) != 0 {
		t.Fatalf("messages survived forget: %+v", msgs)
	}
	if _, ok := repo.Chat(testTenant, "42"); !ok {
		t.Fatalf("chat row must survive forget")
	}
}

func TestForgetAllRequiresExactPhrase(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: "42"})
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodPost, "/api/v1/forget-all", `{"confirm_phrase":"удалить всё"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("wrong phrase status = %d", res.Code)
	}
	if chats := repo.ChatsForTenant(testTenant); len(chats) != 1 {
		t.Fatalf("chats must survive a refused forget-all")
	}

	res = miniappRequest(t, server, http.MethodPost, "/api/v1/forget-all",
		fmt.Sprintf(`{"confirm_phrase":%q}`, contracts.ForgetAllConfirmationPhrase))
	if res.Code != http.StatusOK {
		t.Fatalf("forget-all status = %d body=%s", res.Code, res.Body.String())
	}
	if chats := repo.ChatsForTenant(testTenant); len(chats) != 0 {
		t.Fatalf("chats after forget-all = %+v", chats)
	}
}

func TestUserSettingsRoundTrip(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil))

	// Defaults before anything is saved.
	res := miniappRequest(t, server, http.MethodGet, "/api/v1/settings/user", "")
	var got contracts.UserSettings
	decodeBody(t, res, &got)
	if got.DefaultModeForNewChats != "manual" || !got.LearningEnabled || !got.RichFormattingEnabled || got.StreamingEnabled {
		t.Fatalf("defaults = %+v", got)
	}

	res = miniappRequest(t, server, http.MethodPut, "/api/v1/settings/user",
		`{"display_name":"Алексей","bio":"продакт","default_mode_for_new_chats":"semi_auto","learning_enabled":false,"rich_formatting_enabled":false,"streaming_enabled":true,"language_for_drafts":"primary"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("save status = %d body=%s", res.Code, res.Body.String())
	}

	res = miniappRequest(t, server, http.MethodGet, "/api/v1/settings/user", "")
	decodeBody(t, res, &got)
	if got.DisplayName != "Алексей" || got.DefaultModeForNewChats != "semi_auto" ||
		got.LearningEnabled || got.RichFormattingEnabled || !got.StreamingEnabled || got.LanguageForDrafts != "primary" {
		t.Fatalf("round-trip settings = %+v", got)
	}

	// Whitelisted enum is enforced.
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/settings/user", `{"default_mode_for_new_chats":"yolo"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d", res.Code)
	}
}

func TestChatSummaryPrivacyAndEmptyBranches(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{TenantID: testTenant, ChatID: "42"})
	server := NewServer(core.NewService(repo, nil, nil))

	// No messages yet.
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/chats/42/summary", "")
	if !strings.Contains(res.Body.String(), `"no_messages":true`) {
		t.Fatalf("empty chat summary body = %s", res.Body.String())
	}

	// no_summary flag blocks summarization.
	repo.SaveChatSettings(contracts.ChatSettings{
		TenantID: testTenant, ChatID: "42",
		Mode: contracts.ChatModeManual, PrivacyFlags: contracts.PrivacyFlags{contracts.PrivacyNoSummary},
	})
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/chats/42/summary", "")
	if !strings.Contains(res.Body.String(), `"blocked_by_privacy":true`) {
		t.Fatalf("blocked summary body = %s", res.Body.String())
	}

	// Messages present but no AI client → graceful "unavailable" fallback.
	repo.SaveChatSettings(contracts.ChatSettings{
		TenantID: testTenant, ChatID: "42", Mode: contracts.ChatModeManual,
	})
	repo.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID: "e1", TenantID: testTenant, ChatID: "42",
		SourceMessageID: "m1", MessageText: "привет", CreatedAt: time.Now(),
	})
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/chats/42/summary", "")
	if !strings.Contains(res.Body.String(), `"unavailable":true`) {
		t.Fatalf("unavailable summary body = %s", res.Body.String())
	}
}

func TestMiniAppRoutesRequireAuth(t *testing.T) {
	server := NewServer(core.NewService(store.NewRepository(), nil, nil))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/roles"},
		{http.MethodPost, "/api/v1/roles"},
		{http.MethodGet, "/api/v1/suggestions"},
		{http.MethodPost, "/api/v1/forget-all"},
		{http.MethodGet, "/api/v1/settings/user"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth = %d, want 401", route.method, route.path, res.Code)
		}
	}
}
