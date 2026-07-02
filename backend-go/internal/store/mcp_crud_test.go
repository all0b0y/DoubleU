package store

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// mcpCRUDKey is a fixed 32-byte AES-256 key for the CRUD encryption paths.
func mcpCRUDKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}

// Э1.1: the owner-facing MCP CRUD store methods round-trip, isolate by tenant,
// and never surface the auth header value. Gated on a real Postgres.
func TestMCPServerCRUD(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	tenant := fmt.Sprintf("tenant:mcpcrud:%d", time.Now().UnixNano())
	other := tenant + ":other"
	if _, err := db.Exec(
		`INSERT INTO tenants (id,name) VALUES ($1,'t'),($2,'t') ON CONFLICT DO NOTHING`,
		tenant, other,
	); err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM ai_roles WHERE tenant_id IN ($1,$2)`, tenant, other)
		_, _ = db.Exec(`DELETE FROM mcp_servers WHERE tenant_id IN ($1,$2)`, tenant, other)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id IN ($1,$2)`, tenant, other)
	})

	repo, err := NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	repo.SetSecretsKey(mcpCRUDKey())

	id, err := repo.SaveMCPServer(tenant, "srv", "http", "https://x/mcp", "Bearer a")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// List: one record, HasAuth true, no header field exists on the struct.
	recs := repo.MCPServersForTenant(tenant)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d: %+v", len(recs), recs)
	}
	if recs[0].ID != id || recs[0].Name != "srv" || !recs[0].HasAuth || !recs[0].Enabled {
		t.Fatalf("record = %+v", recs[0])
	}

	// Empty authHeader on update must preserve the stored secret.
	if err := repo.UpdateMCPServer(tenant, id, "srv2", "sse", "https://y/mcp", "", false); err != nil {
		t.Fatalf("update: %v", err)
	}
	recs = repo.MCPServersForTenant(tenant)
	if recs[0].Name != "srv2" || recs[0].Transport != "sse" || recs[0].URL != "https://y/mcp" ||
		recs[0].Enabled || !recs[0].HasAuth {
		t.Fatalf("after metadata update: %+v", recs[0])
	}

	// A non-empty header updates and re-encrypts; verify it decrypts on read.
	role := repo.SaveAIRole(AIRoleRecord{TenantID: tenant, Name: "r", ModelTier: "balanced"})
	repo.SetRoleMCPAccess(role.ID, id, []string{"search"})
	if err := repo.UpdateMCPServer(tenant, id, "srv2", "sse", "https://y/mcp", "Bearer b", true); err != nil {
		t.Fatalf("update header: %v", err)
	}
	specs := repo.RoleMCPServers(role.ID)
	if len(specs) != 1 || specs[0].AuthHeader != "Bearer b" {
		t.Fatalf("header update not reflected: %+v", specs)
	}

	// RoleMCPAccessByRole returns the grant with its allow-list.
	access := repo.RoleMCPAccessByRole(role.ID)
	if len(access) != 1 || access[0].ServerID != id ||
		len(access[0].AllowedTools) != 1 || access[0].AllowedTools[0] != "search" {
		t.Fatalf("access = %+v", access)
	}

	// Tenant isolation: another tenant cannot delete or update this row.
	if err := repo.DeleteMCPServer(other, id); err == nil {
		t.Fatal("cross-tenant delete must error")
	}
	if err := repo.UpdateMCPServer(other, id, "x", "http", "https://z/mcp", "", true); err == nil {
		t.Fatal("cross-tenant update must error")
	}
	if len(repo.MCPServersForTenant(tenant)) != 1 {
		t.Fatal("row must survive cross-tenant delete/update")
	}

	// Owner delete succeeds and cascades the grant.
	if err := repo.DeleteMCPServer(tenant, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(repo.MCPServersForTenant(tenant)) != 0 {
		t.Fatal("row must be gone after owner delete")
	}
	if len(repo.RoleMCPAccessByRole(role.ID)) != 0 {
		t.Fatal("grant must cascade-delete with the server")
	}
}
