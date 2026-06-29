package store

import (
	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// ChatSettings returns settings for a tenant/chat pair.
func (r *Repository) ChatSettings(tenantID, chatID string) contracts.ChatSettings {
	if r.db != nil {
		var mode string
		var rawFlags pq.StringArray
		err := r.db.QueryRow(
			`SELECT mode::text, privacy_flags::text[] FROM chat_settings WHERE tenant_id = $1 AND chat_id = $2`,
			tenantID,
			chatID,
		).Scan(&mode, &rawFlags)
		if err == nil {
			return contracts.ChatSettings{
				TenantID:     tenantID,
				ChatID:       chatID,
				Mode:         contracts.ChatMode(mode),
				PrivacyFlags: privacyFlagsFromStrings([]string(rawFlags)),
			}.WithDefaults()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	settings, ok := r.chatSettings[chatKey(tenantID, chatID)]
	if !ok {
		return contracts.ChatSettings{
			TenantID:     tenantID,
			ChatID:       chatID,
			Mode:         contracts.ChatModeManual,
			PrivacyFlags: contracts.PrivacyFlags{},
		}
	}
	return settings.WithDefaults()
}

// SaveChatSettings stores settings for a tenant/chat pair.
func (r *Repository) SaveChatSettings(settings contracts.ChatSettings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	settings = settings.WithDefaults()
	r.chatSettings[chatKey(settings.TenantID, settings.ChatID)] = settings
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO chat_settings (tenant_id, chat_id, mode, privacy_flags)
			 VALUES ($1, $2, $3::chat_mode, $4::privacy_flag[])
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   mode = EXCLUDED.mode,
			   privacy_flags = EXCLUDED.privacy_flags,
			   updated_at = now()`,
			settings.TenantID,
			settings.ChatID,
			string(settings.Mode),
			pq.Array(stringsFromPrivacyFlags(settings.PrivacyFlags)),
		)
	}
}

func stringsFromPrivacyFlags(flags contracts.PrivacyFlags) []string {
	values := make([]string, 0, len(flags))
	for _, flag := range flags {
		values = append(values, string(flag))
	}
	return values
}

func privacyFlagsFromStrings(values []string) contracts.PrivacyFlags {
	flags := make(contracts.PrivacyFlags, 0, len(values))
	for _, value := range values {
		flags = append(flags, contracts.PrivacyFlag(value))
	}
	return flags
}
