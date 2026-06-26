// Package store provides the repository used by the DoubleU backend.
package store

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// BusinessConnectionRecord stores Telegram Business connection state.
type BusinessConnectionRecord struct {
	ConnectionID string
	TenantID     string
	UserID       string
	UserChatID   string
	CanReply     bool
	IsEnabled    bool
	UpdatedAt    time.Time
}

// ChatRecord stores an observed Telegram chat that the owner can configure.
type ChatRecord struct {
	TenantID             string
	ChatID               string
	DisplayName          string
	ChatType             string
	BusinessConnectionID string
	LastSourceMessageID  string
	LastSeenAt           time.Time
	MessageCount         int
	// Status tracks ownership lifecycle: "unclaimed" → awaiting owner decision,
	// "managed" → owner actively using this chat. Defaults to "managed".
	Status string
	// Transport is the channel the chat's messages arrive through ("business"
	// or "bot"). Fixed by the first message; replies go out the same channel.
	Transport string
}

// UnclaimedChatRecord stores a business chat that arrived before the owner claimed it.
// It maps to the unclaimed_chats table and replaces the "staging:<bc_id>" fake-tenant approach.
type UnclaimedChatRecord struct {
	StagingID            string // businessConnectionID + "::" + chatID
	OwnerTenantID        string // real owner tenant; empty when unknown
	BusinessConnectionID string
	ChatID               string
	DisplayName          string
	ChatType             string
	FirstMessageAt       time.Time
	LastMessageAt        time.Time
	MessageCount         int
	Status               string // "pending", "claimed", "dismissed", "expired"
}

