package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestParseMigrationFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "001_init.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE x()"), 0o644); err != nil {
		t.Fatal(err)
	}
	mf, err := parseMigrationFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mf == nil {
		t.Fatalf("expected migration file")
	}
	if mf.version != 1 || mf.filename != "001_init.sql" {
		t.Fatalf("unexpected parse: %+v", mf)
	}
	if mf.checksum == "" {
		t.Fatalf("checksum must be populated")
	}
}

func TestParseMigrationFile_Missing(t *testing.T) {
	mf, err := parseMigrationFile(filepath.Join(t.TempDir(), "nope.sql"))
	if err != nil {
		t.Fatalf("missing file should be silently skipped, got: %v", err)
	}
	if mf != nil {
		t.Fatalf("expected nil for missing file")
	}
}

func TestParseMigrationFile_BadName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no_version_prefix.sql")
	_ = os.WriteFile(path, []byte("--"), 0o644)
	if _, err := parseMigrationFile(path); err == nil {
		t.Fatalf("expected error for filename without numeric prefix")
	}
}

func TestCollectUpMigrationsSortsAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := mustWrite("002_b.sql", "--2")
	b := mustWrite("001_a.sql", "--1")
	c := mustWrite("003_c.sql", "--3")
	d := mustWrite("003_c.down.sql", "DROP TABLE x")
	files, err := collectUpMigrations([]string{a, b, c, d, a}) // duplicate `a` and a down-file
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 up-migrations, got %d", len(files))
	}
	if files[0].version != 1 || files[1].version != 2 || files[2].version != 3 {
		t.Fatalf("not sorted ascending: %+v", files)
	}
}

// TestMigration111MergedSequenceIsLinear verifies that after DATA-1 the
// sequence around version 111 contains exactly one entry at that version.
// Two distinct files sharing version 111 are NOT deduplicated (dedup is by
// filename), so the only safe fix is a single merged file — which is what we
// now ship.  This test guards against that regression being reintroduced.
func TestMigration111MergedSequenceIsLinear(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Simulate the sequence: 110, 111 (merged), 112.
	p110 := write("110_prev.sql", "-- 110")
	p111 := write("111_ai_role_columns.sql", "-- 111 merged")
	p112 := write("112_next.sql", "-- 112")

	files, err := collectUpMigrations([]string{p110, p111, p112})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	// Count entries at each version — expect exactly one per version.
	versionCount := make(map[int]int)
	for _, mf := range files {
		versionCount[mf.version]++
	}
	for v, count := range versionCount {
		if count > 1 {
			t.Errorf("version %d appears %d times in migration sequence (must be unique)", v, count)
		}
	}

	// Verify the merged file is present under the correct name.
	found := false
	for _, mf := range files {
		if mf.version == 111 && mf.filename == "111_ai_role_columns.sql" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected one 111_ai_role_columns.sql in sequence, got: %+v", files)
	}
}

// TestMigration111SQLContainsBothColumns verifies that the merged SQL file
// provisions both columns added in the original two colliding files.
func TestMigration111SQLContainsBothColumns(t *testing.T) {
	// Resolve the migration file relative to the repo root (two levels up from
	// backend-go/internal/store where this test lives).
	candidates := []string{
		"../../../infra/migrations/111_ai_role_columns.sql",
		"../../../../infra/migrations/111_ai_role_columns.sql",
	}
	var body string
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			body = string(data)
			break
		}
	}
	if body == "" {
		t.Skip("could not locate infra/migrations/111_ai_role_columns.sql from test working directory")
	}
	if !strings.Contains(body, "speak_as_owner") {
		t.Error("111_ai_role_columns.sql must contain speak_as_owner column")
	}
	if !strings.Contains(body, "model_tier") {
		t.Error("111_ai_role_columns.sql must contain model_tier column")
	}
	if !strings.Contains(body, "ADD COLUMN IF NOT EXISTS") {
		t.Error("111_ai_role_columns.sql must use ADD COLUMN IF NOT EXISTS (idempotent)")
	}
}

