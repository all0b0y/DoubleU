package store

import (
	"database/sql"
	"fmt"
	"time"

	"doubleu/backend-go/internal/contracts"
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
	Status         string // pending | accepted | rejected | expired | reverted
	CreatedAt      time.Time
	// AutoApplied is true for suggestions applied by the auto-learn path (no
	// owner review); such rows are revertible via RevertSuggestion.
	AutoApplied bool
	// PreviousValue is the profile field's value captured at auto-apply time,
	// so a revert can restore it (empty = the field was unset).
	PreviousValue string
}

// profileFieldValue reads one whitelisted profile field by name; unknown names
// yield "" (callers whitelist-check via suggestionProfileFields first).
func profileFieldValue(p contracts.ContactProfile, field string) string {
	switch field {
	case "relationship_type":
		return p.RelationshipType
	case "preferred_address":
		return p.PreferredAddress
	case "language":
		return p.Language
	case "tone":
		return p.Tone
	case "gender":
		return p.Gender
	}
	return ""
}

// setProfileFieldValue writes one whitelisted profile field by name.
func setProfileFieldValue(p *contracts.ContactProfile, field, value string) {
	switch field {
	case "relationship_type":
		p.RelationshipType = value
	case "preferred_address":
		p.PreferredAddress = value
	case "language":
		p.Language = value
	case "tone":
		p.Tone = value
	case "gender":
		p.Gender = value
	}
}

// applyProfileFieldTx ensures the profile row exists and sets one whitelisted
// column within tx. Callers MUST validate fieldName against
// suggestionProfileFields before calling — fieldName lands in an identifier
// position, so an unvalidated value would be a SQL-injection vector.
func applyProfileFieldTx(tx *sql.Tx, tenantID, chatID, fieldName, value string) error {
	if _, err := tx.Exec(
		`INSERT INTO contact_profiles (tenant_id, chat_id)
		 VALUES ($1, $2) ON CONFLICT (tenant_id, chat_id) DO NOTHING`,
		tenantID, chatID,
	); err != nil {
		return err
	}
	_, err := tx.Exec(
		fmt.Sprintf(`UPDATE contact_profiles SET %s = $3, updated_at = now()
		  WHERE tenant_id = $1 AND chat_id = $2`, fieldName),
		tenantID, chatID, value,
	)
	return err
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
			setProfileFieldValue(&profile, resolved.FieldName, resolved.SuggestedValue)
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
		if err := applyProfileFieldTx(tx, tenantID, chatID, fieldName, value); err != nil {
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

// RevertableSuggestions returns the most recent auto-applied (still-accepted)
// suggestion per field for a chat (or the whole tenant when chatID is empty),
// so the Mini App can offer an undo. Only the latest per field is returned —
// older rows carry a stale previous_value and must not be surfaced for revert.
func (r *Repository) RevertableSuggestions(tenantID, chatID string) []ProfileSuggestionRecord {
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		latest := make(map[string]ProfileSuggestionRecord)
		for _, s := range r.profileSuggestions {
			if s.TenantID == tenantID && s.Status == "accepted" && s.AutoApplied &&
				(chatID == "" || s.ChatID == chatID) {
				key := s.ChatID + "\x00" + s.FieldName
				if prev, ok := latest[key]; !ok || s.ID > prev.ID {
					latest[key] = s
				}
			}
		}
		result := make([]ProfileSuggestionRecord, 0, len(latest))
		for _, s := range latest {
			result = append(result, s)
		}
		return result
	}
	rows, err := r.db.Query(
		`SELECT DISTINCT ON (chat_id, field_name)
		        id, tenant_id, chat_id, field_name, suggested_value #>> '{}',
		        COALESCE(rationale,''), status, created_at
		   FROM contact_profile_suggestions
		  WHERE tenant_id = $1 AND ($2 = '' OR chat_id = $2)
		    AND status = 'accepted' AND auto_applied = true
		  ORDER BY chat_id, field_name, reviewed_at DESC`,
		tenantID, chatID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]ProfileSuggestionRecord, 0)
	for rows.Next() {
		var s ProfileSuggestionRecord
		s.AutoApplied = true
		if rows.Scan(&s.ID, &s.TenantID, &s.ChatID, &s.FieldName, &s.SuggestedValue,
			&s.Rationale, &s.Status, &s.CreatedAt) == nil {
			result = append(result, s)
		}
	}
	return result
}

