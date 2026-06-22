package store

import (
	"os"
	"strings"
	"testing"
)

func TestMigration113ToolSettingsIsIdempotentAndReversible(t *testing.T) {
	up, err := os.ReadFile("../../../infra/migrations/113_ai_role_tool_settings.sql")
	if err != nil {
		t.Fatalf("read migration 113 up: %v", err)
	}
	down, err := os.ReadFile("../../../infra/migrations/113_ai_role_tool_settings.down.sql")
	if err != nil {
		t.Fatalf("read migration 113 down: %v", err)
	}
	upSQL := string(up)
	if !strings.Contains(upSQL, "ADD COLUMN IF NOT EXISTS tool_settings") ||
		!strings.Contains(upSQL, "jsonb NOT NULL DEFAULT '{}'::jsonb") {
		t.Fatalf("migration 113 up does not define idempotent tool_settings jsonb: %s", upSQL)
	}
	if !strings.Contains(string(down), "DROP COLUMN IF EXISTS tool_settings") {
		t.Fatalf("migration 113 down is not idempotent: %s", down)
	}
}
