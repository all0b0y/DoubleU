package httpapi

import (
	"net/http"
	"testing"

	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"
)

// TestPersonalBotToggleAndAllowlist covers the personal-bot HTTP surface: the
// PATCH toggle, and the allowlist add/list/remove round-trip.
func TestPersonalBotToggleAndAllowlist(t *testing.T) {
	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: testTenant, BotID: 111, BotUsername: "alpha", Token: "t1"})
	server := NewServer(core.NewService(repo, nil, nil))

	// Default: personal mode off.
	res := miniappRequest(t, server, http.MethodGet, "/api/v1/bots", "")
	var list struct {
		Bots []struct {
			BotID        int64 `json:"bot_id"`
			PersonalMode bool  `json:"personal_mode"`
		} `json:"bots"`
	}
	decodeBody(t, res, &list)
	if len(list.Bots) != 1 || list.Bots[0].PersonalMode {
		t.Fatalf("personal_mode must default off: %+v", list.Bots)
	}

	// Turn personal mode on via the shared bot PATCH endpoint.
	res = miniappRequest(t, server, http.MethodPatch, "/api/v1/bots/111/role", `{"personal_mode":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("enable personal mode status = %d body=%s", res.Code, res.Body.String())
	}
	var updated struct {
		PersonalMode bool `json:"personal_mode"`
	}
	decodeBody(t, res, &updated)
	if !updated.PersonalMode {
		t.Fatalf("response personal_mode = %v, want true", updated.PersonalMode)
	}
	if b, _ := repo.UserBotByID(testTenant, 111); !b.PersonalMode {
		t.Fatalf("stored personal_mode = %v, want true", b.PersonalMode)
	}

	// Allowlist starts empty.
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/bots/111/allowlist", "")
	if res.Code != http.StatusOK {
		t.Fatalf("list allowlist status = %d body=%s", res.Code, res.Body.String())
	}
	var allow struct {
		AllowedUsers []string `json:"allowed_users"`
	}
	decodeBody(t, res, &allow)
	if len(allow.AllowedUsers) != 0 {
		t.Fatalf("allowlist must start empty, got %v", allow.AllowedUsers)
	}

	// Non-numeric ids are rejected (must match event.SenderID, which is numeric).
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/bots/111/allowlist", `{"telegram_user_id":"@vasya"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric allowlist id status = %d, want 400", res.Code)
	}

	// Add a valid id.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/bots/111/allowlist", `{"telegram_user_id":"42"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("add allowlist status = %d body=%s", res.Code, res.Body.String())
	}
	decodeBody(t, res, &allow)
	if len(allow.AllowedUsers) != 1 || allow.AllowedUsers[0] != "42" {
		t.Fatalf("allowlist after add = %v, want [42]", allow.AllowedUsers)
	}
	if !repo.IsBotAllowedUser(testTenant, 111, "42") {
		t.Fatalf("store must report 42 as allowed")
	}

	// Remove it.
	res = miniappRequest(t, server, http.MethodDelete, "/api/v1/bots/111/allowlist/42", "")
	if res.Code != http.StatusOK {
		t.Fatalf("remove allowlist status = %d body=%s", res.Code, res.Body.String())
	}
	decodeBody(t, res, &allow)
	if len(allow.AllowedUsers) != 0 {
		t.Fatalf("allowlist after remove = %v, want empty", allow.AllowedUsers)
	}
}

// TestPersonalBotStubShownIdempotent verifies the once-per-contact marker: the
// first call reports "not shown" (send the stub), later calls report "shown".
func TestPersonalBotStubShownIdempotent(t *testing.T) {
	repo := store.NewRepository()
	if repo.MarkPersonalBotStubShown(testTenant, "bot:555:42") {
		t.Fatalf("first MarkPersonalBotStubShown must report not-yet-shown (false)")
	}
	if !repo.MarkPersonalBotStubShown(testTenant, "bot:555:42") {
		t.Fatalf("second MarkPersonalBotStubShown must report already-shown (true)")
	}
	// A different contact is independent.
	if repo.MarkPersonalBotStubShown(testTenant, "bot:555:43") {
		t.Fatalf("distinct contact must start not-shown")
	}
}
