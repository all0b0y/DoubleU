package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ============================================================
// Contact profile suggestions (§4.5, MEM-4)
// ============================================================

// ProfileSuggestionRecord maps to contact_profile_suggestions: an AI-proposed
// profile field update awaiting owner review. The AI never writes the profile
// directly — accepted suggestions are applied by ResolveSuggestion (§15).
type ProfileSuggestionRecord struct {
	ID             int64
	TenantID       string
	ChatID         string
	FieldName      string
	SuggestedValue string
	Rationale      string
	Status         string // pending | accepted | rejected | expired
	CreatedAt      time.Time
}

// suggestionProfileFields whitelists the contact_profiles columns a suggestion
// may target; ResolveSuggestion refuses anything else (defence against a
// poisoned field_name reaching the SQL identifier position).
var suggestionProfileFields = map[string]bool{
	"relationship_type": true,
	"preferred_address": true,
	"language":          true,
	"tone":              true,
	"gender":            true,
}

// SaveProfileSuggestion stages a suggestion for owner review. Returns the new
// row ID, or 0 when an identical pending suggestion for the field exists
// (unique partial index from migration 016).
func (r *Repository) SaveProfileSuggestion(rec ProfileSuggestionRecord) int64 {
	if !suggestionProfileFields[rec.FieldName] {
		return 0
	}
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, existing := range r.profileSuggestions {
			if existing.TenantID == rec.TenantID && existing.ChatID == rec.ChatID &&
				existing.FieldName == rec.FieldName && existing.Status == "pending" {
				return 0
			}
		}
		r.aiRoleSeq++
		rec.ID = r.aiRoleSeq
		rec.Status = "pending"
		rec.CreatedAt = time.Now()
		r.profileSuggestions = append(r.profileSuggestions, rec)
		return rec.ID
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO contact_profile_suggestions (tenant_id, chat_id, field_name, suggested_value, rationale)
		 VALUES ($1, $2, $3, to_jsonb($4::text), NULLIF($5,''))
		 ON CONFLICT (tenant_id, chat_id, field_name) WHERE status = 'pending'
		 DO NOTHING
		 RETURNING id`,
		rec.TenantID, rec.ChatID, rec.FieldName, rec.SuggestedValue, rec.Rationale,
	).Scan(&id)
	if err != nil {
		if err != sql.ErrNoRows {
			r.logWriteError("save_profile_suggestion", rec.TenantID, rec.ChatID, err)
		}
		return 0
	}
	return id
}

// PendingSuggestions returns pending suggestions for a chat, or for the whole
// tenant when chatID is empty (the /suggestions command view).
func (r *Repository) PendingSuggestions(tenantID, chatID string) []ProfileSuggestionRecord {
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		result := make([]ProfileSuggestionRecord, 0)
		for _, s := range r.profileSuggestions {
			if s.TenantID == tenantID && s.Status == "pending" && (chatID == "" || s.ChatID == chatID) {
				result = append(result, s)
			}
		}
		return result
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, chat_id, field_name, suggested_value #>> '{}', COALESCE(rationale,''), status, created_at
		   FROM contact_profile_suggestions
		  WHERE tenant_id = $1 AND status = 'pending' AND ($2 = '' OR chat_id = $2)
		  ORDER BY created_at ASC`,
		tenantID, chatID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]ProfileSuggestionRecord, 0)
	for rows.Next() {
		var s ProfileSuggestionRecord
		if rows.Scan(&s.ID, &s.TenantID, &s.ChatID, &s.FieldName, &s.SuggestedValue, &s.Rationale, &s.Status, &s.CreatedAt) == nil {
			result = append(result, s)
		}
	}
	return result
}

// ResolveSuggestion marks a pending suggestion accepted or rejected. Accepting
// applies the value to contact_profiles in the same transaction — the only
// path by which AI-derived data reaches the profile (§4.5/§15).
func (r *Repository) ResolveSuggestion(tenantID string, id int64, accept bool) error {
	status := "rejected"
	if accept {
		status = "accepted"
	}
	if r.db == nil {
		r.mu.Lock()
		var resolved *ProfileSuggestionRecord
		for i := range r.profileSuggestions {
			s := &r.profileSuggestions[i]
			if s.TenantID == tenantID && s.ID == id && s.Status == "pending" {
				s.Status = status
				resolved = s
				break
			}
		}
		r.mu.Unlock()
		if resolved == nil {
			return fmt.Errorf("pending suggestion %d not found", id)
		}
		if accept {
			profile, _ := r.ContactProfile(tenantID, resolved.ChatID)
			profile.TenantID, profile.ChatID = tenantID, resolved.ChatID
			switch resolved.FieldName {
			case "relationship_type":
				profile.RelationshipType = resolved.SuggestedValue
			case "preferred_address":
				profile.PreferredAddress = resolved.SuggestedValue
			case "language":
				profile.Language = resolved.SuggestedValue
			case "tone":
				profile.Tone = resolved.SuggestedValue
			case "gender":
				profile.Gender = resolved.SuggestedValue
			}
			r.SaveContactProfile(profile)
		}
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	var chatID, fieldName, value string
	if err := tx.QueryRow(
		`UPDATE contact_profile_suggestions
		    SET status = $3, reviewed_at = now()
		  WHERE tenant_id = $1 AND id = $2 AND status = 'pending'
		  RETURNING chat_id, field_name, suggested_value #>> '{}'`,
		tenantID, id, status,
	).Scan(&chatID, &fieldName, &value); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("pending suggestion %d not found: %w", id, err)
	}
	if accept {
		if !suggestionProfileFields[fieldName] {
			_ = tx.Rollback()
			return fmt.Errorf("suggestion field %q is not allowed", fieldName)
		}
		// Ensure the profile row exists, then set the single whitelisted column.
		if _, err := tx.Exec(
			`INSERT INTO contact_profiles (tenant_id, chat_id)
			 VALUES ($1, $2) ON CONFLICT (tenant_id, chat_id) DO NOTHING`,
			tenantID, chatID,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		// fieldName is whitelist-validated above — safe in identifier position.
		if _, err := tx.Exec(
			fmt.Sprintf(`UPDATE contact_profiles SET %s = $3, updated_at = now()
			  WHERE tenant_id = $1 AND chat_id = $2`, fieldName),
			tenantID, chatID, value,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if accept {
		r.invalidateContextScore(tenantID, chatID)
		// Refresh the in-memory profile mirror from the DB on next read.
		r.mu.Lock()
		delete(r.contactProfiles, chatKey(tenantID, chatID))
		r.mu.Unlock()
	}
	return nil
}

// ExpireOldSuggestions marks pending suggestions older than maxAge as expired
// (nightly hygiene, §4.5). Returns the number of expired rows.
func (r *Repository) ExpireOldSuggestions(tenantID string, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		expired := 0
		for i := range r.profileSuggestions {
			s := &r.profileSuggestions[i]
			if s.TenantID == tenantID && s.Status == "pending" && s.CreatedAt.Before(cutoff) {
				s.Status = "expired"
				expired++
			}
		}
		return expired
	}
	res, err := r.db.Exec(
		`UPDATE contact_profile_suggestions
		    SET status = 'expired', reviewed_at = now()
		  WHERE tenant_id = $1 AND status = 'pending' AND created_at < $2`,
		tenantID, cutoff,
	)
	if err != nil {
		r.logWriteError("expire_suggestions", tenantID, "", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}
