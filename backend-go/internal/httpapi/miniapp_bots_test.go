package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// TestBotsListAndPerBotRole covers the multi-bot HTTP surface: GET /bots lists
// every bot, and PUT /bots/{bot_id}/role binds a role to one bot without
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
			BotID  int64 `json:"bot_id"`
			AIRole *struct {
				ID int64 `json:"id"`
			} `json:"ai_role"`
		} `json:"bots"`
	}
	decodeBody(t, res, &list)
	if len(list.Bots) != 2 {
		t.Fatalf("bots = %d, want 2", len(list.Bots))
	}

	// Bind the role to bot 111 only.
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/bots/111/role",
		fmt.Sprintf(`{"role_id":%d}`, role.ID))
	if res.Code != http.StatusOK {
		t.Fatalf("set role status = %d body=%s", res.Code, res.Body.String())
	}
	if b, _ := repo.UserBotByID(testTenant, 111); b.AIRoleID != role.ID {
		t.Fatalf("bot 111 role = %d, want %d", b.AIRoleID, role.ID)
	}
	if b, _ := repo.UserBotByID(testTenant, 222); b.AIRoleID != 0 {
		t.Fatalf("bot 222 role = %d, want 0 (untouched)", b.AIRoleID)
	}
}
