package store

// SkillCurationCandidate is one skill the autonomous curator may improve: it
// belongs to a role that opted into auto-improvement and has accumulated enough
// fresh usage (with negatives) since it was last curated.
type SkillCurationCandidate struct {
	TenantID       string
	SkillID        int64
	Name           string
	Description    string
	Body           string
	RecentLoads    int
	RecentPositive int
	RecentNegative int
}

// SkillsDueForCuration returns tenant-owned skills of auto-improve roles whose
// usage since the last curator version crosses the thresholds (>= minLoads loads
// AND >= minNegative negatives). DB-only: the curator runs against Postgres.
func (r *Repository) SkillsDueForCuration(minLoads, minNegative, limit int) []SkillCurationCandidate {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`WITH curated AS (
		     SELECT skill_id, MAX(created_at) AS last
		       FROM skill_versions WHERE author = 'curator' GROUP BY skill_id
		 )
		 SELECT s.id, s.tenant_id, s.name, s.description, s.body,
		        COUNT(u.*) FILTER (WHERE u.created_at > COALESCE(c.last, 'epoch'::timestamptz)) AS loads,
		        COUNT(u.*) FILTER (WHERE u.rating = 'up'   AND u.created_at > COALESCE(c.last, 'epoch'::timestamptz)) AS pos,
		        COUNT(u.*) FILTER (WHERE u.rating = 'down' AND u.created_at > COALESCE(c.last, 'epoch'::timestamptz)) AS neg
		   FROM skills s
		   LEFT JOIN curated c ON c.skill_id = s.id
		   LEFT JOIN skill_usage u ON u.skill_id = s.id
		  WHERE s.tenant_id IS NOT NULL AND s.enabled
		    -- EXISTS (not JOIN) so a skill enabled in several auto-improve roles is
		    -- not counted once per role, which would inflate the usage tallies.
		    AND EXISTS (
		        SELECT 1 FROM role_skills rs JOIN ai_roles ar ON ar.id = rs.role_id
		         WHERE rs.skill_id = s.id AND ar.auto_improve_skills
		    )
		  GROUP BY s.id, s.tenant_id, s.name, s.description, s.body, c.last
		 HAVING COUNT(u.*) FILTER (WHERE u.created_at > COALESCE(c.last, 'epoch'::timestamptz)) >= $1
		    AND COUNT(u.*) FILTER (WHERE u.rating = 'down' AND u.created_at > COALESCE(c.last, 'epoch'::timestamptz)) >= $2
		  ORDER BY neg DESC
		  LIMIT $3`,
		minLoads, minNegative, limit,
	)
	if err != nil {
		r.logWriteError("skills_due_for_curation", "", "", err)
		return nil
	}
	defer rows.Close()
	var out []SkillCurationCandidate
	for rows.Next() {
		var c SkillCurationCandidate
		if rows.Scan(&c.SkillID, &c.TenantID, &c.Name, &c.Description, &c.Body,
			&c.RecentLoads, &c.RecentPositive, &c.RecentNegative) == nil {
			out = append(out, c)
		}
	}
	return out
}

// ApplyCuratedSkill applies an autonomously-improved body to a tenant-owned
// skill: it snapshots the pre-change body (revert target), overwrites the live
// body, and records the new body as a 'curator' version (which also anchors the
// "since last curated" window). Returns ErrSkillNotFound if the skill is gone.
func (r *Repository) ApplyCuratedSkill(tenantID string, skillID int64, body string) error {
	if r.db == nil {
		r.SaveSkillVersion(skillID, body, "curator")
		return nil
	}
	var current string
	if err := r.db.QueryRow(
		`SELECT body FROM skills WHERE id=$1 AND tenant_id=$2`, skillID, tenantID,
	).Scan(&current); err != nil {
		return ErrSkillNotFound
	}
	r.SaveSkillVersion(skillID, current, "owner")
	res, err := r.db.Exec(
		`UPDATE skills SET body=$1 WHERE id=$2 AND tenant_id=$3`, body, skillID, tenantID,
	)
	if err != nil {
		r.logWriteError("apply_curated_skill", tenantID, "", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSkillNotFound
	}
	r.SaveSkillVersion(skillID, body, "curator")
	return nil
}
