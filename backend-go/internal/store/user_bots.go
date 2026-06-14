// User-supplied bot registry and tenant ingestion mode (bot-transport ingestion).
//
// Security invariant: UserBotRecord.Token is a credential. It must never be
// written to logs or returned to clients unmasked — callers log bot_id /
// bot_username instead.
package store

import (
	"database/sql"

	"doubleu/backend-go/internal/contracts"
)

// UserBotRecord stores a tenant's user-supplied Telegram bot used for
// bot-transport ingestion (long polling getUpdates).
type UserBotRecord struct {
	TenantID    string
	BotID       int64
	BotUsername string
	Token       string // secret: never log, never expose unmasked
	Status      string // active | disabled | invalid
	// LastUpdateOffset is the highest processed update_id; the poller resumes
	// from LastUpdateOffset+1 so a restart never replays acknowledged updates.
	LastUpdateOffset int64
	// AIRoleID is the role that governs every bot-transport chat of this bot
	// (migration 102); 0 means the default assistant persona. Managed via
	// SetUserBotRole, not SaveUserBot, so re-register/offset writes preserve it.
	AIRoleID int64
}

// SaveUserBot registers or replaces the tenant's bot (one bot per tenant).
func (r *Repository) SaveUserBot(record UserBotRecord) {
	if record.Status == "" {
		record.Status = "active"
	}
	r.mu.Lock()
	// Preserve the bot's role across re-registration: the DB upsert below leaves
	// ai_role_id untouched, so the in-memory path must match (the RegisterBot
	// caller never carries the role).
	if existing, ok := r.userBots[record.TenantID]; ok && record.AIRoleID == 0 {
		record.AIRoleID = existing.AIRoleID
	}
	r.userBots[record.TenantID] = record
	r.mu.Unlock()
	if r.db != nil {
		r.ensureTenant(record.TenantID, "")
		_, err := r.db.Exec(
			`INSERT INTO user_bots (tenant_id, bot_id, bot_username, token, status, last_update_offset)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id) DO UPDATE SET
			   bot_id             = EXCLUDED.bot_id,
			   bot_username       = EXCLUDED.bot_username,
			   token              = EXCLUDED.token,
			   status             = EXCLUDED.status,
			   last_update_offset = EXCLUDED.last_update_offset,
			   updated_at         = now()`,
			record.TenantID,
			record.BotID,
			record.BotUsername,
			record.Token,
			record.Status,
			record.LastUpdateOffset,
		)
		r.logWriteError("save_user_bot", record.TenantID, "", err)
	}
}

// SetUserBotRole binds a role to the tenant's bot (roleID 0 clears it). Updates
// only ai_role_id so it never clobbers token/offset/status; kept separate from
// SaveUserBot so re-registering the bot preserves the chosen role.
func (r *Repository) SetUserBotRole(tenantID string, roleID int64) {
	r.mu.Lock()
	if rec, ok := r.userBots[tenantID]; ok {
		rec.AIRoleID = roleID
		r.userBots[tenantID] = rec
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`UPDATE user_bots SET ai_role_id = NULLIF($2, 0), updated_at = now() WHERE tenant_id = $1`,
			tenantID, roleID)
		r.logWriteError("set_user_bot_role", tenantID, "", err)
	}
}