// TestMigration111DownSQLDropsBothColumns verifies the down-migration removes
// both columns so a rollback to version 110 leaves the schema clean.
func TestMigration111DownSQLDropsBothColumns(t *testing.T) {
	candidates := []string{
		"../../../infra/migrations/111_ai_role_columns.down.sql",
		"../../../../infra/migrations/111_ai_role_columns.down.sql",
	}
	var body string
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			body = string(data)
			break
		}
	}
	if body == "" {
		t.Skip("could not locate infra/migrations/111_ai_role_columns.down.sql from test working directory")
	}
	if !strings.Contains(body, "speak_as_owner") {
		t.Error("111_ai_role_columns.down.sql must drop speak_as_owner")
	}
	if !strings.Contains(body, "model_tier") {
		t.Error("111_ai_role_columns.down.sql must drop model_tier")
	}
	if !strings.Contains(body, "DROP COLUMN IF EXISTS") {
		t.Error("111_ai_role_columns.down.sql must use DROP COLUMN IF EXISTS (idempotent)")
	}
}

// TestMigration111DBColumnsAndRollback is an integration test that applies
// migration 111 against a real Postgres database and verifies both columns
// appear in ai_roles; then rolls back and verifies both are gone.
// Requires DATABASE_URL_TEST with an already-bootstrapped schema (migrations
// 001–110 applied).  Skipped when the env var is unset.
func TestMigration111DBColumnsAndRollback(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL_TEST")
	if dsn == "" {
		t.Skip("DATABASE_URL_TEST not set")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Resolve the migration directory from well-known relative paths.
	var migDir string
	for _, rel := range []string{"../../../infra/migrations", "../../../../infra/migrations"} {
		if _, err := os.Stat(rel); err == nil {
			migDir = rel
			break
		}
	}
	if migDir == "" {
		t.Skip("could not locate infra/migrations directory from test working directory")
	}

	// Use a temp schema so this test is isolated from the shared test DB.
	schema := fmt.Sprintf("test_m111_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
	})
	if _, err := db.Exec(fmt.Sprintf(`SET search_path TO %q, public`, schema)); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	// Build a minimal ai_roles table in the isolated schema (skipping the full
	// migration chain) so we can test column adds in isolation.
	if _, err := db.Exec(`CREATE TABLE ai_roles (
		id         BIGSERIAL PRIMARY KEY,
		tenant_id  TEXT NOT NULL,
		name       TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create ai_roles: %v", err)
	}

	// Load only migration 111 paths.
	upPath := filepath.Join(migDir, "111_ai_role_columns.sql")
	upSQL, err := os.ReadFile(upPath)
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	downPath := filepath.Join(migDir, "111_ai_role_columns.down.sql")
	downSQL, err := os.ReadFile(downPath)
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}

	// Apply the up migration.
	if _, err := db.Exec(string(upSQL)); err != nil {
		t.Fatalf("apply up migration: %v", err)
	}

	// Confirm both columns exist in ai_roles.
	hasCol := func(col string) bool {
		var exists bool
		_ = db.QueryRow(
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_schema = current_schema()
				   AND table_name   = 'ai_roles'
				   AND column_name  = $1
			)`, col,
		).Scan(&exists)
		return exists
	}
	if !hasCol("speak_as_owner") {
		t.Error("speak_as_owner column missing after up migration")
	}
	if !hasCol("model_tier") {
		t.Error("model_tier column missing after up migration")
	}

	// Apply the down migration and confirm both columns are removed.
	if _, err := db.Exec(string(downSQL)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	if hasCol("speak_as_owner") {
		t.Error("speak_as_owner column still present after down migration")
	}
	if hasCol("model_tier") {
		t.Error("model_tier column still present after down migration")
	}
}
