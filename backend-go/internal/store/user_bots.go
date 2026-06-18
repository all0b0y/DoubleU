// User-supplied bot registry and tenant ingestion mode (bot-transport ingestion).
//
// Security invariant: UserBotRecord.Token is a credential. It must never be
// written to logs or returned to clients unmasked — callers log bot_id /
// bot_username instead.
package store

import (
	"database/sql"
	"sort"
	"strconv"

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
	// Nil delivery toggles inherit role/owner defaults; non-nil values override them.
	RichFormattingEnabled *bool
	StreamingEnabled      *bool
}

// botStoreKey is the in-memory map key for a single bot registration. A tenant
// may now own several bots, so the key combines tenant and bot id; "#" never
// occurs in a tenant id ("tenant:user:…") or a numeric bot id.
func botStoreKey(tenantID string, botID int64) string {
	return tenantID + "#" + strconv.FormatInt(botID, 10)
}

// SaveUserBot registers or updates one of the tenant's bots. A tenant may own
// several bots; the row is identified by the globally-unique bot_id (DB upsert
// on bot_id), so saving never disturbs the tenant's other bots.
func (r *Repository) SaveUserBot(record UserBotRecord) {
	if record.Status == "" {
		record.Status = "active"
	}
	key := botStoreKey(record.TenantID, record.BotID)
	r.mu.Lock()
	// Preserve this bot's role across re-registration: the DB upsert below leaves
	// ai_role_id untouched, so the in-memory path must match (the RegisterBot
	// caller never carries the role).
	if existing, ok := r.userBots[key]; ok {
		if record.AIRoleID == 0 {
			record.AIRoleID = existing.AIRoleID
		}
		if record.RichFormattingEnabled == nil {
			record.RichFormattingEnabled = cloneBoolPtr(existing.RichFormattingEnabled)
		}
		if record.StreamingEnabled == nil {
			record.StreamingEnabled = cloneBoolPtr(existing.StreamingEnabled)
		}
	}
	r.userBots[key] = record
	r.mu.Unlock()
	if r.db != nil {
		r.ensureTenant(record.TenantID, "")
		_, err := r.db.Exec(
			`INSERT INTO user_bots (tenant_id, bot_id, bot_username, token, status, last_update_offset)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (bot_id) DO UPDATE SET
			   tenant_id          = EXCLUDED.tenant_id,
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

// SetUserBotRole binds a role to one specific bot (roleID 0 clears it). Updates
// only ai_role_id so it never clobbers token/offset/status; kept separate from
// SaveUserBot so re-registering the bot preserves the chosen role.
func (r *Repository) SetUserBotRole(tenantID string, botID, roleID int64) {
	key := botStoreKey(tenantID, botID)
	r.mu.Lock()
	if rec, ok := r.userBots[key]; ok {
		rec.AIRoleID = roleID
		r.userBots[key] = rec
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`UPDATE user_bots SET ai_role_id = NULLIF($3, 0), updated_at = now() WHERE tenant_id = $1 AND bot_id = $2`,
			tenantID, botID, roleID)
		r.logWriteError("set_user_bot_role", tenantID, "", err)
	}
}

// SetUserBotDeliverySettings stores nullable per-bot delivery overrides. Nil
// means the bot inherits the governing role/owner setting.
func (r *Repository) SetUserBotDeliverySettings(tenantID string, botID int64, richFormatting, streaming *bool) {
	key := botStoreKey(tenantID, botID)
	r.mu.Lock()
	if rec, ok := r.userBots[key]; ok {
		rec.RichFormattingEnabled = cloneBoolPtr(richFormatting)
		rec.StreamingEnabled = cloneBoolPtr(streaming)
		r.userBots[key] = rec
	}
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(
			`UPDATE user_bots
			    SET rich_formatting_enabled = $3,
			        streaming_enabled = $4,
			        updated_at = now()
			  WHERE tenant_id = $1 AND bot_id = $2`,
			tenantID, botID, nullableBoolArg(richFormatting), nullableBoolArg(streaming),
		)
		r.logWriteError("set_user_bot_delivery_settings", tenantID, "", err)
	}
}

// scanUserBot reads one user_bots row (with role) from a row scanner.
func scanUserBot(sc rowScanner) (UserBotRecord, bool) {
	var rec UserBotRecord
	var roleID sql.NullInt64
	var richFormatting sql.NullBool
	var streaming sql.NullBool
	if err := sc.Scan(
		&rec.TenantID, &rec.BotID, &rec.BotUsername, &rec.Token, &rec.Status, &rec.LastUpdateOffset,
		&roleID, &richFormatting, &streaming,
	); err != nil {
		return UserBotRecord{}, false
	}
	rec.AIRoleID = roleID.Int64
	rec.RichFormattingEnabled = boolPtrFromNull(richFormatting)
	rec.StreamingEnabled = boolPtrFromNull(streaming)
	return rec, true
}

const userBotColumns = `tenant_id, bot_id, bot_username, token, status, last_update_offset, COALESCE(ai_role_id, 0), rich_formatting_enabled, streaming_enabled`

// UserBotByID returns one specific bot of the tenant.
func (r *Repository) UserBotByID(tenantID string, botID int64) (UserBotRecord, bool) {
	if r.db != nil {
		row := r.db.QueryRow(
			`SELECT `+userBotColumns+` FROM user_bots WHERE tenant_id = $1 AND bot_id = $2`, tenantID, botID)
		return scanUserBot(row)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.userBots[botStoreKey(tenantID, botID)]
	return rec, ok
}

// UserBotsByTenant returns every bot the tenant owns, ordered by bot id for a
// stable UI listing.
func (r *Repository) UserBotsByTenant(tenantID string) []UserBotRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT `+userBotColumns+` FROM user_bots WHERE tenant_id = $1 ORDER BY bot_id`, tenantID)
		if err != nil {
			r.logWriteError("list_tenant_bots", tenantID, "", err)
			return nil
		}
		defer rows.Close()
		var out []UserBotRecord
		for rows.Next() {
			if rec, ok := scanUserBot(rows); ok {
				out = append(out, rec)
			}
		}
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UserBotRecord, 0)
	for _, rec := range r.userBots {
		if rec.TenantID == tenantID {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BotID < out[j].BotID })
	return out
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
	for key, rec := range r.userBots {
		if rec.BotID == botID {
			rec.LastUpdateOffset = offset
			r.userBots[key] = rec
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
	for key, rec := range r.userBots {
		if rec.BotID == botID {
			rec.Status = status
			r.userBots[key] = rec
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

// DeleteUserBot removes one specific bot registration of the tenant, leaving the
// tenant's other bots untouched.
func (r *Repository) DeleteUserBot(tenantID string, botID int64) {
	r.mu.Lock()
	delete(r.userBots, botStoreKey(tenantID, botID))
	r.mu.Unlock()
	if r.db != nil {
		_, err := r.db.Exec(`DELETE FROM user_bots WHERE tenant_id = $1 AND bot_id = $2`, tenantID, botID)
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
