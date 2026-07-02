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

// testSecretsKey is a fixed 32-byte AES-256 key for the encryption round-trip.
func testSecretsKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// RoleMCPServers must return a role's enabled servers with the per-role tool
// allow-list (#7): NULL allow-list => all tools, a set => exactly those. The
// auth header round-trips through at-rest encryption (Э0.3): a direct SELECT
// shows NULL plaintext and a populated auth_header_enc. Gated on a real
// Postgres (DATABASE_URL_TEST).
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
	repo.SetSecretsKey(testSecretsKey())
	role := repo.SaveAIRole(store.AIRoleRecord{TenantID: tenant, Name: "r", ModelTier: "balanced"})
	if role.ID == 0 {
		t.Fatal("SaveAIRole returned no id")
	}

	openID, err := repo.SaveMCPServer(tenant, "open", "http", "https://x/mcp", "")
	if err != nil {
		t.Fatalf("save open: %v", err)
	}
	scopedID, err := repo.SaveMCPServer(tenant, "scoped", "sse", "https://y/sse", "Bearer z")
	if err != nil {
		t.Fatalf("save scoped: %v", err)
	}
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

	// At rest the header is encrypted: plaintext column NULL, enc populated.
	var plaintext sql.NullString
	var enc []byte
	if err := db.QueryRow(
		`SELECT auth_header, auth_header_enc FROM mcp_servers WHERE tenant_id=$1 AND name='scoped'`,
		tenant,
	).Scan(&plaintext, &enc); err != nil {
		t.Fatalf("select scoped: %v", err)
	}
	if plaintext.Valid && plaintext.String != "" {
		t.Fatalf("plaintext auth_header must be NULL, got %q", plaintext.String)
	}
	if len(enc) == 0 {
		t.Fatal("auth_header_enc must be populated")
	}
	if string(enc) == "Bearer z" {
		t.Fatal("auth_header_enc must not equal the plaintext header")
	}
}

// SaveMCPServer must fail closed: a non-empty auth header with no configured
// encryption key is an error, never a plaintext write (Э0.3).
func TestSaveMCPServerFailsClosedWithoutKey(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	tenant := fmt.Sprintf("tenant:mcp:nokey:%d", time.Now().UnixNano())
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	mustExec(t, db, `INSERT INTO tenants (id, name) VALUES ($1,'t') ON CONFLICT DO NOTHING`, tenant)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM mcp_servers WHERE tenant_id=$1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id=$1`, tenant)
	})

	repo, err := store.NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	// No SetSecretsKey → key is nil.
	id, err := repo.SaveMCPServer(tenant, "needs-auth", "http", "https://x/mcp", "Bearer secret")
	if err == nil {
		t.Fatal("expected fail-closed error when saving a header with no key")
	}
	if id != 0 {
		t.Fatalf("expected id 0 on failure, got %d", id)
	}
	// An empty header is fine without a key (nothing to protect).
	if _, err := repo.SaveMCPServer(tenant, "no-auth", "http", "https://x/mcp", ""); err != nil {
		t.Fatalf("empty-header save should succeed without a key: %v", err)
	}
}

// A legacy row whose secret still lives in the plaintext auth_header column
// (pre-encryption) keeps being read after Э0.3 — backward compatibility.
func TestRoleMCPServersReadsLegacyPlaintext(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	tenant := fmt.Sprintf("tenant:mcp:legacy:%d", time.Now().UnixNano())
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
	repo.SetSecretsKey(testSecretsKey())
	role := repo.SaveAIRole(store.AIRoleRecord{TenantID: tenant, Name: "r", ModelTier: "balanced"})

	// Simulate a pre-encryption row: plaintext auth_header, no enc.
	var legacyID int64
	if err := db.QueryRow(
		`INSERT INTO mcp_servers (tenant_id, name, transport, url, auth_header, enabled)
		 VALUES ($1,'legacy','http','https://legacy/mcp','Bearer legacy', true) RETURNING id`,
		tenant,
	).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	repo.SetRoleMCPAccess(role.ID, legacyID, nil)

	specs := repo.RoleMCPServers(role.ID)
	if len(specs) != 1 || specs[0].AuthHeader != "Bearer legacy" {
		t.Fatalf("legacy plaintext header must still read, got %+v", specs)
	}
}
