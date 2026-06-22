package core

import (
	"testing"

	"doubleu/backend-go/internal/store"
)

func TestAIRoleForPropagatesToolSettings(t *testing.T) {
	repo := store.NewRepository()
	role := repo.SaveAIRole(store.AIRoleRecord{
		TenantID: "tenant",
		Name:     "Менеджер",
		ToolSettings: map[string]bool{
			"memory.search_facts": false,
		},
	})
	repo.AssignAIRoleToChat("tenant", "chat", role.ID)
	service := NewService(repo, nil, nil)
	got := service.aiRoleFor("tenant", "chat")
	if got == nil || got.ToolSettings["memory.search_facts"] {
		t.Fatalf("tool settings not propagated to AI contract: %+v", got)
	}
}
