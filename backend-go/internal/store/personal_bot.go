// Personal-bot mode storage (migration 122): the per-bot toggle, the allowlist
// of Telegram users the bot also serves, and the once-per-contact stub marker.
package store

import "sort"

// SetUserBotPersonalMode toggles personal mode for one bot. Kept separate from
// SaveUserBot (like SetUserBotRole) so re-registering the bot preserves it.
func (r *Repository) SetUserBotPersonalMode(tenantID string, botID int64, enabled bool) {
	key := botStoreKey(tenantID, botID)
	r.mu.Lock()
	if rec, ok := r.userBots[key]; ok {
		rec.PersonalMode = enabled
		r.userBots[key] = rec
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`UPDATE user_bots SET personal_mode = $3, updated_at = now() WHERE tenant_id = $1 AND bot_id = $2`,
			tenantID, botID, enabled)
		r.logWriteError("set_user_bot_personal_mode", tenantID, "", err)
	}
}

// AddBotAllowedUser adds a Telegram user id to a personal bot's allowlist
// (idempotent). The owner is always served and is not stored here.
func (r *Repository) AddBotAllowedUser(tenantID string, botID int64, telegramUserID string) {
	if telegramUserID == "" {
		return
	}
	key := botStoreKey(tenantID, botID)
	r.mu.Lock()
	set, ok := r.botAllowedUsers[key]
	if !ok {
		set = make(map[string]struct{})
		r.botAllowedUsers[key] = set
	}
	set[telegramUserID] = struct{}{}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`INSERT INTO personal_bot_allowed_users (bot_id, tenant_id, telegram_user_id)
			 VALUES ($1, $2, $3) ON CONFLICT (bot_id, telegram_user_id) DO NOTHING`,
			botID, tenantID, telegramUserID)
		r.logWriteError("add_bot_allowed_user", tenantID, "", err)
	}
}

// RemoveBotAllowedUser drops a Telegram user id from a personal bot's allowlist.
func (r *Repository) RemoveBotAllowedUser(tenantID string, botID int64, telegramUserID string) {
	key := botStoreKey(tenantID, botID)
	r.mu.Lock()
	if set, ok := r.botAllowedUsers[key]; ok {
		delete(set, telegramUserID)
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`DELETE FROM personal_bot_allowed_users WHERE bot_id = $1 AND telegram_user_id = $2`,
			botID, telegramUserID)
		r.logWriteError("remove_bot_allowed_user", tenantID, "", err)
	}
}

// BotAllowedUsers returns the personal bot's allowlist (Telegram user ids),
// sorted for a stable UI listing.
func (r *Repository) BotAllowedUsers(tenantID string, botID int64) []string {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT telegram_user_id FROM personal_bot_allowed_users
			  WHERE bot_id = $1 ORDER BY created_at`, botID)
		if err != nil {
			r.logWriteError("list_bot_allowed_users", tenantID, "", err)
			return nil
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				out = append(out, id)
			}
		}
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.botAllowedUsers[botStoreKey(tenantID, botID)]
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsBotAllowedUser reports whether a Telegram user is on the personal bot's
// allowlist. The owner check is the caller's responsibility (owner is not stored).
func (r *Repository) IsBotAllowedUser(tenantID string, botID int64, telegramUserID string) bool {
	if telegramUserID == "" {
		return false
	}
	if r.db != nil {
		var exists bool
		err := r.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM personal_bot_allowed_users
			                WHERE bot_id = $1 AND telegram_user_id = $2)`,
			botID, telegramUserID).Scan(&exists)
		if err != nil {
			r.logWriteError("is_bot_allowed_user", tenantID, "", err)
			return false
		}
		return exists
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.botAllowedUsers[botStoreKey(tenantID, botID)]
	if !ok {
		return false
	}
	_, allowed := set[telegramUserID]
	return allowed
}

// MarkPersonalBotStubShown records that a contact has been shown the personal-bot
// stub and returns whether it had already been shown. This is the once-per-contact
// gate: the first call returns false (send the stub), later calls return true
// (stay silent). chatID is the bot-namespaced chat id.
func (r *Repository) MarkPersonalBotStubShown(tenantID, chatID string) bool {
	if r.db != nil {
		res, err := r.db.Exec(
			`INSERT INTO personal_bot_stub_shown (tenant_id, chat_id)
			 VALUES ($1, $2) ON CONFLICT (tenant_id, chat_id) DO NOTHING`,
			tenantID, chatID)
		if err != nil {
			r.logWriteError("mark_personal_bot_stub_shown", tenantID, "", err)
			// On a write error, report "already shown" so we fail closed and do not
			// risk a stub-send loop against a possibly-flooding stranger.
			return true
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return true
		}
		return affected == 0
	}
	key := tenantID + "#" + chatID
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.personalStubShown[key]; ok {
		return true
	}
	r.personalStubShown[key] = struct{}{}
	return false
}