// IsChatClaimed returns true only when the chat has status "managed" for this tenant.
// Unclaimed chats (status="unclaimed") are not considered claimed.
func (r *Repository) IsChatClaimed(tenantID, chatID string) bool {
	if r.db != nil {
		var count int
		_ = r.db.QueryRow(
			`SELECT COUNT(*) FROM chats WHERE tenant_id = $1 AND chat_id = $2 AND status = 'managed'`,
			tenantID, chatID,
		).Scan(&count)
		return count > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	chat, ok := r.chats[chatKey(tenantID, chatID)]
	return ok && (chat.Status == "" || chat.Status == "managed")
}

// SaveUnclaimedChat stores or updates a business chat awaiting owner decision.
// Subsequent calls for the same (businessConnectionID, chatID) pair increment MessageCount
// and update LastMessageAt; the first_message_at is preserved.
func (r *Repository) SaveUnclaimedChat(record UnclaimedChatRecord) {
	if record.StagingID == "" {
		record.StagingID = record.BusinessConnectionID + "::" + record.ChatID
	}
	if record.Status == "" {
		record.Status = "pending"
	}
	r.mu.Lock()
	if existing, ok := r.unclaimedChats[record.StagingID]; ok && existing.Status == "pending" {
		record.FirstMessageAt = existing.FirstMessageAt
		record.MessageCount = existing.MessageCount + 1
	}
	if record.FirstMessageAt.IsZero() {
		record.FirstMessageAt = record.LastMessageAt
	}
	r.unclaimedChats[record.StagingID] = record
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	if record.OwnerTenantID != "" {
		r.ensureTenant(record.OwnerTenantID, "")
	}
	_, _ = r.db.Exec(
		`INSERT INTO unclaimed_chats (
		   staging_id, owner_tenant_id, business_connection_id, chat_id,
		   display_name, chat_type, first_message_at, last_message_at, message_count
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $7, 1)
		 ON CONFLICT (staging_id) DO UPDATE SET
		   last_message_at  = EXCLUDED.last_message_at,
		   message_count    = unclaimed_chats.message_count + 1,
		   display_name     = EXCLUDED.display_name,
		   owner_tenant_id  = COALESCE(unclaimed_chats.owner_tenant_id, EXCLUDED.owner_tenant_id)`,
		record.StagingID,
		nullableString(record.OwnerTenantID),
		record.BusinessConnectionID,
		record.ChatID,
		record.DisplayName,
		valueOrDefault(record.ChatType, "private"),
		record.LastMessageAt,
	)
}

// MarkUnclaimedChatClaimed marks a pending unclaimed chat as claimed.
func (r *Repository) MarkUnclaimedChatClaimed(businessConnectionID, chatID string) {
	key := businessConnectionID + "::" + chatID
	r.mu.Lock()
	if uc, ok := r.unclaimedChats[key]; ok {
		uc.Status = "claimed"
		r.unclaimedChats[key] = uc
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`UPDATE unclaimed_chats SET status = 'claimed', claimed_at = now()
		  WHERE business_connection_id = $1 AND chat_id = $2 AND status = 'pending'`,
		businessConnectionID, chatID,
	)
}

// UnclaimedChatsForOwner returns pending unclaimed chats visible to ownerTenantID, as ChatRecords.
func (r *Repository) UnclaimedChatsForOwner(ownerTenantID string) []ChatRecord {
	if r.db != nil {
		return r.dbUnclaimedChatsForOwner(ownerTenantID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unclaimedChatsForOwnerLocked(ownerTenantID)
}

// StagingChatsForConnection returns pending unclaimed chats for a business connection.
func (r *Repository) StagingChatsForConnection(connectionID string) []ChatRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT business_connection_id, chat_id, display_name, chat_type, last_message_at
			   FROM unclaimed_chats
			  WHERE business_connection_id = $1 AND status = 'pending'`,
			connectionID,
		)
		if err == nil {
			defer rows.Close()
			chats := make([]ChatRecord, 0)
			for rows.Next() {
				var uc UnclaimedChatRecord
				if rows.Scan(&uc.BusinessConnectionID, &uc.ChatID, &uc.DisplayName, &uc.ChatType, &uc.LastMessageAt) == nil {
					chats = append(chats, unclaimedToChat(uc))
				}
			}
			return chats
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	chats := make([]ChatRecord, 0)
	for _, uc := range r.unclaimedChats {
		if uc.BusinessConnectionID == connectionID && uc.Status == "pending" {
			chats = append(chats, unclaimedToChat(uc))
		}
	}
	return chats
}

// UnclaimedChatByConnection returns the pending unclaimed record for a (connectionID, chatID) pair.
func (r *Repository) UnclaimedChatByConnection(connectionID, chatID string) (UnclaimedChatRecord, bool) {
	key := connectionID + "::" + chatID
	if r.db != nil {
		var uc UnclaimedChatRecord
		var ownerTenantID sql.NullString
		err := r.db.QueryRow(
			`SELECT staging_id, owner_tenant_id, business_connection_id, chat_id,
			        display_name, chat_type, first_message_at, last_message_at, message_count, status
			   FROM unclaimed_chats
			  WHERE business_connection_id = $1 AND chat_id = $2 AND status = 'pending'`,
			connectionID, chatID,
		).Scan(
			&uc.StagingID, &ownerTenantID, &uc.BusinessConnectionID, &uc.ChatID,
			&uc.DisplayName, &uc.ChatType, &uc.FirstMessageAt, &uc.LastMessageAt,
			&uc.MessageCount, &uc.Status,
		)
		if err != nil {
			return UnclaimedChatRecord{}, false
		}
		uc.OwnerTenantID = ownerTenantID.String
		return uc, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	uc, ok := r.unclaimedChats[key]
	if !ok || uc.Status != "pending" {
		return UnclaimedChatRecord{}, false
	}
	return uc, true
}

// SaveBusinessConnection stores the latest Telegram Business connection state.
func (r *Repository) SaveBusinessConnection(record BusinessConnectionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.businessConnections[record.ConnectionID] = record
	r.connectionTenants[record.ConnectionID] = record.TenantID
	if record.UserID != "" {
		r.connectionByUser[record.UserID] = record.ConnectionID
	}
	if r.db != nil {
		r.ensureTenant(record.TenantID, record.UserID)
		_, _ = r.db.Exec(
			`INSERT INTO telegram_business_connections (
			   id, tenant_id, telegram_connection_id, telegram_user_id, permissions_json, is_active
			 ) VALUES ($1, $2, $3, $4, jsonb_build_object('can_reply', $5::boolean), $6)
			 ON CONFLICT (telegram_connection_id) DO UPDATE SET
			   tenant_id = EXCLUDED.tenant_id,
			   telegram_user_id = EXCLUDED.telegram_user_id,
			   permissions_json = EXCLUDED.permissions_json,
			   is_active = EXCLUDED.is_active,
			   updated_at = now()`,
			"tbc:"+record.ConnectionID,
			record.TenantID,
			record.ConnectionID,
			record.UserID,
			record.CanReply,
			record.IsEnabled,
		)
	}
}

// BusinessConnection returns a Telegram Business connection by ID.
func (r *Repository) BusinessConnection(connectionID string) (BusinessConnectionRecord, bool) {
	if r.db != nil {
		var record BusinessConnectionRecord
		err := r.db.QueryRow(
			`SELECT telegram_connection_id, tenant_id, telegram_user_id,
			        COALESCE((permissions_json->>'can_reply')::boolean, false), is_active, updated_at
			   FROM telegram_business_connections
			  WHERE telegram_connection_id = $1`,
			connectionID,
		).Scan(&record.ConnectionID, &record.TenantID, &record.UserID, &record.CanReply, &record.IsEnabled, &record.UpdatedAt)
		if err == nil {
			record.UserChatID = record.UserID
			return record, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.businessConnections[connectionID]
	return record, ok
}

// BusinessConnectionByUser returns the latest Telegram Business connection for a user.
func (r *Repository) BusinessConnectionByUser(userID string) (BusinessConnectionRecord, bool) {
	if r.db != nil {
		var record BusinessConnectionRecord
		err := r.db.QueryRow(
			`SELECT telegram_connection_id, tenant_id, telegram_user_id,
			        COALESCE((permissions_json->>'can_reply')::boolean, false), is_active, updated_at
			   FROM telegram_business_connections
			  WHERE telegram_user_id = $1
			  ORDER BY updated_at DESC
			  LIMIT 1`,
			userID,
		).Scan(&record.ConnectionID, &record.TenantID, &record.UserID, &record.CanReply, &record.IsEnabled, &record.UpdatedAt)
		if err == nil {
			record.UserChatID = record.UserID
			return record, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	connectionID, ok := r.connectionByUser[userID]
	if !ok {
		return BusinessConnectionRecord{}, false
	}
	record, ok := r.businessConnections[connectionID]
	return record, ok
}

// BindBusinessConnectionTenant links observed business messages to an owner tenant.
func (r *Repository) BindBusinessConnectionTenant(connectionID, tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if connectionID == "" || tenantID == "" {
		return
	}
	r.connectionTenants[connectionID] = tenantID
	if record, ok := r.businessConnections[connectionID]; ok {
		record.TenantID = tenantID
		r.businessConnections[connectionID] = record
	}
	if r.db != nil {
		r.ensureTenant(tenantID, "")
		_, _ = r.db.Exec(
			`UPDATE telegram_business_connections
			    SET tenant_id = $1, updated_at = now()
			  WHERE telegram_connection_id = $2`,
			tenantID,
			connectionID,
		)
	}
}

// TenantForBusinessConnection returns the owner tenant bound to a business connection.
func (r *Repository) TenantForBusinessConnection(connectionID string) (string, bool) {
	if r.db != nil {
		var tenantID string
		err := r.db.QueryRow(
			`SELECT tenant_id FROM telegram_business_connections WHERE telegram_connection_id = $1`,
			connectionID,
		).Scan(&tenantID)
		if err == nil {
			return tenantID, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tenantID, ok := r.connectionTenants[connectionID]
	return tenantID, ok
}

// ChatSettingsForTenant returns all chat settings for a tenant.
func (r *Repository) ChatSettingsForTenant(tenantID string) []contracts.ChatSettings {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT tenant_id, chat_id, mode::text, privacy_flags::text[]
			   FROM chat_settings
			  WHERE tenant_id = $1`,
			tenantID,
		)
		if err == nil {
			defer rows.Close()
			settings := make([]contracts.ChatSettings, 0)
			for rows.Next() {
				var item contracts.ChatSettings
				var mode string
				var flags pq.StringArray
				if rows.Scan(&item.TenantID, &item.ChatID, &mode, &flags) == nil {
					item.Mode = contracts.ChatMode(mode)
					item.PrivacyFlags = privacyFlagsFromStrings([]string(flags))
					settings = append(settings, item.WithDefaults())
				}
			}
			return settings
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	settings := make([]contracts.ChatSettings, 0)
	for _, value := range r.chatSettings {
		if value.TenantID == tenantID {
			settings = append(settings, value.WithDefaults())
		}
	}
	return settings
}

// SaveChat stores an observed business chat for owner-facing selection.
// Status defaults to "managed" when not set; status never downgrades from "managed" on conflict.
func (r *Repository) SaveChat(record ChatRecord) {
	if record.Status == "" {
		record.Status = "managed"
	}
	if record.Transport == "" {
		record.Transport = contracts.TransportBusiness
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Transport is fixed by the first message: keep the original channel
	// when the chat row already exists (mirrors the SQL ON CONFLICT below).
	if existing, ok := r.chats[chatKey(record.TenantID, record.ChatID)]; ok && existing.Transport != "" {
		record.Transport = existing.Transport
	}
	r.chats[chatKey(record.TenantID, record.ChatID)] = record
	if r.db != nil {
		r.ensureTenant(record.TenantID, "")
		_, _ = r.db.Exec(
			`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type, display_name, status, transport)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   telegram_chat_id = EXCLUDED.telegram_chat_id,
			   chat_type        = EXCLUDED.chat_type,
			   display_name     = EXCLUDED.display_name,
			   status           = CASE WHEN EXCLUDED.status = 'managed' THEN 'managed'
			                          ELSE chats.status END,
			   updated_at       = now()`,
			record.TenantID,
			record.ChatID,
			record.ChatID,
			valueOrDefault(record.ChatType, "private"),
			record.DisplayName,
			record.Status,
			record.Transport,
		)
	}
}

// ChatsForTenant returns chats visible to a tenant: managed claims plus pending
// unclaimed chats whose ownership has not been decided yet. Managed entries win
// when both exist for the same chat_id so a claimed chat is never duplicated by
// its prior unclaimed row.
func (r *Repository) ChatsForTenant(tenantID string) []ChatRecord {
	if r.db != nil {
		chats := r.dbChatsForTenant(tenantID)
		seen := make(map[string]struct{}, len(chats))
		for _, c := range chats {
			seen[c.ChatID] = struct{}{}
		}
		for _, uc := range r.dbUnclaimedChatsForOwner(tenantID) {
			if _, ok := seen[uc.ChatID]; ok {
				continue
			}
			seen[uc.ChatID] = struct{}{}
			chats = append(chats, uc)
		}
		sortChats(chats)
		return chats
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	chats := r.chatsForTenantLocked(tenantID)
	sortChats(chats)
	return chats
}

// ClaimChatForTenant promotes a chat to "managed" status under an owner tenant.
// Creates default chat_settings when none exist (mirrors the DB UPDATE trigger).
func (r *Repository) ClaimChatForTenant(tenantID string, chat ChatRecord) ChatRecord {
	chat.TenantID = tenantID
	chat.Status = "managed"
	r.mu.Lock()
	r.chats[chatKey(tenantID, chat.ChatID)] = chat
	if chat.BusinessConnectionID != "" {
		r.connectionTenants[chat.BusinessConnectionID] = tenantID
		key := chat.BusinessConnectionID + "::" + chat.ChatID
		if uc, ok := r.unclaimedChats[key]; ok && uc.Status == "pending" {
			uc.Status = "claimed"
			r.unclaimedChats[key] = uc
		}
	}
	// Bootstrap default settings in memory if not already present (ON CONFLICT DO NOTHING semantics).
	settingsKey := chatKey(tenantID, chat.ChatID)
	if _, hasSettings := r.chatSettings[settingsKey]; !hasSettings {
		r.chatSettings[settingsKey] = contracts.ChatSettings{
			TenantID:     tenantID,
			ChatID:       chat.ChatID,
			Mode:         contracts.ChatModeManual,
			PrivacyFlags: contracts.PrivacyFlags{},
		}.WithDefaults()
	}
	r.mu.Unlock()
	if r.db != nil {
		r.ensureTenant(tenantID, "")
		_, _ = r.db.Exec(
			`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type, display_name, status)
			 VALUES ($1, $2, $3, $4, $5, 'managed')
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   display_name = EXCLUDED.display_name,
			   chat_type    = EXCLUDED.chat_type,
			   status       = 'managed',
			   updated_at   = now()`,
			tenantID,
			chat.ChatID,
			chat.ChatID,
			valueOrDefault(chat.ChatType, "private"),
			chat.DisplayName,
		)
		// Explicit bootstrap: the trigger fires on INSERT; on UPDATE (unclaimed→managed) we insert manually.
		_, _ = r.db.Exec(
			`INSERT INTO chat_settings (tenant_id, chat_id, mode)
			 VALUES ($1, $2, 'manual')
			 ON CONFLICT DO NOTHING`,
			tenantID, chat.ChatID,
		)
		if chat.BusinessConnectionID != "" {
			_, _ = r.db.Exec(
				`UPDATE telegram_business_connections
				    SET tenant_id = $1, updated_at = now()
				  WHERE telegram_connection_id = $2`,
				tenantID,
				chat.BusinessConnectionID,
			)
			_, _ = r.db.Exec(
				`UPDATE unclaimed_chats SET status = 'claimed', claimed_at = now()
				  WHERE business_connection_id = $1 AND chat_id = $2 AND status = 'pending'`,
				chat.BusinessConnectionID,
				chat.ChatID,
			)
		}
	}
	return chat
}

func (r *Repository) chatsForTenantLocked(tenantID string) []ChatRecord {
	chats := make([]ChatRecord, 0)
	seen := make(map[string]struct{})
	for _, chat := range r.chats {
		if chat.TenantID == tenantID && (chat.Status == "" || chat.Status == "managed") {
			chats = append(chats, chat)
			seen[chat.ChatID] = struct{}{}
		}
	}
	// Pending unclaimed chats visible to this tenant are surfaced alongside
	// managed claims so /chats and /mode_* can act on a chat before the owner
	// has explicitly claimed it. Managed entries (above) take precedence.
	for _, uc := range r.unclaimedChatsForOwnerLocked(tenantID) {
		if _, ok := seen[uc.ChatID]; ok {
			continue
		}
		seen[uc.ChatID] = struct{}{}
		chats = append(chats, uc)
	}
	return chats
}

// ChatBySelector resolves either a chat id or 1-based list index from `/chats`.
func (r *Repository) ChatBySelector(tenantID string, selector string) (ChatRecord, bool) {
	if r.db != nil {
		if chat, ok := r.dbChatByID(tenantID, selector); ok {
			return chat, true
		}
		index, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
		if err != nil || index <= 0 {
			return ChatRecord{}, false
		}
		chats := r.ChatsForTenant(tenantID)
		if index > len(chats) {
			return ChatRecord{}, false
		}
		return chats[index-1], true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if chat, ok := r.chats[chatKey(tenantID, selector)]; ok {
		return chat, true
	}
	index, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
	if err != nil || index <= 0 {
		return ChatRecord{}, false
	}
	chats := r.chatsForTenantLocked(tenantID)
	sortChats(chats)
	if index > len(chats) {
		return ChatRecord{}, false
	}
	return chats[index-1], true
}

// unclaimedChatsForOwnerLocked returns pending unclaimed chats visible to tenantID.
// Chats with a known ownerTenantID are returned only to that tenant.
// Chats with an unknown owner (ownerTenantID="") are shown to any real tenant:user:
// whose business connection is not yet bound to a different owner.
func (r *Repository) unclaimedChatsForOwnerLocked(tenantID string) []ChatRecord {
	chats := make([]ChatRecord, 0)
	seen := make(map[string]struct{})
	isRealOwner := strings.HasPrefix(tenantID, "tenant:user:")
	for _, uc := range r.unclaimedChats {
		if uc.Status != "pending" {
			continue
		}
		matches := uc.OwnerTenantID == tenantID
		if !matches && uc.OwnerTenantID == "" && isRealOwner {
			bound, exists := r.connectionTenants[uc.BusinessConnectionID]
			matches = !exists || bound == tenantID
		}
		if !matches {
			continue
		}
		if _, ok := seen[uc.ChatID]; ok {
			continue
		}
		seen[uc.ChatID] = struct{}{}
		chats = append(chats, unclaimedToChat(uc))
	}
	return chats
}

func (r *Repository) dbUnclaimedChatsForOwner(ownerTenantID string) []ChatRecord {
	rows, err := r.db.Query(
		`SELECT business_connection_id, chat_id, display_name, chat_type, last_message_at
		   FROM unclaimed_chats
		  WHERE owner_tenant_id = $1 AND status = 'pending'
		  ORDER BY last_message_at DESC`,
		ownerTenantID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	chats := make([]ChatRecord, 0)
	for rows.Next() {
		var uc UnclaimedChatRecord
		if rows.Scan(&uc.BusinessConnectionID, &uc.ChatID, &uc.DisplayName, &uc.ChatType, &uc.LastMessageAt) == nil {
			chats = append(chats, unclaimedToChat(uc))
		}
	}
	return chats
}

func unclaimedToChat(uc UnclaimedChatRecord) ChatRecord {
	return ChatRecord{
		TenantID:             uc.OwnerTenantID,
		ChatID:               uc.ChatID,
		DisplayName:          uc.DisplayName,
		ChatType:             uc.ChatType,
		BusinessConnectionID: uc.BusinessConnectionID,
		LastSeenAt:           uc.LastMessageAt,
	}
}

func sortChats(chats []ChatRecord) {
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].LastSeenAt.Equal(chats[j].LastSeenAt) {
			return chats[i].ChatID < chats[j].ChatID
		}
		return chats[i].LastSeenAt.After(chats[j].LastSeenAt)
	})
}

func (r *Repository) dbChatsForTenant(tenantID string) []ChatRecord {
	rows, err := r.db.Query(
		`SELECT c.tenant_id, c.chat_id, c.display_name, c.chat_type,
		        COALESCE((
		          SELECT m.business_connection_id
		            FROM messages m
		           WHERE m.tenant_id = c.tenant_id AND m.chat_id = c.chat_id
		           ORDER BY m.received_at DESC
		           LIMIT 1
		        ), '') AS business_connection_id,
		        COALESCE((
		          SELECT m.source_message_id
		            FROM messages m
		           WHERE m.tenant_id = c.tenant_id AND m.chat_id = c.chat_id
		           ORDER BY m.received_at DESC
		           LIMIT 1
		        ), '') AS last_source_message_id,
		        c.updated_at,
		        COALESCE(c.transport, 'business')
		   FROM chats c
		  WHERE c.tenant_id = $1 AND c.status = 'managed'`,
		tenantID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanChats(rows)
}

// Chat returns the ChatRecord for a specific tenant/chat pair.
func (r *Repository) Chat(tenantID, chatID string) (ChatRecord, bool) {
	if r.db != nil {
		return r.dbChatByID(tenantID, chatID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.chats[chatKey(tenantID, chatID)]
	return record, ok
}

func (r *Repository) dbChatByID(tenantID, chatID string) (ChatRecord, bool) {
	rows, err := r.db.Query(
		`SELECT c.tenant_id, c.chat_id, c.display_name, c.chat_type,
		        COALESCE((
		          SELECT m.business_connection_id
		            FROM messages m
		           WHERE m.tenant_id = c.tenant_id AND m.chat_id = c.chat_id
		           ORDER BY m.received_at DESC
		           LIMIT 1
		        ), '') AS business_connection_id,
		        COALESCE((
		          SELECT m.source_message_id
		            FROM messages m
		           WHERE m.tenant_id = c.tenant_id AND m.chat_id = c.chat_id
		           ORDER BY m.received_at DESC
		           LIMIT 1
		        ), '') AS last_source_message_id,
		        c.updated_at,
		        COALESCE(c.transport, 'business')
		   FROM chats c
		  WHERE c.tenant_id = $1 AND c.chat_id = $2 AND c.status = 'managed'`,
		tenantID,
		chatID,
	)
	if err != nil {
		return ChatRecord{}, false
	}
	defer rows.Close()
	chats := scanChats(rows)
	if len(chats) == 0 {
		return ChatRecord{}, false
	}
	return chats[0], true
}

func scanChats(rows *sql.Rows) []ChatRecord {
	chats := make([]ChatRecord, 0)
	for rows.Next() {
		var chat ChatRecord
		if rows.Scan(
			&chat.TenantID,
			&chat.ChatID,
			&chat.DisplayName,
			&chat.ChatType,
			&chat.BusinessConnectionID,
			&chat.LastSourceMessageID,
			&chat.LastSeenAt,
			&chat.Transport,
		) == nil {
			chats = append(chats, chat)
		}
	}
	return chats
}

// IncrementChatMessageCount increments the incoming message counter for a chat and returns the new count.
func (r *Repository) IncrementChatMessageCount(tenantID, chatID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := chatKey(tenantID, chatID)
	r.messageCounts[key]++
	return r.messageCounts[key]
}

// ChatMessageCount returns the stored incoming message count for a chat.
func (r *Repository) ChatMessageCount(tenantID, chatID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messageCounts[chatKey(tenantID, chatID)]
}

// OwnerTenantForBusinessConnection returns the owner tenant ID for a given business_connection_id.
func (r *Repository) OwnerTenantForBusinessConnection(businessConnectionID string) string {
	if r.db != nil {
		var tenantID string
		err := r.db.QueryRow(
			`SELECT c.tenant_id
			   FROM chats c
			  WHERE c.tenant_id LIKE 'tenant:user:%'
			    AND EXISTS (
			      SELECT 1 FROM messages m
			       WHERE m.tenant_id = c.tenant_id
			         AND m.chat_id   = c.chat_id
			         AND m.business_connection_id = $1
			    )
			  ORDER BY c.updated_at DESC
			  LIMIT 1`,
			businessConnectionID,
		).Scan(&tenantID)
		if err == nil && tenantID != "" {
			return tenantID
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, chat := range r.chats {
		if chat.BusinessConnectionID == businessConnectionID &&
			strings.HasPrefix(chat.TenantID, "tenant:user:") {
			return chat.TenantID
		}
	}
	return ""
}

// AvailableModesForChat returns the modes the owner is allowed to select for this chat.
func (r *Repository) AvailableModesForChat(tenantID, chatID string) []contracts.ChatMode {
	return contracts.UnlockedModesForScore(r.ContextScoreForChat(tenantID, chatID))
}

// ChatPausedHours returns the number of hours since the last message in a chat.
// Uses the view_chat_paused_hours SQL function when a DB is configured.
func (r *Repository) ChatPausedHours(tenantID, chatID string) float64 {
	if r.db != nil {
		var hours float64
		err := r.db.QueryRow(
			`SELECT COALESCE(view_chat_paused_hours($1, $2), 0)`,
			tenantID, chatID,
		).Scan(&hours)
		if err == nil {
			return hours
		}
	}
	return 0
}
