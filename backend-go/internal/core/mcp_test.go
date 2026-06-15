package core

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"

	_ "github.com/lib/pq"
)

// RoleMCPServers must return a role's enabled servers with the per-role tool
// allow-list (#7): NULL allow-list => all tools, a set => exactly those. Gated
// on a real Postgres (DATABASE_URL_TEST).
func TestRoleMCPServersRoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	tenant := fmt.Sprintf("tenant:mcp:%d", time.Now().UnixNano())
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ($1,'t') ON CONFLICT DO NOTHING`, tenant)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM ai_roles WHERE tenant_id=$1`, tenant)
		_, _ = db.Exec(`DELETE FROM mcp_servers WHERE tenant_id=$1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id=$1`, tenant)
	})

	repo, err := store.NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	role := repo.SaveAIRole(store.AIRoleRecord{TenantID: tenant, Name: "r"})
	if role.ID == 0 {
		t.Fatal("SaveAIRole returned no id")
	}

	openID := repo.SaveMCPServer(tenant, "open", "http", "https://x/mcp", "")
	scopedID := repo.SaveMCPServer(tenant, "scoped", "sse", "https://y/sse", "Bearer z")
	if openID == 0 || scopedID == 0 {
		t.Fatal("SaveMCPServer returned 0")
	}
	repo.SetRoleMCPAccess(role.ID, openID, nil)                  // all tools
	repo.SetRoleMCPAccess(role.ID, scopedID, []string{"search"}) // allow-list

	specs := repo.RoleMCPServers(role.ID)
	if len(specs) != 2 {
		t.Fatalf("want 2 specs, got %d: %+v", len(specs), specs)
	}
	byName := map[string]contracts.MCPServerSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if o := byName["open"]; o.Transport != "http" || o.URL != "https://x/mcp" || len(o.AllowedTools) != 0 {
		t.Fatalf("open spec wrong: %+v", o)
	}
	sc := byName["scoped"]
	if sc.Transport != "sse" || sc.AuthHeader != "Bearer z" ||
		len(sc.AllowedTools) != 1 || sc.AllowedTools[0] != "search" {
		t.Fatalf("scoped spec wrong: %+v", sc)
	}
}
