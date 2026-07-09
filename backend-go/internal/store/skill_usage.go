package store

import "time"

// SkillUsageRecord is one load_skill invocation. rating is filled post-hoc from
// the owner's draft feedback on the turn that loaded the skill ("up"|"down"),
// empty until then. Maps to the skill_usage table (migration 124).
type SkillUsageRecord struct {
	ID              int64
	TenantID        string
	SkillID         int64
	ChatID          string
	SourceMessageID string
	Rating          string // "" | "up" | "down"
	CreatedAt       time.Time
}

// SkillSignalSummary aggregates a skill's usage into the counts an improvement
// pass reasons over: how often it was loaded and how the owner rated the replies.
type SkillSignalSummary struct {
	Loads    int
	Positive int
	Negative int
}

// RecordSkillLoad appends one usage row. The load_skill Python tool records its
// own hits directly; this Go path backs the in-memory store and tests, and is
// available if load recording ever moves server-side.
func (r *Repository) RecordSkillLoad(tenantID string, skillID int64, chatID, sourceMessageID string) {
	if r.db != nil {
		if _, err := r.db.Exec(
			`INSERT INTO skill_usage (tenant_id, skill_id, chat_id, source_message_id)
			 VALUES ($1, $2, $3, $4)`,
			tenantID, skillID, chatID, sourceMessageID,
		); err != nil {
			r.logWriteError("record_skill_load", tenantID, chatID, err)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skillUsage = append(r.skillUsage, SkillUsageRecord{
		ID: int64(len(r.skillUsage) + 1), TenantID: tenantID, SkillID: skillID,
		ChatID: chatID, SourceMessageID: sourceMessageID, CreatedAt: time.Now(),
	})
}

// RecordSkillFeedbackForTurn stamps a rating onto every still-unrated skill_usage
// row for one turn (the message that produced the rated draft), attributing the
// owner's up/down to whichever skills the writer loaded that turn. Idempotent:
// only rows without a rating are updated. Returns how many rows were stamped.
func (r *Repository) RecordSkillFeedbackForTurn(tenantID, chatID, sourceMessageID, rating string) int {
	if rating != "up" && rating != "down" {
		return 0
	}
	if sourceMessageID == "" {
		return 0
	}
	if r.db != nil {
		res, err := r.db.Exec(
			`UPDATE skill_usage SET rating = $4
			  WHERE tenant_id = $1 AND chat_id = $2 AND source_message_id = $3 AND rating IS NULL`,
			tenantID, chatID, sourceMessageID, rating,
		)
		if err != nil {
			r.logWriteError("record_skill_feedback", tenantID, chatID, err)
			return 0
		}
		n, _ := res.RowsAffected()
		return int(n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stamped := 0
	for i := range r.skillUsage {
		u := &r.skillUsage[i]
		if u.TenantID == tenantID && u.ChatID == chatID &&
			u.SourceMessageID == sourceMessageID && u.Rating == "" {
			u.Rating = rating
			stamped++
		}
	}
	return stamped
}

// SkillSignal aggregates a skill's usage counts (loads + up/down tallies) for the
// improvement prompt and the autonomous-curator threshold.
func (r *Repository) SkillSignal(skillID int64) SkillSignalSummary {
	if r.db != nil {
		var s SkillSignalSummary
		_ = r.db.QueryRow(
			`SELECT COUNT(*),
			        COUNT(*) FILTER (WHERE rating = 'up'),
			        COUNT(*) FILTER (WHERE rating = 'down')
			   FROM skill_usage WHERE skill_id = $1`,
			skillID,
		).Scan(&s.Loads, &s.Positive, &s.Negative)
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var s SkillSignalSummary
	for _, u := range r.skillUsage {
		if u.SkillID != skillID {
			continue
		}
		s.Loads++
		switch u.Rating {
		case "up":
			s.Positive++
		case "down":
			s.Negative++
		}
	}
	return s
}
