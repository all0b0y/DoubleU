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

// Э4.7: boundary validation and role checks for the skills API (run in memory).
func TestSkillHandlersValidation(t *testing.T) {
	repo := store.NewRepository()
	server := NewServer(core.NewService(repo, nil, nil))

	// Empty name → 400.
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/skills",
		`{"name":"","description":"d","body":"b"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("empty name status=%d body=%s", res.Code, res.Body.String())
	}
	// Empty description → 400.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/skills",
		`{"name":"n","description":"","body":"b"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("empty description status=%d body=%s", res.Code, res.Body.String())
	}
	// Role skills on a non-existent role → 404.
	res = miniappRequest(t, server, http.MethodPut, "/api/v1/roles/999/skills", `{"skill_ids":[]}`)
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown role status=%d body=%s", res.Code, res.Body.String())
	}
	// A real role granted a skill it cannot see → 400.
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles", `{"name":"R","description":"d"}`)
	var role roleJSON
	decodeBody(t, res, &role)
	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d/skills", role.ID),
		`{"skill_ids":[424242]}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("foreign skill_id status=%d body=%s", res.Code, res.Body.String())
	}
}

// Э4.7: full CRUD + per-role selection round-trip against a real Postgres,
// including that a global skill is read-only through the tenant API.
func TestSkillHandlersRoundTripDB(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO tenants (id,name) VALUES ($1,'t') ON CONFLICT DO NOTHING`, testTenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	name := fmt.Sprintf("skill-%d", time.Now().UnixNano())
	globalName := fmt.Sprintf("global-%d", time.Now().UnixNano())
	var globalID int64
	if err := db.QueryRow(
		`INSERT INTO skills (tenant_id,name,description,body) VALUES (NULL,$1,'g','GB') RETURNING id`, globalName,
	).Scan(&globalID); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM skills WHERE tenant_id=$1`, testTenant)
		_, _ = db.Exec(`DELETE FROM skills WHERE id=$1`, globalID)
	})

	repo, err := store.NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	server := NewServer(core.NewService(repo, nil, nil))

	// Create + list.
	res := miniappRequest(t, server, http.MethodPost, "/api/v1/skills",
		fmt.Sprintf(`{"name":%q,"description":"how to","body":"step 1"}`, name))
	if res.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", res.Code, res.Body.String())
	}
	var created skillJSON
	decodeBody(t, res, &created)
	if created.ID == 0 {
		t.Fatal("created skill has no id")
	}
	res = miniappRequest(t, server, http.MethodGet, "/api/v1/skills", "")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), name) ||
		!strings.Contains(res.Body.String(), globalName) {
		t.Fatalf("list status=%d body=%s", res.Code, res.Body.String())
	}

	// Update own skill.
	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/skills/%d", created.ID),
		fmt.Sprintf(`{"name":%q,"description":"updated","body":"step 2","enabled":true}`, name))
	if res.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", res.Code, res.Body.String())
	}

	// A global skill is read-only through the tenant API: update and delete 404.
	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/skills/%d", globalID),
		`{"name":"x","description":"y","body":"z"}`)
	if res.Code != http.StatusNotFound {
		t.Fatalf("global update must 404, got %d", res.Code)
	}
	res = miniappRequest(t, server, http.MethodDelete, fmt.Sprintf("/api/v1/skills/%d", globalID), "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("global delete must 404, got %d", res.Code)
	}

	// Grant a role its own + the global skill.
	roleName := fmt.Sprintf("Skill Role %d", time.Now().UnixNano())
	res = miniappRequest(t, server, http.MethodPost, "/api/v1/roles",
		fmt.Sprintf(`{"name":%q,"description":"d"}`, roleName))
	if res.Code != http.StatusOK {
		t.Fatalf("create role status=%d body=%s", res.Code, res.Body.String())
	}
	var role roleJSON
	decodeBody(t, res, &role)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM ai_roles WHERE id=$1`, role.ID) })

	res = miniappRequest(t, server, http.MethodPut, fmt.Sprintf("/api/v1/roles/%d/skills", role.ID),
		fmt.Sprintf(`{"skill_ids":[%d,%d]}`, created.ID, globalID))
	if res.Code != http.StatusOK {
		t.Fatalf("set role skills status=%d body=%s", res.Code, res.Body.String())
	}

	// Own delete works.
	res = miniappRequest(t, server, http.MethodDelete, fmt.Sprintf("/api/v1/skills/%d", created.ID), "")
	if res.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", res.Code, res.Body.String())
	}
}
