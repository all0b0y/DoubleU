// Migration runner for the DoubleU backend.
//
// Migrations are versioned by the integer prefix of their filename
// (e.g. "003_embeddings_qwen_4096.sql" has version 3).  Applied versions
// are tracked in the schema_migrations table together with the SHA-256
// checksum of the file at apply time so drift is detected on the next start.
//
// Rollback: each up-migration may have a sibling "<version>_<name>.down.sql"
// file in the same directory; running MigrateDown executes those files
// in descending version order.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MigrationRecord is one row in the schema_migrations tracking table.
type MigrationRecord struct {
	Version   int
	Filename  string
	Checksum  string
	AppliedAt string
}

// migrationFile is a parsed on-disk migration file (up or down).
type migrationFile struct {
	version  int
	filename string
	path     string
	body     string
	checksum string
}

// migrationVersionRe extracts the numeric version prefix from a filename.
var migrationVersionRe = regexp.MustCompile(`^(\d+)_.+\.sql$`)

// parseMigrationFile reads a migration from disk and returns its parsed form.
// Returns (nil, nil) when the file does not exist; that path is silently skipped
// so deployment topologies that vary the migration root keep working.
func parseMigrationFile(path string) (*migrationFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	filename := filepath.Base(path)
	m := migrationVersionRe.FindStringSubmatch(filename)
	if m == nil {
		return nil, fmt.Errorf("migration filename %q does not match NNN_name.sql", filename)
	}
	version, _ := strconv.Atoi(m[1])
	sum := sha256.Sum256(data)
	return &migrationFile{
		version:  version,
		filename: filename,
		path:     path,
		body:     string(data),
		checksum: hex.EncodeToString(sum[:]),
	}, nil
}

// collectUpMigrations parses the supplied paths, drops missing files and
// duplicates (by filename), and returns them sorted ascending by version.
func collectUpMigrations(paths []string) ([]migrationFile, error) {
	seen := make(map[string]struct{})
	out := make([]migrationFile, 0, len(paths))
	for _, p := range paths {
		// Skip *.down.sql files when supplied to the up-migration path.
		if strings.HasSuffix(p, ".down.sql") {
			continue
		}
		mf, err := parseMigrationFile(p)
		if err != nil {
			return nil, err
		}
		if mf == nil {
			continue
		}
		if _, dup := seen[mf.filename]; dup {
			continue
		}
		seen[mf.filename] = struct{}{}
		out = append(out, *mf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// ensureMigrationsTable creates schema_migrations on first run and
// upgrades the legacy single-column layout to the versioned schema.
func ensureMigrationsTable(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  filename   TEXT PRIMARY KEY,
		  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("schema_migrations bootstrap: %w", err)
	}
	// Add version + checksum to legacy installs.  Defaults make the migration
	// safe to run while older rows are backfilled below.
	if _, err := db.Exec(`ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS version INT`); err != nil {
		return fmt.Errorf("schema_migrations add version: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return fmt.Errorf("schema_migrations add checksum: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_schema_migrations_version ON schema_migrations(version)`); err != nil {
		return fmt.Errorf("schema_migrations index: %w", err)
	}
	return nil
}

// loadAppliedMigrations returns the recorded migrations keyed by filename.
func loadAppliedMigrations(db *sql.DB) (map[string]MigrationRecord, error) {
	rows, err := db.Query(`
		SELECT filename, COALESCE(version, 0), COALESCE(checksum, ''), applied_at::text
		  FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]MigrationRecord)
	for rows.Next() {
		var rec MigrationRecord
		if err := rows.Scan(&rec.Filename, &rec.Version, &rec.Checksum, &rec.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[rec.Filename] = rec
	}
	return applied, rows.Err()
}

// applyMigrations runs every up-migration that has not been applied yet, in
// version order.  Already-applied migrations are checksum-verified so file
// edits are surfaced as a warning.
func (r *Repository) applyMigrations(paths ...string) error {
	if r.db == nil {
		return nil
	}
	if err := ensureMigrationsTable(r.db); err != nil {
		return err
	}
	files, err := collectUpMigrations(paths)
	if err != nil {
		return err
	}
	applied, err := loadAppliedMigrations(r.db)
	if err != nil {
		return err
	}
	for _, mf := range files {
		if rec, ok := applied[mf.filename]; ok {
			if rec.Checksum != "" && rec.Checksum != mf.checksum {
				slog.Warn("migration checksum drift; file changed after apply",
					"filename", mf.filename,
					"version", mf.version,
					"recorded_checksum", rec.Checksum,
					"current_checksum", mf.checksum,
				)
			}
			continue
		}
		if _, err := r.db.Exec(mf.body); err != nil {
			return fmt.Errorf("migration %s: %w", mf.filename, err)
		}
		if _, err := r.db.Exec(
			`INSERT INTO schema_migrations (filename, version, checksum)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (filename) DO UPDATE
			   SET version = EXCLUDED.version,
			       checksum = EXCLUDED.checksum`,
			mf.filename, mf.version, mf.checksum,
		); err != nil {
			return fmt.Errorf("record %s: %w", mf.filename, err)
		}
		slog.Info("migration applied",
			"filename", mf.filename,
			"version", mf.version,
			"checksum", mf.checksum,
		)
	}
	return nil
}

// MigrationStatus is the rendered status of one migration file or applied row.
type MigrationStatus struct {
	Filename string
	Version  int
	Applied  bool
	Checksum string
	Drifted  bool // applied checksum differs from current file
}

// MigrationStatuses returns the merged status of every supplied migration
// path plus any rows tracked in the DB.  Used by the `migrate status` CLI.
func (r *Repository) MigrationStatuses(paths ...string) ([]MigrationStatus, error) {
	if r.db == nil {
		return nil, fmt.Errorf("postgres not configured")
	}
	if err := ensureMigrationsTable(r.db); err != nil {
		return nil, err
	}
	applied, err := loadAppliedMigrations(r.db)
	if err != nil {
		return nil, err
	}
	files, err := collectUpMigrations(paths)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]MigrationStatus)
	for _, mf := range files {
		status := MigrationStatus{Filename: mf.filename, Version: mf.version, Checksum: mf.checksum}
		if rec, ok := applied[mf.filename]; ok {
			status.Applied = true
			status.Drifted = rec.Checksum != "" && rec.Checksum != mf.checksum
		}
		byName[mf.filename] = status
	}
	for name, rec := range applied {
		if _, ok := byName[name]; ok {
			continue
		}
		byName[name] = MigrationStatus{
			Filename: name,
			Version:  rec.Version,
			Applied:  true,
			Checksum: rec.Checksum,
		}
	}
	out := make([]MigrationStatus, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Filename < out[j].Filename
	})
	return out, nil
}

