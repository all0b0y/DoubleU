package store

import (
	"os"
	"path/filepath"
	"testing"
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
