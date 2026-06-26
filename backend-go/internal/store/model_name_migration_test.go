package store

import (
	"os"
	"strings"
	"testing"
)

func TestMigration115ModelNameIsIdempotentAndReversible(t *testing.T) {
	up, err := os.ReadFile("../../../infra/migrations/115_ai_role_model_name.sql")
	if err != nil {
		t.Fatalf("read migration 115 up: %v", err)
	}
	down, err := os.ReadFile("../../../infra/migrations/115_ai_role_model_name.down.sql")
	if err != nil {
		t.Fatalf("read migration 115 down: %v", err)
	}
	upSQL := string(up)
	if !strings.Contains(upSQL, "ADD COLUMN IF NOT EXISTS model_name") ||
		!strings.Contains(upSQL, "text NOT NULL DEFAULT ''") {
		t.Fatalf("migration 115 up does not define idempotent model_name text: %s", upSQL)
	}
	if !strings.Contains(string(down), "DROP COLUMN IF EXISTS model_name") {
		t.Fatalf("migration 115 down is not idempotent: %s", down)
	}
}
