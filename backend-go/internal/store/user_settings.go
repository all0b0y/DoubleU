package store

import (
	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// UserSettings returns the owner settings for a tenant, if they have been configured.
func (r *Repository) UserSettings(tenantID string) (contracts.UserSettings, bool) {
	if r.db != nil {
		var s contracts.UserSettings
		var secLangs pq.StringArray
		err := r.db.QueryRow(
			`SELECT us.user_id, us.tenant_id,
			        COALESCE(us.display_name,''), COALESCE(us.bio,''), COALESCE(us.timezone,''),
			        us.primary_language, us.secondary_languages,
			        us.default_mode_for_new_chats, us.learning_enabled, us.rich_formatting_enabled,
			        us.streaming_enabled,
			        us.language_for_drafts
			   FROM user_settings us
			  WHERE us.tenant_id = $1
			  LIMIT 1`,
			tenantID,
		).Scan(
			&s.UserID, &s.TenantID,
			&s.DisplayName, &s.Bio, &s.Timezone,
			&s.PrimaryLanguage, &secLangs,
			&s.DefaultModeForNewChats, &s.LearningEnabled, &s.RichFormattingEnabled,
			&s.StreamingEnabled,
			&s.LanguageForDrafts,
		)
		if err == nil {
			s.SecondaryLanguages = []string(secLangs)
			return s, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.userSettings[tenantID]
	return s, ok
}

// SaveUserSettings stores owner-level identity and behaviour settings.
func (r *Repository) SaveUserSettings(settings contracts.UserSettings) {
	r.mu.Lock()
	r.userSettings[settings.TenantID] = settings
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`INSERT INTO user_settings (
		   user_id, tenant_id, display_name, bio, timezone,
		   primary_language, secondary_languages,
		   default_mode_for_new_chats, learning_enabled, rich_formatting_enabled,
		   streaming_enabled,
		   language_for_drafts
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (user_id) DO UPDATE SET
		   display_name            = EXCLUDED.display_name,
		   bio                     = EXCLUDED.bio,
		   timezone                = EXCLUDED.timezone,
		   primary_language        = EXCLUDED.primary_language,
		   secondary_languages     = EXCLUDED.secondary_languages,
		   default_mode_for_new_chats = EXCLUDED.default_mode_for_new_chats,
		   learning_enabled        = EXCLUDED.learning_enabled,
		   rich_formatting_enabled = EXCLUDED.rich_formatting_enabled,
		   streaming_enabled       = EXCLUDED.streaming_enabled,
		   language_for_drafts     = EXCLUDED.language_for_drafts,
		   updated_at              = now()`,
		settings.UserID, settings.TenantID,
		settings.DisplayName, settings.Bio, settings.Timezone,
		settings.PrimaryLanguage, pq.Array(nilToEmpty(settings.SecondaryLanguages)),
		settings.DefaultModeForNewChats, settings.LearningEnabled, settings.RichFormattingEnabled,
		settings.StreamingEnabled,
		settings.LanguageForDrafts,
	)
}
