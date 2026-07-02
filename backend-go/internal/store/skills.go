package store

import (
	"database/sql"
	"errors"
	"time"

	"doubleu/backend-go/internal/contracts"
)

// ErrSkillNotFound signals a save/delete that matched no tenant-owned row —
// a missing id, a cross-tenant attempt, or a global (first-party) skill that a
// tenant may use but not modify.
var ErrSkillNotFound = errors.New("skill not found")

// SkillRecord is one owner-authored (or global first-party) skill. Global is
// true when tenant_id IS NULL — such skills are visible to every tenant but
// read-only through the tenant API (Г4).
type SkillRecord struct {
	ID          int64
	TenantID    string // "" when Global
	Global      bool
	Name        string
	Description string
	Body        string
	SourceURL   string
	ContentHash string
	Enabled     bool
	CreatedAt   time.Time
}

// SaveSkill upserts a tenant-owned skill by (tenant_id, name) and returns its id.
// Global skills (tenant_id NULL) are seeded out-of-band, never through this path.
func (r *Repository) SaveSkill(tenantID string, s SkillRecord) (int64, error) {
	if r.db == nil {
		return 0, nil
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO skills (tenant_id, name, description, body, source_url, content_hash, enabled)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (tenant_id, name)
		 DO UPDATE SET description = EXCLUDED.description, body = EXCLUDED.body,
		               source_url = EXCLUDED.source_url, content_hash = EXCLUDED.content_hash,
		               enabled = EXCLUDED.enabled
		 RETURNING id`,
		tenantID, s.Name, s.Description, s.Body, s.SourceURL, s.ContentHash, s.Enabled,
	).Scan(&id)
	if err != nil {
		r.logWriteError("save_skill", tenantID, "", err)
		return 0, err
	}
	return id, nil
}

// SkillsForTenant returns the skills a tenant may use — its own plus every global
// first-party skill — in one query (Г4). Bodies are included so the owner UI can
// edit them; global rows are flagged read-only.
func (r *Repository) SkillsForTenant(tenantID string) []SkillRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, name, description, body, source_url, content_hash, enabled, created_at
		   FROM skills WHERE tenant_id IS NULL OR tenant_id = $1
		  ORDER BY tenant_id NULLS FIRST, name`,
		tenantID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []SkillRecord
	for rows.Next() {
		var rec SkillRecord
		var tid sql.NullString
		if rows.Scan(&rec.ID, &tid, &rec.Name, &rec.Description, &rec.Body,
			&rec.SourceURL, &rec.ContentHash, &rec.Enabled, &rec.CreatedAt) != nil {
			continue
		}
		rec.Global = !tid.Valid
		rec.TenantID = tid.String
		out = append(out, rec)
	}
	return out
}

// UpdateSkill edits a tenant-owned skill in place by id (Э4.7). Scoped by
// tenant_id, so globals (tenant_id NULL) and other tenants' rows never match;
// an unmatched id/tenant is ErrSkillNotFound.
func (r *Repository) UpdateSkill(tenantID string, id int64, name, description, body string, enabled bool) error {
	if r.db == nil {
		return nil
	}
	res, err := r.db.Exec(
		`UPDATE skills SET name=$1, description=$2, body=$3, enabled=$4
		  WHERE id=$5 AND tenant_id=$6`,
		name, description, body, enabled, id, tenantID,
	)
	if err != nil {
		r.logWriteError("update_skill", tenantID, "", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSkillNotFound
	}
	return nil
}

// DeleteSkill removes a tenant-owned skill. The tenant_id predicate is non-null,
// so global (tenant_id NULL) skills never match and cannot be deleted by a
// tenant; an unmatched id/tenant is an error.
func (r *Repository) DeleteSkill(tenantID string, id int64) error {
	if r.db == nil {
		return nil
	}
	res, err := r.db.Exec(`DELETE FROM skills WHERE id=$1 AND tenant_id=$2`, id, tenantID)
	if err != nil {
		r.logWriteError("delete_skill", tenantID, "", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSkillNotFound
	}
	return nil
}

// SetRoleSkills replaces a role's entire enabled-skill set in one transaction
// (Г4): the UI sends the full desired set, so de-selected skills lose their link.
// Caller validates that each skill is visible to the tenant.
func (r *Repository) SetRoleSkills(roleID int64, skillIDs []int64) error {
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err := tx.Exec(`DELETE FROM role_skills WHERE role_id = $1`, roleID); err != nil {
		return err
	}
	for _, sid := range skillIDs {
		if _, err := tx.Exec(
			`INSERT INTO role_skills (role_id, skill_id) VALUES ($1,$2)
			 ON CONFLICT (role_id, skill_id) DO NOTHING`,
			roleID, sid,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RoleSkills returns the level-1 (name+description) view of a role's enabled
// skills in one JOIN (Г4). The body is deliberately NOT fetched here — it is
// loaded on demand by the load_skill tool, keeping the role wire payload small.
func (r *Repository) RoleSkills(roleID int64) []contracts.SkillSpec {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT s.name, s.description
		   FROM role_skills rs JOIN skills s ON s.id = rs.skill_id
		  WHERE rs.role_id = $1 AND s.enabled
		  ORDER BY s.name`,
		roleID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []contracts.SkillSpec
	for rows.Next() {
		var spec contracts.SkillSpec
		if rows.Scan(&spec.Name, &spec.Description) == nil {
			out = append(out, spec)
		}
	}
	return out
}
