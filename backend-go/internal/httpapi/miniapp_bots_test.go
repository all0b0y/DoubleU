package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// TestBotsListAndPerBotRole covers the multi-bot HTTP surface: GET /bots lists
// every bot, and PATCH /bots/{bot_id}/role binds a role to one bot without
// touching the tenant's other bots.
func TestBotsListAndPerBotRole(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: testTenant, BotID: 111, BotUsername: "alpha", Token: "t1"})
	repo.SaveUserBot(store.UserBotRecord{TenantID: testTenant, BotID: 222, BotUsername: "beta", Token: "t2"})
	role := repo.SaveAIRole(store.AIRoleRecord{TenantID: testTenant, Name: "Менеджер", Description: "d"})
	server := NewServer(core.NewService(repo, nil, nil))

	res := miniappRequest(t, server, http.MethodGet, "/api/v1/bots", "")
	if res.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", res.Code, res.Body.String())
	}
	var list struct {
		Bots []struct {
			BotID                 int64 `json:"bot_id"`
			RichFormattingEnabled *bool `json:"rich_formatting_enabled"`
			StreamingEnabled      *bool `json:"streaming_enabled"`
			AIRole                *struct {
				ID int64 `json:"id"`
			} `json:"ai_role"`
		} `json:"bots"`
	}
	decodeBody(t, res, &list)
	if len(list.Bots) != 2 {
		t.Fatalf("bots = %d, want 2", len(list.Bots))
	}
	if list.Bots[0].RichFormattingEnabled != nil || list.Bots[0].StreamingEnabled != nil {
		t.Fatalf("bot delivery settings must inherit by default: %+v", list.Bots[0])
	}

	// Role changes require an explicit memory-context policy.
	res = miniappRequest(t, server, http.MethodPatch, "/api/v1/bots/111/role",
		fmt.Sprintf(`{"role_id":%d}`, role.ID))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("set role without policy status = %d body=%s", res.Code, res.Body.String())
	}
	res = miniappRequest(t, server, http.MethodPatch, "/api/v1/bots/111/role",
		fmt.Sprintf(`{"role_id":%d,"role_context_policy":"reuse"}`, role.ID))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("set role with invalid policy status = %d body=%s", res.Code, res.Body.String())
	}

	// Bind the role to bot 111 only, reusing the role's prior memory context.
	res = miniappRequest(t, server, http.MethodPatch, "/api/v1/bots/111/role",
		fmt.Sprintf(`{"role_id":%d,"role_context_policy":"reuse_role_context"}`, role.ID))
	if res.Code != http.StatusOK {
		t.Fatalf("set role status = %d body=%s", res.Code, res.Body.String())
	}
	bot111, _ := repo.UserBotByID(testTenant, 111)
	if bot111.AIRoleID != role.ID {
		t.Fatalf("bot 111 role = %d, want %d", bot111.AIRoleID, role.ID)
	}
	reusedContextID := bot111.ActiveRoleContextID
	if reusedContextID == 0 {
		t.Fatalf("bot 111 active role context was not set: %+v", bot111)
	}
	if b, _ := repo.UserBotByID(testTenant, 222); b.AIRoleID != 0 {
		t.Fatalf("bot 222 role = %d, want 0 (untouched)", b.AIRoleID)
	}

	// A new role context starts clean even for the same role.
	res = miniappRequest(t, server, http.MethodPatch, "/api/v1/bots/111/role",
		fmt.Sprintf(`{"role_id":%d,"role_context_policy":"new_role_context"}`, role.ID))
	if res.Code != http.StatusOK {
		t.Fatalf("set role with new context status = %d body=%s", res.Code, res.Body.String())
	}
	bot111, _ = repo.UserBotByID(testTenant, 111)
	if bot111.AIRoleID != role.ID {
		t.Fatalf("bot 111 role after new context = %d, want %d", bot111.AIRoleID, role.ID)
	}
	newContextID := bot111.ActiveRoleContextID
	if newContextID == 0 || newContextID == reusedContextID {
		t.Fatalf("bot 111 new context = %d, want non-zero distinct from %d", newContextID, reusedContextID)
	}

	// Delivery settings patch must not disturb the bound role.
	res = miniappRequest(t, server, http.MethodPatch, "/api/v1/bots/111/role",
		`{"rich_formatting_enabled":false,"streaming_enabled":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("set delivery status = %d body=%s", res.Code, res.Body.String())
	}
	var updated struct {
		AIRole *struct {
			ID int64 `json:"id"`
		} `json:"ai_role"`
		RichFormattingEnabled *bool `json:"rich_formatting_enabled"`
		StreamingEnabled      *bool `json:"streaming_enabled"`
	}
	decodeBody(t, res, &updated)
	if updated.AIRole == nil || updated.AIRole.ID != role.ID {
		t.Fatalf("bot role changed while updating delivery settings: %+v", updated.AIRole)
	}
	if updated.RichFormattingEnabled == nil || *updated.RichFormattingEnabled || updated.StreamingEnabled == nil || !*updated.StreamingEnabled {
		t.Fatalf("bot delivery settings response = %+v", updated)
	}
	if b, _ := repo.UserBotByID(testTenant, 111); b.AIRoleID != role.ID || b.ActiveRoleContextID != newContextID || b.RichFormattingEnabled == nil || *b.RichFormattingEnabled || b.StreamingEnabled == nil || !*b.StreamingEnabled {
		t.Fatalf("bot 111 after delivery patch = %+v", b)
	}
}
