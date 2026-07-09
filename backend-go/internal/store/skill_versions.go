package store

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// SkillVersionRecord is an immutable snapshot of a skill body (migration 124),
// captured on every change so an owner or curator edit can be rolled back.
type SkillVersionRecord struct {
	ID          int64
	SkillID     int64
	Body        string
	ContentHash string
	Author      string // "owner" | "curator"
	CreatedAt   time.Time
}

// skillBodyHash is the content hash used to skip no-op version snapshots.
func skillBodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// SaveSkillVersion snapshots a skill body unless it is identical to the latest
// snapshot (so repeated saves without a body change do not pile up versions).
// author is "owner" or "curator". Returns the new version id (0 when skipped).
func (r *Repository) SaveSkillVersion(skillID int64, body, author string) int64 {
	if author != "owner" && author != "curator" {
		author = "owner"
	}
	hash := skillBodyHash(body)
	if r.db != nil {
		var latest string
		// Skip when the newest version already carries this exact body.
		_ = r.db.QueryRow(
			`SELECT content_hash FROM skill_versions
			  WHERE skill_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`,
			skillID,
		).Scan(&latest)
		if latest == hash {
			return 0
		}
		var id int64
		if err := r.db.QueryRow(
			`INSERT INTO skill_versions (skill_id, body, content_hash, author)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			skillID, body, hash, author,
		).Scan(&id); err != nil {
			r.logWriteError("save_skill_version", "", "", err)
			return 0
		}
		return id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.skillVersions) - 1; i >= 0; i-- {
		if r.skillVersions[i].SkillID == skillID {
			if r.skillVersions[i].ContentHash == hash {
				return 0
			}
			break
		}
	}
	id := int64(len(r.skillVersions) + 1)
	r.skillVersions = append(r.skillVersions, SkillVersionRecord{
		ID: id, SkillID: skillID, Body: body, ContentHash: hash,
		Author: author, CreatedAt: time.Now(),
	})
	return id
}

// SkillVersions lists a skill's snapshots newest first.
func (r *Repository) SkillVersions(skillID int64) []SkillVersionRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, skill_id, body, content_hash, author, created_at
			   FROM skill_versions WHERE skill_id = $1
			  ORDER BY created_at DESC, id DESC`,
			skillID,
		)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []SkillVersionRecord
		for rows.Next() {
			var v SkillVersionRecord
			if rows.Scan(&v.ID, &v.SkillID, &v.Body, &v.ContentHash, &v.Author, &v.CreatedAt) == nil {
				out = append(out, v)
			}
		}
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SkillVersionRecord
	for i := len(r.skillVersions) - 1; i >= 0; i-- {
		if r.skillVersions[i].SkillID == skillID {
			out = append(out, r.skillVersions[i])
		}
	}
	return out
}

// RevertSkillToVersion restores a tenant-owned skill's body to a prior version
// and snapshots the restore as a new version, so it too is undoable. Scoped by
// tenant so globals and other tenants' skills never match.
func (r *Repository) RevertSkillToVersion(tenantID string, skillID, versionID int64) error {
	if r.db == nil {
		return nil
	}
	var body string
	if err := r.db.QueryRow(
		`SELECT body FROM skill_versions WHERE id = $1 AND skill_id = $2`,
		versionID, skillID,
	).Scan(&body); err != nil {
		return ErrSkillNotFound
	}
	// Snapshot the current (pre-revert) body so the revert itself is undoable.
	var current string
	if err := r.db.QueryRow(
		`SELECT body FROM skills WHERE id=$1 AND tenant_id=$2`, skillID, tenantID,
	).Scan(&current); err == nil {
		r.SaveSkillVersion(skillID, current, "owner")
	}
	res, err := r.db.Exec(
		`UPDATE skills SET body = $1 WHERE id = $2 AND tenant_id = $3`,
		body, skillID, tenantID,
	)
	if err != nil {
		r.logWriteError("revert_skill", tenantID, "", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSkillNotFound
	}
	return nil
}
