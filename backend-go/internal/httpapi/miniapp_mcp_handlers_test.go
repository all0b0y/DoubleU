package httpapi

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"doubleu/backend-go/internal/core"
	"doubleu/backend-go/internal/store"

	_ "github.com/lib/pq"
)

// Э1.2: boundary validation and tenant-scoping of the MCP CRUD API. These paths
// run before any DB access, so they exercise fully in memory.
func TestMCPServerHandlersValidation(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil))

	// SSRF guard: a private-IP URL is rejected at the boundary.
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"bad","transport":"http","url":"https://10.0.0.5/mcp"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("private-IP create status=%d body=%s", res.Code, res.Body.String())
	}

	// Invalid transport is rejected.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"bad","transport":"ftp","url":"https://93.184.216.34/mcp"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("bad-transport create status=%d body=%s", res.Code, res.Body.String())
	}

	// Setting MCP on a non-existent role is a 404.
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/roles/999/mcp",
		`{"servers":[]}`)
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown role status=%d body=%s", res.Code, res.Body.String())
	}

	// A real role granted a server it does not own is a 400.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		`{"name":"R","description":"d"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("create role status=%d body=%s", res.Code, res.Body.String())
	}
	var role roleJSON
	decodeBody(t, res, &role)
	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d/mcp", role.ID),
		`{"servers":[{"server_id":12345,"allowed_tools":null}]}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("foreign server_id status=%d body=%s", res.Code, res.Body.String())
	}

	// Create response must never echo the auth header value.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"ok","transport":"http","url":"https://93.184.216.34/mcp","auth_header":"Bearer topsecret"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("valid create status=%d body=%s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "topsecret") || strings.Contains(res.Body.String(), "auth_header") {
		t.Fatalf("create response leaked the auth header: %s", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"has_auth":true`) {
		t.Fatalf("create response should report has_auth=true: %s", res.Body.String())
	}
}

// Э1.2: full CRUD + per-role grant round-trip against a real Postgres, asserting
// the auth header value never appears in any response.
func TestMCPServerHandlersRoundTripDB(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// The Mini App harness authenticates as testTenant; clean its MCP rows.
	if _, err := db.Exec(`INSERT INTO tenants (id,name) VALUES ($1,'t') ON CONFLICT DO NOTHING`, testTenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	name := fmt.Sprintf("srv-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM mcp_servers WHERE tenant_id=$1 AND name=$2`, testTenant, name)
	})

	repo, err := store.NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	repo.SetSecretsKey(key)
	server := NewServer(core.NewService(repo, nil, nil))

	// Create with an auth header.
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/mcp-servers",
		fmt.Sprintf(`{"name":%q,"transport":"http","url":"https://93.184.216.34/mcp","auth_header":"Bearer topsecret"}`, name))
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "topsecret") {
		t.Fatalf("create status=%d body=%s", res.Code, res.Body.String())
	}
	var created mcpServerJSON
	decodeBody(t, res, &created)
	if created.ID == 0 || !created.HasAuth {
		t.Fatalf("created = %+v", created)
	}

	// List reflects it and never exposes the header.
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/mcp-servers", "")
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "topsecret") ||
		strings.Contains(res.Body.String(), "auth_header") {
		t.Fatalf("list status=%d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), name) {
		t.Fatalf("list missing created server: %s", res.Body.String())
	}

	// Grant a role access with an allow-list, then read it back.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles", `{"name":"MCP Role","description":"d"}`)
	var role roleJSON
	decodeBody(t, res, &role)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM ai_roles WHERE id=$1`, role.ID) })

	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d/mcp", role.ID),
		fmt.Sprintf(`{"servers":[{"server_id":%d,"allowed_tools":["search"]}]}`, created.ID))
	if res.Code != http.StatusOK {
		t.Fatalf("set role mcp status=%d body=%s", res.Code, res.Body.String())
	}
	res = miniappRequest(t, server, http.MethodGet, fmt.Sprintf("/api/v1/roles/%d/mcp", role.ID), "")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), fmt.Sprintf(`"server_id":%d`, created.ID)) ||
		!strings.Contains(res.Body.String(), "search") {
		t.Fatalf("get role mcp status=%d body=%s", res.Code, res.Body.String())
	}

	// Delete removes it.
	res = miniappRequest(t, server, http.MethodDelete, fmt.Sprintf("/api/v1/mcp-servers/%d", created.ID), "")
	if res.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", res.Code, res.Body.String())
	}
}