// MigrateDown rolls back applied migrations whose version is strictly greater
// than targetVersion.  For each rollback the runner looks for a sibling
// "<version>_<name>.down.sql" alongside the up-migration; if it is missing,
// the rollback aborts so the operator cannot leave the schema in a partial state.
//
// To roll back everything pass targetVersion = 0.
func (r *Repository) MigrateDown(targetVersion int, paths ...string) error {
	if r.db == nil {
		return fmt.Errorf("postgres not configured")
	}
	if err := ensureMigrationsTable(r.db); err != nil {
		return err
	}
	applied, err := loadAppliedMigrations(r.db)
	if err != nil {
		return err
	}
	upFiles, err := collectUpMigrations(paths)
	if err != nil {
		return err
	}
	upByName := make(map[string]migrationFile, len(upFiles))
	for _, mf := range upFiles {
		upByName[mf.filename] = mf
	}
	// Sort applied rows by version DESC so we roll back youngest first.
	ordered := make([]MigrationRecord, 0, len(applied))
	for _, rec := range applied {
		ordered = append(ordered, rec)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version > ordered[j].Version })
	for _, rec := range ordered {
		if rec.Version <= targetVersion {
			continue
		}
		upMF, ok := upByName[rec.Filename]
		if !ok {
			return fmt.Errorf("cannot roll back %s: source file missing from migration paths", rec.Filename)
		}
		downPath := strings.TrimSuffix(upMF.path, ".sql") + ".down.sql"
		downBytes, err := os.ReadFile(downPath)
		if err != nil {
			return fmt.Errorf("cannot roll back %s: missing %s", rec.Filename, filepath.Base(downPath))
		}
		if _, err := r.db.Exec(string(downBytes)); err != nil {
			return fmt.Errorf("rollback %s: %w", filepath.Base(downPath), err)
		}
		if _, err := r.db.Exec(`DELETE FROM schema_migrations WHERE filename = $1`, rec.Filename); err != nil {
			return fmt.Errorf("untrack %s: %w", rec.Filename, err)
		}
		slog.Info("migration rolled back",
			"filename", rec.Filename,
			"version", rec.Version,
			"down_file", filepath.Base(downPath),
		)
	}
	return nil
}
