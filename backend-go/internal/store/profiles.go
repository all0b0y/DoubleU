package store

import (
	"database/sql"
	"encoding/json"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// ============================================================
// Contact Profile (Per-Chat Identity Layer)
// ============================================================

// SaveContactProfile stores the owner-supplied contact profile for a chat.
// It maps both the legacy fields (Name, Notes) and the new architecture fields
// (DisplayNames, StyleNotes, ChatContext, Tone, EmojiUsage, AddressForm, …) to
// the contact_profiles table.
func (r *Repository) SaveContactProfile(profile contracts.ContactProfile) {
	r.invalidateContextScore(profile.TenantID, profile.ChatID)
	// Synchronise Name ↔ DisplayNames[0] so both representations stay consistent.
	if profile.Name != "" && len(profile.DisplayNames) == 0 {
		profile.DisplayNames = []string{profile.Name}
	} else if len(profile.DisplayNames) > 0 && profile.Name == "" {
		profile.Name = profile.DisplayNames[0]
	}

	// Parse the legacy notes JSON field into the new dedicated columns.
	if profile.Notes != "" {
		var notes struct {
			MeStyle string `json:"me_style"`
			ChatCtx string `json:"chat_context"`
		}
		if err := json.Unmarshal([]byte(profile.Notes), &notes); err == nil {
			if notes.MeStyle != "" && profile.StyleNotes == "" {
				profile.StyleNotes = notes.MeStyle
			}
			if notes.ChatCtx != "" && profile.ChatContext == "" {
				profile.ChatContext = notes.ChatCtx
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.contactProfiles[chatKey(profile.TenantID, profile.ChatID)] = profile
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO contact_profiles (
			   tenant_id, chat_id, name, gender, role, relationship_type,
			   display_names, preferred_address, language, contact_timezone,
			   tone, emoji_usage, address_form, style_notes,
			   topics, chat_context, do_not_discuss,
			   notes, updated_at
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,now())
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   name              = EXCLUDED.name,
			   gender            = EXCLUDED.gender,
			   role              = EXCLUDED.role,
			   relationship_type = EXCLUDED.relationship_type,
			   display_names     = EXCLUDED.display_names,
			   preferred_address = EXCLUDED.preferred_address,
			   language          = EXCLUDED.language,
			   contact_timezone  = EXCLUDED.contact_timezone,
			   tone              = EXCLUDED.tone,
			   emoji_usage       = EXCLUDED.emoji_usage,
			   address_form      = EXCLUDED.address_form,
			   style_notes       = EXCLUDED.style_notes,
			   topics            = EXCLUDED.topics,
			   chat_context      = EXCLUDED.chat_context,
			   do_not_discuss    = EXCLUDED.do_not_discuss,
			   notes             = EXCLUDED.notes,
			   updated_at        = now()`,
			profile.TenantID, profile.ChatID,
			profile.Name, profile.Gender, profile.Role, profile.RelationshipType,
			pq.Array(nilToEmpty(profile.DisplayNames)),
			nullableString(profile.PreferredAddress),
			nullableString(profile.Language),
			nullableString(profile.ContactTimezone),
			nullableString(profile.Tone),
			nullableString(profile.EmojiUsage),
			nullableString(profile.AddressForm),
			nullableString(profile.StyleNotes),
			pq.Array(nilToEmpty(profile.Topics)),
			nullableString(profile.ChatContext),
			pq.Array(nilToEmpty(profile.DoNotDiscuss)),
			buildLegacyNotes(profile),
		)
	}
}

// buildLegacyNotes serialises the new style fields back to the legacy JSON notes format
// so the existing Mini App UI (which reads notes.me_style and notes.chat_context) continues
// to work without changes.
func buildLegacyNotes(p contracts.ContactProfile) string {
	type notesPayload struct {
		MeStyle string `json:"me_style,omitempty"`
		ChatCtx string `json:"chat_context,omitempty"`
	}
	payload := notesPayload{MeStyle: p.StyleNotes, ChatCtx: p.ChatContext}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ContactProfile returns the contact profile for a chat.
// It populates both legacy fields and new architecture fields from the DB row.
func (r *Repository) ContactProfile(tenantID, chatID string) (contracts.ContactProfile, bool) {
	if r.db != nil {
		var p contracts.ContactProfile
		p.TenantID = tenantID
		p.ChatID = chatID
		var displayNames pq.StringArray
		var topics pq.StringArray
		var doNotDiscuss pq.StringArray
		var (
			preferredAddress sql.NullString
			language         sql.NullString
			contactTimezone  sql.NullString
			tone             sql.NullString
			emojiUsage       sql.NullString
			addressForm      sql.NullString
			styleNotes       sql.NullString
			chatContext      sql.NullString
			notes            sql.NullString
		)
		err := r.db.QueryRow(
			`SELECT COALESCE(name,''), COALESCE(gender,''), COALESCE(role,''),
			        COALESCE(relationship_type,''),
			        COALESCE(display_names, ARRAY[]::text[]),
			        preferred_address, language, contact_timezone,
			        tone, emoji_usage, address_form, style_notes,
			        COALESCE(topics, ARRAY[]::text[]),
			        chat_context,
			        COALESCE(do_not_discuss, ARRAY[]::text[]),
			        notes
			   FROM contact_profiles WHERE tenant_id=$1 AND chat_id=$2`,
			tenantID, chatID,
		).Scan(
			&p.Name, &p.Gender, &p.Role, &p.RelationshipType,
			&displayNames,
			&preferredAddress, &language, &contactTimezone,
			&tone, &emojiUsage, &addressForm, &styleNotes,
			&topics,
			&chatContext,
			&doNotDiscuss,
			&notes,
		)
		if err == nil {
			p.DisplayNames = []string(displayNames)
			p.Topics = []string(topics)
			p.DoNotDiscuss = []string(doNotDiscuss)
			p.PreferredAddress = preferredAddress.String
			p.Language = language.String
			p.ContactTimezone = contactTimezone.String
			p.Tone = tone.String
			p.EmojiUsage = emojiUsage.String
			p.AddressForm = addressForm.String
			p.StyleNotes = styleNotes.String
			p.ChatContext = chatContext.String
			p.Notes = notes.String
			// Ensure Name and DisplayNames are consistent.
			if p.Name == "" && len(p.DisplayNames) > 0 {
				p.Name = p.DisplayNames[0]
			}
			// Populate legacy notes from new columns if notes is empty.
			if p.Notes == "" {
				p.Notes = buildLegacyNotes(p)
			}
			return p, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.contactProfiles[chatKey(tenantID, chatID)]
	return p, ok
}
