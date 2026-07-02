package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// Э4.2: the skills store round-trips, isolates tenants (globals are read-only,
// no cross-tenant delete), and RoleSkills returns level-1 metadata without the
// body. Gated on a real Postgres.
func TestSkillsStore(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	tenantA := fmt.Sprintf("tenant:skills:a:%d", time.Now().UnixNano())
	tenantB := tenantA + ":b"
	globalName := fmt.Sprintf("global-%d", time.Now().UnixNano())
	for _, tid := range []string{tenantA, tenantB} {
		if _, err := db.Exec(`INSERT INTO tenants (id,name) VALUES ($1,'t') ON CONFLICT DO NOTHING`, tid); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	var globalID int64
	if err := db.QueryRow(
		`INSERT INTO skills (tenant_id, name, description, body) VALUES (NULL,$1,'global skill','GLOBAL BODY') RETURNING id`,
		globalName,
	).Scan(&globalID); err != nil {
		t.Fatalf("seed global skill: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM ai_roles WHERE tenant_id IN ($1,$2)`, tenantA, tenantB)
		_, _ = db.Exec(`DELETE FROM skills WHERE tenant_id IN ($1,$2)`, tenantA, tenantB)
		_, _ = db.Exec(`DELETE FROM skills WHERE id=$1`, globalID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id IN ($1,$2)`, tenantA, tenantB)
	})

	repo, err := NewPostgresRepository(dsn)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}

	ownID, err := repo.SaveSkill(tenantA, SkillRecord{
		Name: "reminders", Description: "how to phrase reminders", Body: "step 1...", Enabled: true,
	})
	if err != nil || ownID == 0 {
		t.Fatalf("save skill: id=%d err=%v", ownID, err)
	}

	// Tenant A sees its own skill + the global one (flagged read-only).
	listA := repo.SkillsForTenant(tenantA)
	var sawOwn, sawGlobal bool
	for _, s := range listA {
		if s.ID == ownID {
			sawOwn = true
			if s.Global {
				t.Fatal("own skill must not be flagged global")
			}
		}
		if s.ID == globalID {
			sawGlobal = true
			if !s.Global {
				t.Fatal("global skill must be flagged Global")
			}
		}
	}
	if !sawOwn || !sawGlobal {
		t.Fatalf("tenant A list missing rows: own=%v global=%v", sawOwn, sawGlobal)
	}

	// Tenant B sees the global skill but not tenant A's private one.
	for _, s := range repo.SkillsForTenant(tenantB) {
		if s.ID == ownID {
			t.Fatal("tenant B must not see tenant A's skill")
		}
	}

	// A tenant cannot delete a global skill, nor another tenant's skill.
	if err := repo.DeleteSkill(tenantA, globalID); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("deleting a global skill must fail: %v", err)
	}
	if err := repo.DeleteSkill(tenantB, ownID); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("cross-tenant delete must fail: %v", err)
	}

	// RoleSkills returns name+description only (SkillSpec has no body field) for
	// enabled skills; a disabled skill is excluded.
	disabledID, err := repo.SaveSkill(tenantA, SkillRecord{
		Name: "hidden", Description: "disabled", Body: "x", Enabled: false,
	})
	if err != nil {
		t.Fatalf("save disabled: %v", err)
	}
	role := repo.SaveAIRole(AIRoleRecord{TenantID: tenantA, Name: "r", ModelTier: "balanced"})
	if err := repo.SetRoleSkills(role.ID, []int64{ownID, globalID, disabledID}); err != nil {
		t.Fatalf("set role skills: %v", err)
	}
	specs := repo.RoleSkills(role.ID)
	if len(specs) != 2 {
		t.Fatalf("want 2 enabled skills (disabled excluded), got %d: %+v", len(specs), specs)
	}
	for _, sp := range specs {
		if sp.Name == "" || sp.Description == "" {
			t.Fatalf("skill spec missing name/description: %+v", sp)
		}
	}

	// Owner delete of its own skill succeeds and cascades the role link.
	if err := repo.DeleteSkill(tenantA, ownID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
}