// UserBotByTenant returns the tenant's registered bot.
func (r *Repository) UserBotByTenant(tenantID string) (UserBotRecord, bool) {
	if r.db != nil {
		row := r.db.QueryRow(
			`SELECT tenant_id, bot_id, bot_username, token, status, last_update_offset, COALESCE(ai_role_id, 0)
			   FROM user_bots WHERE tenant_id = $1`, tenantID)
		var rec UserBotRecord
		var roleID sql.NullInt64
		if err := row.Scan(&rec.TenantID, &rec.BotID, &rec.BotUsername, &rec.Token, &rec.Status, &rec.LastUpdateOffset, &roleID); err == nil {
			rec.AIRoleID = roleID.Int64
			return rec, true
		}
		return UserBotRecord{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.userBots[tenantID]
	return rec, ok
}

// ActiveUserBots returns every bot with status "active" — the set the polling
// manager must run on startup.
func (r *Repository) ActiveUserBots() []UserBotRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT tenant_id, bot_id, bot_username, token, status, last_update_offset
			   FROM user_bots WHERE status = 'active'`)
		if err != nil {
			r.logWriteError("list_user_bots", "", "", err)
			return nil
		}
		defer rows.Close()
		var out []UserBotRecord
		for rows.Next() {
			var rec UserBotRecord
			if err := rows.Scan(&rec.TenantID, &rec.BotID, &rec.BotUsername, &rec.Token, &rec.Status, &rec.LastUpdateOffset); err == nil {
				out = append(out, rec)
			}
		}
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UserBotRecord, 0, len(r.userBots))
	for _, rec := range r.userBots {
		if rec.Status == "active" {
			out = append(out, rec)
		}
	}
	return out
}

// UpdateUserBotOffset persists the poller's getUpdates progress for one bot.
func (r *Repository) UpdateUserBotOffset(botID int64, offset int64) {
	r.mu.Lock()
	for tenantID, rec := range r.userBots {
		if rec.BotID == botID {
			rec.LastUpdateOffset = offset
			r.userBots[tenantID] = rec
			break
		}
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`UPDATE user_bots SET last_update_offset = $2, updated_at = now() WHERE bot_id = $1`,
			botID, offset)
		r.logWriteError("update_user_bot_offset", "", "", err)
	}
}

// UpdateUserBotStatus transitions a bot between active / disabled / invalid
// (e.g. the poller marks a bot "invalid" after Telegram rejects its token).
func (r *Repository) UpdateUserBotStatus(botID int64, status string) {
	r.mu.Lock()
	for tenantID, rec := range r.userBots {
		if rec.BotID == botID {
			rec.Status = status
			r.userBots[tenantID] = rec
			break
		}
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`UPDATE user_bots SET status = $2, updated_at = now() WHERE bot_id = $1`,
			botID, status)
		r.logWriteError("update_user_bot_status", "", "", err)
	}
}

// DeleteUserBot removes the tenant's bot registration.
func (r *Repository) DeleteUserBot(tenantID string) {
	r.mu.Lock()
	delete(r.userBots, tenantID)
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(`DELETE FROM user_bots WHERE tenant_id = $1`, tenantID)
		r.logWriteError("delete_user_bot", tenantID, "", err)
	}
}

// TenantIngestionMode returns the tenant's ingestion channel selection.
// Empty result means "not set" — the caller applies the configured default.
func (r *Repository) TenantIngestionMode(tenantID string) contracts.IngestionMode {
	if r.db != nil {
		var raw string
		err := r.db.QueryRow(`SELECT ingestion_mode FROM tenants WHERE id = $1`, tenantID).Scan(&raw)
		if err != nil {
			return ""
		}
		mode, _ := contracts.ParseIngestionMode(raw)
		return mode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tenantModes[tenantID]
}

// SetTenantIngestionMode persists the tenant's ingestion channel selection.
func (r *Repository) SetTenantIngestionMode(tenantID string, mode contracts.IngestionMode) {
	r.mu.Lock()
	r.tenantModes[tenantID] = mode
	r.mu.Unlock()
	if r.db != nil {
		r.ensureTenant(tenantID, "")
		_, err := r.db.Exec(
			`UPDATE tenants SET ingestion_mode = $2, updated_at = now() WHERE id = $1`,
			tenantID, string(mode))
		r.logWriteError("set_ingestion_mode", tenantID, "", err)
	}
}

// ChatTransport returns the channel the chat's messages arrive through
// ("business" or "bot"); empty when the chat is unknown.
func (r *Repository) ChatTransport(tenantID, chatID string) string {
	if r.db != nil {
		var transport string
		err := r.db.QueryRow(
			`SELECT transport FROM chats WHERE tenant_id = $1 AND chat_id = $2`,
			tenantID, chatID).Scan(&transport)
		if err != nil {
			return ""
		}
		return transport
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chats[chatKey(tenantID, chatID)].Transport
}