// AutoApplyProfileSuggestions applies every pending suggestion for a chat
// straight to contact_profiles, marking each accepted+auto_applied and
// capturing the pre-change value for a later revert. This is the auto-learn
// path (no owner review); callers gate it on the role's AutoLearnProfile flag
// and on privacy. Idempotent: only pending→accepted transitions happen, so a
// re-run applies nothing. Returns the number of suggestions applied.
func (r *Repository) AutoApplyProfileSuggestions(tenantID, chatID string) (int, error) {
	if r.db == nil {
		return r.autoApplyProfileSuggestionsInMemory(tenantID, chatID), nil
	}
	// Collect pending rows first; database/sql forbids other statements on the
	// same tx while a result set is open.
	rows, err := r.db.Query(
		`SELECT id, field_name, suggested_value #>> '{}'
		   FROM contact_profile_suggestions
		  WHERE tenant_id = $1 AND chat_id = $2 AND status = 'pending'
		  ORDER BY created_at ASC`,
		tenantID, chatID,
	)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id    int64
		field string
		value string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.field, &p.value); err == nil {
			todo = append(todo, p)
		}
	}
	rows.Close()

	applied := 0
	for _, p := range todo {
		if !suggestionProfileFields[p.field] {
			continue // defence-in-depth; SaveProfileSuggestion already whitelists
		}
		tx, err := r.db.Begin()
		if err != nil {
			return applied, err
		}
		// Snapshot the current value (whitelisted field) for revert.
		var prev sql.NullString
		if err := tx.QueryRow(
			fmt.Sprintf(`SELECT %s FROM contact_profiles WHERE tenant_id = $1 AND chat_id = $2`, p.field),
			tenantID, chatID,
		).Scan(&prev); err != nil && err != sql.ErrNoRows {
			_ = tx.Rollback()
			return applied, err
		}
		res, err := tx.Exec(
			`UPDATE contact_profile_suggestions
			    SET status = 'accepted', auto_applied = true,
			        previous_value = to_jsonb($3::text), reviewed_at = now()
			  WHERE tenant_id = $1 AND id = $2 AND status = 'pending'`,
			tenantID, p.id, nullableString(prev.String),
		)
		if err != nil {
			_ = tx.Rollback()
			return applied, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Lost the race (already resolved) — skip without applying.
			_ = tx.Rollback()
			continue
		}
		if err := applyProfileFieldTx(tx, tenantID, chatID, p.field, p.value); err != nil {
			_ = tx.Rollback()
			return applied, err
		}
		if err := tx.Commit(); err != nil {
			return applied, err
		}
		applied++
	}
	if applied > 0 {
		r.invalidateContextScore(tenantID, chatID)
		r.mu.Lock()
		delete(r.contactProfiles, chatKey(tenantID, chatID))
		r.mu.Unlock()
	}
	return applied, nil
}

// autoApplyProfileSuggestionsInMemory is the r.db == nil path for tests.
func (r *Repository) autoApplyProfileSuggestionsInMemory(tenantID, chatID string) int {
	type pending struct {
		idx   int
		field string
		value string
	}
	r.mu.Lock()
	var todo []pending
	for i := range r.profileSuggestions {
		s := &r.profileSuggestions[i]
		if s.TenantID == tenantID && s.ChatID == chatID && s.Status == "pending" &&
			suggestionProfileFields[s.FieldName] {
			todo = append(todo, pending{idx: i, field: s.FieldName, value: s.SuggestedValue})
		}
	}
	r.mu.Unlock()

	applied := 0
	for _, p := range todo {
		profile, _ := r.ContactProfile(tenantID, chatID)
		profile.TenantID, profile.ChatID = tenantID, chatID
		prev := profileFieldValue(profile, p.field)
		setProfileFieldValue(&profile, p.field, p.value)
		r.SaveContactProfile(profile)
		r.mu.Lock()
		r.profileSuggestions[p.idx].Status = "accepted"
		r.profileSuggestions[p.idx].AutoApplied = true
		r.profileSuggestions[p.idx].PreviousValue = prev
		r.mu.Unlock()
		applied++
	}
	return applied
}

// RevertSuggestion undoes an auto-applied suggestion: it restores the profile
// field to the value captured at apply time and marks the row 'reverted'. Only
// accepted+auto_applied rows are revertible (owner-accepted rows are not).
func (r *Repository) RevertSuggestion(tenantID string, id int64) error {
	if r.db == nil {
		r.mu.Lock()
		var target *ProfileSuggestionRecord
		for i := range r.profileSuggestions {
			s := &r.profileSuggestions[i]
			if s.TenantID == tenantID && s.ID == id && s.Status == "accepted" && s.AutoApplied {
				s.Status = "reverted"
				target = s
				break
			}
		}
		r.mu.Unlock()
		if target == nil {
			return fmt.Errorf("auto-applied suggestion %d not found", id)
		}
		profile, _ := r.ContactProfile(tenantID, target.ChatID)
		profile.TenantID, profile.ChatID = tenantID, target.ChatID
		setProfileFieldValue(&profile, target.FieldName, target.PreviousValue)
		r.SaveContactProfile(profile)
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	var chatID, fieldName string
	var prev sql.NullString
	if err := tx.QueryRow(
		`UPDATE contact_profile_suggestions
		    SET status = 'reverted', reviewed_at = now()
		  WHERE tenant_id = $1 AND id = $2 AND status = 'accepted' AND auto_applied = true
		  RETURNING chat_id, field_name, previous_value #>> '{}'`,
		tenantID, id,
	).Scan(&chatID, &fieldName, &prev); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("auto-applied suggestion %d not found: %w", id, err)
	}
	if !suggestionProfileFields[fieldName] {
		_ = tx.Rollback()
		return fmt.Errorf("suggestion field %q is not allowed", fieldName)
	}
	if err := applyProfileFieldTx(tx, tenantID, chatID, fieldName, prev.String); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.invalidateContextScore(tenantID, chatID)
	r.mu.Lock()
	delete(r.contactProfiles, chatKey(tenantID, chatID))
	r.mu.Unlock()
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
