// Package store provides the repository used by the Technical MVP.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// MessageRecord stores operational state for a normalized message.
type MessageRecord struct {
	Event  contracts.NormalizedTelegramEvent
	States []contracts.MessageState
}

// DraftStatus is the owner approval lifecycle status for a draft.
type DraftStatus string

const (
	// DraftPending means the draft is waiting for owner action.
	DraftPending DraftStatus = "pending"
	// DraftApproved means the owner approved the draft.
	DraftApproved DraftStatus = "approved"
	// DraftRejected means the owner rejected the draft.
	DraftRejected DraftStatus = "rejected"
	// DraftSent means the approved draft was sent to the recipient.
	DraftSent DraftStatus = "sent"
)

// DraftRecord stores an owner-facing draft.
type DraftRecord struct {
	DraftID              string
	DecisionID           string
	TenantID             string
	ChatID               string
	BusinessConnectionID string
	SourceMessageID      string
	SourceMessageText    string // original contact message text; stored so regen works even with no_memory
	ResponseText         string
	Status               DraftStatus
	OwnerChatID          string   // owner's bot chat ID (for editing the draft card)
	OwnerMessageID       string   // Telegram message_id of the draft card in owner's chat
	SentMessageID        string   // Telegram message_id of the reply sent to the contact
	ActionHistory        []string // sequence of modification actions taken before final approve
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// MemoryFactRecord stores a durable fact extracted from a conversation.
type MemoryFactRecord struct {
	TenantID        string
	ChatID          string
	FactType        string
	FactText        string
	Confidence      float64
	SourceMessageID string
}

// SendRecord stores an outbound send idempotency record.
type SendRecord struct {
	Key             string
	TenantID        string
	ChatID          string
	SourceMessageID string
	DecisionID      string
	Text            string
	CreatedAt       time.Time
}

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
}

// Repository stores MVP state in memory.
type Repository struct {
	mu                  sync.Mutex
	db                  *sql.DB
	chatSettings        map[string]contracts.ChatSettings
	receiveKeys         map[string]struct{}
	sendKeys            map[string]SendRecord
	callbackKeys        map[string]struct{}
	messages            map[string]MessageRecord
	audits              []contracts.AuditEvent
	drafts              map[string]DraftRecord
	draftsByMsg         map[string]string
	businessConnections map[string]BusinessConnectionRecord
	pendingInputs       map[string]contracts.PendingInput
	draftFeedbacks      []contracts.DraftFeedback
	contactProfiles     map[string]contracts.ContactProfile
	messageCounts       map[string]int
	memoryFacts         []MemoryFactRecord
	connectionByUser    map[string]string
	connectionTenants   map[string]string
	chats               map[string]ChatRecord
}

// NewRepository creates an empty repository.
func NewRepository() *Repository {
	return &Repository{
		chatSettings:        make(map[string]contracts.ChatSettings),
		receiveKeys:         make(map[string]struct{}),
		sendKeys:            make(map[string]SendRecord),
		callbackKeys:        make(map[string]struct{}),
		messages:            make(map[string]MessageRecord),
		drafts:              make(map[string]DraftRecord),
		draftsByMsg:         make(map[string]string),
		businessConnections: make(map[string]BusinessConnectionRecord),
		connectionByUser:    make(map[string]string),
		connectionTenants:   make(map[string]string),
		chats:               make(map[string]ChatRecord),
		pendingInputs:       make(map[string]contracts.PendingInput),
		contactProfiles:     make(map[string]contracts.ContactProfile),
		messageCounts:       make(map[string]int),
	}
}

// NewPostgresRepository creates a repository backed by Postgres plus an in-memory cache.
func NewPostgresRepository(databaseURL string, migrationPaths ...string) (*Repository, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	repository := NewRepository()
	repository.db = db
	if err := repository.applyMigrations(migrationPaths...); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repository, nil
}

// Close releases database resources held by the repository.
func (r *Repository) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *Repository) applyMigrations(paths ...string) error {
	if r.db == nil {
		return nil
	}
	applied := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // file not present in this environment; skip
		}
		if _, err = r.db.Exec(string(data)); err != nil {
			return fmt.Errorf("migration %s: %w", path, err)
		}
		applied++
	}
	_ = applied
	return nil
}

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

// MarkReceive returns true when the receive idempotency key was already seen.
func (r *Repository) MarkReceive(key string) bool {
	if r.db != nil {
		return r.markIdempotency(key, "receive_update", SendRecord{})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.receiveKeys[key]; ok {
		return true
	}
	r.receiveKeys[key] = struct{}{}
	return false
}

// MarkCallback returns true when the approval callback key was already seen.
func (r *Repository) MarkCallback(key string) bool {
	if r.db != nil {
		return r.markIdempotency(key, "approval_callback", SendRecord{})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.callbackKeys[key]; ok {
		return true
	}
	r.callbackKeys[key] = struct{}{}
	return false
}

// MarkSend returns true when the send idempotency key was already seen.
func (r *Repository) MarkSend(record SendRecord) bool {
	if r.db != nil {
		return r.markIdempotency(record.Key, "send_reply", record)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sendKeys[record.Key]; ok {
		return true
	}
	r.sendKeys[record.Key] = record
	return false
}

// SaveMessage stores a normalized message record.
func (r *Repository) SaveMessage(event contracts.NormalizedTelegramEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := messageKey(event.TenantID, event.ChatID, event.SourceMessageID)
	record := r.messages[key]
	record.Event = event
	r.messages[key] = record
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO messages (
			   tenant_id, chat_id, source_message_id, event_id, telegram_update_id,
			   business_connection_id, sender_id, message_text, message_type, received_at
			 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			 ON CONFLICT (tenant_id, chat_id, source_message_id) DO UPDATE SET
			   event_id = EXCLUDED.event_id,
			   telegram_update_id = EXCLUDED.telegram_update_id,
			   business_connection_id = EXCLUDED.business_connection_id,
			   sender_id = EXCLUDED.sender_id,
			   message_text = EXCLUDED.message_text,
			   message_type = EXCLUDED.message_type`,
			event.TenantID,
			event.ChatID,
			event.SourceMessageID,
			event.EventID,
			event.TelegramUpdateID,
			event.BusinessConnectionID,
			event.SenderID,
			event.MessageText,
			event.MessageType,
			event.CreatedAt,
		)
		for _, state := range record.States {
			r.insertState(event.TenantID, event.ChatID, event.SourceMessageID, state)
		}
		r.saveMessageMemory(event)
	}
}

// Message returns a stored message and whether it exists.
func (r *Repository) Message(tenantID, chatID, sourceMessageID string) (MessageRecord, bool) {
	if r.db != nil {
		var event contracts.NormalizedTelegramEvent
		err := r.db.QueryRow(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at
			   FROM messages
			  WHERE tenant_id = $1 AND chat_id = $2 AND source_message_id = $3`,
			tenantID,
			chatID,
			sourceMessageID,
		).Scan(
			&event.EventID,
			&event.TenantID,
			&event.ChatID,
			&event.TelegramUpdateID,
			&event.BusinessConnectionID,
			&event.SourceMessageID,
			&event.SenderID,
			&event.MessageText,
			&event.MessageType,
			&event.CreatedAt,
		)
		if err == nil {
			return MessageRecord{Event: event}, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.messages[messageKey(tenantID, chatID, sourceMessageID)]
	if !ok || record.Event.EventID == "" {
		return MessageRecord{}, false
	}
	return record, true
}

// AppendState appends a message state to the operational record.
func (r *Repository) AppendState(tenantID, chatID, sourceMessageID string, state contracts.MessageState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := messageKey(tenantID, chatID, sourceMessageID)
	record := r.messages[key]
	record.States = append(record.States, state)
	r.messages[key] = record
	// Only write to DB if the message row exists (EventID is set by SaveMessage).
	// AppendState may be called before SaveMessage for early lifecycle states; those
	// are tracked in-memory only to avoid FK violations on message_states → messages.
	if r.db != nil && record.Event.EventID != "" {
		r.insertState(tenantID, chatID, sourceMessageID, state)
	}
}

// AppendAudit stores an audit event.
func (r *Repository) AppendAudit(event contracts.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, event)
	if r.db == nil {
		return
	}
	// Only write to DB if the parent message row exists (EventID is set by SaveMessage).
	// Audit events for early lifecycle states (before SaveMessage) are tracked in-memory only
	// to avoid FK violations on audit_events → messages.
	msgRecord := r.messages[messageKey(event.TenantID, event.ChatID, event.SourceMessageID)]
	if msgRecord.Event.EventID == "" {
		return
	}
	_, _ = r.db.Exec(
		`INSERT INTO audit_events (tenant_id, chat_id, source_message_id, decision, reason)
		 VALUES ($1, $2, $3, $4::decision, $5)
		 ON CONFLICT (tenant_id, chat_id, source_message_id, decision, reason) DO NOTHING`,
		event.TenantID,
		event.ChatID,
		event.SourceMessageID,
		string(event.Decision),
		event.Reason,
	)
}

// Audits returns a copy of all audit events.
func (r *Repository) Audits() []contracts.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.AuditEvent(nil), r.audits...)
}

// SaveDraft stores an owner-facing draft.
func (r *Repository) SaveDraft(draft DraftRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drafts[draft.DraftID] = draft
	r.draftsByMsg[messageKey(draft.TenantID, draft.ChatID, draft.SourceMessageID)] = draft.DraftID
}

// Draft returns a draft by ID.
func (r *Repository) Draft(draftID string) (DraftRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	draft, ok := r.drafts[draftID]
	return draft, ok
}

// DraftByMessage returns the latest draft for a message.
func (r *Repository) DraftByMessage(tenantID, chatID, sourceMessageID string) (DraftRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	draftID, ok := r.draftsByMsg[messageKey(tenantID, chatID, sourceMessageID)]
	if !ok {
		return DraftRecord{}, false
	}
	draft, ok := r.drafts[draftID]
	return draft, ok
}

// IsChatClaimed returns true when the chat has been explicitly added to this tenant.
// Staging chats (tenant_id starting with "staging:") are NOT considered claimed.
func (r *Repository) IsChatClaimed(tenantID, chatID string) bool {
	if r.db != nil {
		var count int
		_ = r.db.QueryRow(
			`SELECT COUNT(*) FROM chats WHERE tenant_id = $1 AND chat_id = $2`,
			tenantID, chatID,
		).Scan(&count)
		return count > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.chats[chatKey(tenantID, chatID)]
	return ok
}

// StagingChatsForConnection returns unclaimed chats observed on a business connection.
func (r *Repository) StagingChatsForConnection(connectionID string) []ChatRecord {
	return r.ChatsForTenant("staging:" + connectionID)
}

// DraftsForChat returns all drafts for a tenant/chat pair, newest first.
func (r *Repository) DraftsForChat(tenantID, chatID string) []DraftRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []DraftRecord
	for _, draft := range r.drafts {
		if draft.TenantID == tenantID && draft.ChatID == chatID {
			result = append(result, draft)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// UpdateDraft stores a modified draft.
func (r *Repository) UpdateDraft(draft DraftRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drafts[draft.DraftID] = draft
}

// DraftBySentMessageID returns the draft whose reply was sent as the given Telegram message_id.
func (r *Repository) DraftBySentMessageID(tenantID, chatID, sentMessageID string) (DraftRecord, bool) {
	if sentMessageID == "" {
		return DraftRecord{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.drafts {
		if d.TenantID == tenantID && d.ChatID == chatID && d.SentMessageID == sentMessageID {
			return d, true
		}
	}
	return DraftRecord{}, false
}

// SendCount returns the number of unique outbound sends.
func (r *Repository) SendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sendKeys)
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
func (r *Repository) SaveChat(record ChatRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chats[chatKey(record.TenantID, record.ChatID)] = record
	if r.db != nil {
		r.ensureTenant(record.TenantID, "")
		_, _ = r.db.Exec(
			`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type, display_name)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   telegram_chat_id = EXCLUDED.telegram_chat_id,
			   chat_type = EXCLUDED.chat_type,
			   display_name = EXCLUDED.display_name,
			   updated_at = now()`,
			record.TenantID,
			record.ChatID,
			record.ChatID,
			valueOrDefault(record.ChatType, "private"),
			record.DisplayName,
		)
	}
}

// ChatsForTenant returns observed chats for one tenant in stable insertion-independent order.
func (r *Repository) ChatsForTenant(tenantID string) []ChatRecord {
	if r.db != nil {
		chats := r.dbChatsForTenant(tenantID)
		if len(chats) == 0 && strings.HasPrefix(tenantID, "tenant:user:") {
			chats = r.dbUnclaimedBusinessChats()
		}
		sortChats(chats)
		return chats
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	chats := r.chatsForTenantLocked(tenantID)
	if len(chats) == 0 && strings.HasPrefix(tenantID, "tenant:user:") {
		chats = r.unclaimedBusinessChatsLocked()
	}
	sortChats(chats)
	return chats
}

// ClaimChatForTenant stores an observed chat under an owner tenant after explicit selection.
func (r *Repository) ClaimChatForTenant(tenantID string, chat ChatRecord) ChatRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	chat.TenantID = tenantID
	r.chats[chatKey(tenantID, chat.ChatID)] = chat
	if chat.BusinessConnectionID != "" {
		r.connectionTenants[chat.BusinessConnectionID] = tenantID
	}
	if r.db != nil {
		r.ensureTenant(tenantID, "")
		_, _ = r.db.Exec(
			`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type, display_name)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   display_name = EXCLUDED.display_name,
			   chat_type = EXCLUDED.chat_type,
			   updated_at = now()`,
			tenantID,
			chat.ChatID,
			chat.ChatID,
			valueOrDefault(chat.ChatType, "private"),
			chat.DisplayName,
		)
		if chat.BusinessConnectionID != "" {
			_, _ = r.db.Exec(
				`UPDATE telegram_business_connections
				    SET tenant_id = $1, updated_at = now()
				  WHERE telegram_connection_id = $2`,
				tenantID,
				chat.BusinessConnectionID,
			)
		}
	}
	return chat
}

func (r *Repository) chatsForTenantLocked(tenantID string) []ChatRecord {
	chats := make([]ChatRecord, 0)
	for _, chat := range r.chats {
		if chat.TenantID == tenantID {
			chats = append(chats, chat)
		}
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
	if len(chats) == 0 && strings.HasPrefix(tenantID, "tenant:user:") {
		chats = r.unclaimedBusinessChatsLocked()
	}
	sortChats(chats)
	if index > len(chats) {
		return ChatRecord{}, false
	}
	return chats[index-1], true
}

// RecentMessages returns recent stored messages for a tenant/chat, oldest first.
func (r *Repository) RecentMessages(tenantID, chatID string, limit int) []MessageRecord {
	if limit <= 0 {
		return nil
	}
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at
			   FROM (
			     SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			            source_message_id, sender_id, message_text, message_type, received_at
			       FROM messages
			      WHERE tenant_id = $1 AND chat_id = $2
			      ORDER BY received_at DESC
			      LIMIT $3
			   ) recent
			  ORDER BY received_at ASC`,
			tenantID,
			chatID,
			limit,
		)
		if err == nil {
			defer rows.Close()
			messages := make([]MessageRecord, 0)
			for rows.Next() {
				var event contracts.NormalizedTelegramEvent
				if rows.Scan(
					&event.EventID,
					&event.TenantID,
					&event.ChatID,
					&event.TelegramUpdateID,
					&event.BusinessConnectionID,
					&event.SourceMessageID,
					&event.SenderID,
					&event.MessageText,
					&event.MessageType,
					&event.CreatedAt,
				) == nil {
					messages = append(messages, MessageRecord{Event: event})
				}
			}
			return messages
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	messages := make([]MessageRecord, 0)
	for _, record := range r.messages {
		if record.Event.TenantID == tenantID && record.Event.ChatID == chatID && record.Event.EventID != "" {
			messages = append(messages, record)
		}
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Event.CreatedAt.Before(messages[j].Event.CreatedAt)
	})
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return append([]MessageRecord(nil), messages...)
}

func (r *Repository) unclaimedBusinessChatsLocked() []ChatRecord {
	chats := make([]ChatRecord, 0)
	seen := make(map[string]struct{})
	for _, chat := range r.chats {
		if chat.BusinessConnectionID == "" {
			continue
		}
		if strings.HasPrefix(chat.TenantID, "tenant:user:") {
			continue
		}
		if _, claimed := r.connectionTenants[chat.BusinessConnectionID]; claimed {
			continue
		}
		if _, ok := seen[chat.ChatID]; ok {
			continue
		}
		seen[chat.ChatID] = struct{}{}
		chats = append(chats, chat)
	}
	return chats
}

func sortChats(chats []ChatRecord) {
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].LastSeenAt.Equal(chats[j].LastSeenAt) {
			return chats[i].ChatID < chats[j].ChatID
		}
		return chats[i].LastSeenAt.After(chats[j].LastSeenAt)
	})
}

func (r *Repository) markIdempotency(key string, operation string, record SendRecord) bool {
	result, err := r.db.Exec(
		`INSERT INTO idempotency_records (
		   idempotency_key, operation, tenant_id, chat_id, source_message_id, decision_id, completed_at
		 ) VALUES ($1, $2::idempotency_operation, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), now())
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		key,
		operation,
		record.TenantID,
		record.ChatID,
		record.SourceMessageID,
		record.DecisionID,
	)
	if err != nil {
		return false
	}
	affected, err := result.RowsAffected()
	return err == nil && affected == 0
}

func (r *Repository) insertState(tenantID, chatID, sourceMessageID string, state contracts.MessageState) {
	_, _ = r.db.Exec(
		`INSERT INTO message_states (tenant_id, chat_id, source_message_id, state)
		 VALUES ($1, $2, $3, $4::message_state)
		 ON CONFLICT (tenant_id, chat_id, source_message_id, state, actor) DO NOTHING`,
		tenantID,
		chatID,
		sourceMessageID,
		string(state),
	)
}

func (r *Repository) saveMessageMemory(event contracts.NormalizedTelegramEvent) {
	if strings.TrimSpace(event.MessageText) == "" {
		return
	}
	contentHash := contentHash(event.TenantID, event.ChatID, event.SourceMessageID, event.MessageText)
	var factID string
	err := r.db.QueryRow(
		`INSERT INTO memory_facts (
		   tenant_id, chat_id, scope, fact_type, fact_text, confidence, source_message_id, metadata
		 ) VALUES ($1, $2, 'conversation', 'message', $3, 0.700, $4, jsonb_build_object('message_type', $5::text))
		 ON CONFLICT (tenant_id, chat_id, scope, fact_type, source_message_id, fact_text)
		 DO UPDATE SET updated_at = now()
		 RETURNING id::text`,
		event.TenantID,
		event.ChatID,
		event.MessageText,
		event.SourceMessageID,
		event.MessageType,
	).Scan(&factID)
	if err != nil {
		return
	}
	embedding := placeholderEmbedding(event.MessageText, 16)
	_, _ = r.db.Exec(
		`INSERT INTO memory_embeddings (
		   tenant_id, chat_id, memory_fact_id, embedding_model, embedding_dimensions,
		   embedding, vector_backend, content_hash
		 ) VALUES ($1, $2, $3::uuid, 'hash-placeholder-v1', $4, $5, 'placeholder_float8_array', $6)
		 ON CONFLICT (tenant_id, content_hash) DO NOTHING`,
		event.TenantID,
		event.ChatID,
		factID,
		len(embedding),
		pq.Array(embedding),
		contentHash,
	)
}

func (r *Repository) ensureTenant(tenantID, telegramUserID string) {
	if r.db == nil || tenantID == "" {
		return
	}
	var ownerUserID any
	if telegramUserID != "" {
		userID := "user:" + telegramUserID
		_, _ = r.db.Exec(
			`INSERT INTO users (id, telegram_user_id)
			 VALUES ($1, $2)
			 ON CONFLICT (id) DO UPDATE SET telegram_user_id = EXCLUDED.telegram_user_id, updated_at = now()`,
			userID,
			telegramUserID,
		)
		ownerUserID = userID
	}
	_, _ = r.db.Exec(
		`INSERT INTO tenants (id, owner_user_id, name)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET owner_user_id = COALESCE(EXCLUDED.owner_user_id, tenants.owner_user_id), updated_at = now()`,
		tenantID,
		ownerUserID,
		tenantID,
	)
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
		        c.updated_at
		   FROM chats c
		  WHERE c.tenant_id = $1`,
		tenantID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanChats(rows)
}

func (r *Repository) dbUnclaimedBusinessChats() []ChatRecord {
	rows, err := r.db.Query(
		`SELECT DISTINCT ON (m.business_connection_id, c.chat_id)
		        c.tenant_id, c.chat_id, c.display_name, c.chat_type,
		        m.business_connection_id, m.source_message_id, m.received_at
		   FROM chats c
		   JOIN messages m ON m.tenant_id = c.tenant_id AND m.chat_id = c.chat_id
		  WHERE m.business_connection_id <> ''
		    AND c.tenant_id NOT LIKE 'tenant:user:%'
		  ORDER BY m.business_connection_id, c.chat_id, m.received_at DESC`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanChats(rows)
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
		        c.updated_at
		   FROM chats c
		  WHERE c.tenant_id = $1 AND c.chat_id = $2`,
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
		) == nil {
			chats = append(chats, chat)
		}
	}
	return chats
}

// SaveMemoryFact persists an extracted memory fact for a chat.
func (r *Repository) SaveMemoryFact(fact MemoryFactRecord) {
	r.mu.Lock()
	r.memoryFacts = append(r.memoryFacts, fact)
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`INSERT INTO memory_facts (tenant_id, chat_id, scope, fact_type, fact_text, confidence, source_message_id, metadata)
		 VALUES ($1, $2, 'conversation', $3, $4, $5, $6, '{}')
		 ON CONFLICT (tenant_id, chat_id, scope, fact_type, source_message_id, fact_text) DO NOTHING`,
		fact.TenantID,
		fact.ChatID,
		fact.FactType,
		fact.FactText,
		fact.Confidence,
		fact.SourceMessageID,
	)
}

// MemoryFacts returns non-deleted memory facts for a chat.
func (r *Repository) MemoryFacts(tenantID, chatID string, limit int) []MemoryFactRecord {
	if r.db == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		result := make([]MemoryFactRecord, 0)
		for i := len(r.memoryFacts) - 1; i >= 0 && len(result) < limit; i-- {
			f := r.memoryFacts[i]
			if f.TenantID == tenantID && f.ChatID == chatID {
				result = append(result, f)
			}
		}
		return result
	}
	rows, err := r.db.Query(
		`SELECT fact_type, fact_text, confidence, COALESCE(source_message_id,'')
		   FROM memory_facts
		  WHERE tenant_id = $1 AND chat_id = $2 AND hard_deleted = false
		  ORDER BY created_at DESC
		  LIMIT $3`,
		tenantID, chatID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]MemoryFactRecord, 0)
	for rows.Next() {
		var f MemoryFactRecord
		f.TenantID = tenantID
		f.ChatID = chatID
		if rows.Scan(&f.FactType, &f.FactText, &f.Confidence, &f.SourceMessageID) == nil {
			result = append(result, f)
		}
	}
	return result
}

// SetPendingInput stores a pending-input state for an owner chat (TTL enforced at read time).
func (r *Repository) SetPendingInput(tenantID, chatID string, input contracts.PendingInput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingInputs[chatKey(tenantID, chatID)] = input
}

// PendingInput returns the pending input state for an owner chat, if any and not expired.
func (r *Repository) PendingInput(tenantID, chatID string) (contracts.PendingInput, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	input, ok := r.pendingInputs[chatKey(tenantID, chatID)]
	if !ok {
		return contracts.PendingInput{}, false
	}
	if !input.ExpiresAt.IsZero() && time.Now().After(input.ExpiresAt) {
		delete(r.pendingInputs, chatKey(tenantID, chatID))
		return contracts.PendingInput{}, false
	}
	return input, true
}

// ClearPendingInput removes any pending input state for an owner chat.
func (r *Repository) ClearPendingInput(tenantID, chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingInputs, chatKey(tenantID, chatID))
}

// SaveDraftFeedback stores owner feedback on a generated draft.
func (r *Repository) SaveDraftFeedback(feedback contracts.DraftFeedback) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draftFeedbacks = append(r.draftFeedbacks, feedback)
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO draft_feedback (draft_id, tenant_id, chat_id, rating, feedback_text, response_text, model_used, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (draft_id, rating) DO NOTHING`,
			feedback.DraftID,
			feedback.TenantID,
			feedback.ChatID,
			feedback.Rating,
			feedback.FeedbackText,
			feedback.ResponseText,
			feedback.ModelUsed,
			feedback.CreatedAt,
		)
	}
}

// RecentFeedback returns the most recent draft feedback entries for a chat.
func (r *Repository) RecentFeedback(tenantID, chatID string, limit int) []contracts.DraftFeedback {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT draft_id, tenant_id, chat_id, rating, COALESCE(feedback_text,''), COALESCE(response_text,''), COALESCE(model_used,''), created_at
			   FROM draft_feedback
			  WHERE tenant_id = $1 AND chat_id = $2
			  ORDER BY created_at DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			result := make([]contracts.DraftFeedback, 0)
			for rows.Next() {
				var fb contracts.DraftFeedback
				if rows.Scan(&fb.DraftID, &fb.TenantID, &fb.ChatID, &fb.Rating, &fb.FeedbackText, &fb.ResponseText, &fb.ModelUsed, &fb.CreatedAt) == nil {
					result = append(result, fb)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]contracts.DraftFeedback, 0)
	for i := len(r.draftFeedbacks) - 1; i >= 0 && len(result) < limit; i-- {
		fb := r.draftFeedbacks[i]
		if fb.TenantID == tenantID && fb.ChatID == chatID {
			result = append(result, fb)
		}
	}
	return result
}

// SaveContactProfile stores the owner-supplied contact profile for a chat.
func (r *Repository) SaveContactProfile(profile contracts.ContactProfile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contactProfiles[chatKey(profile.TenantID, profile.ChatID)] = profile
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO contact_profiles (tenant_id, chat_id, name, gender, role, relationship_type, notes, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   name = EXCLUDED.name, gender = EXCLUDED.gender, role = EXCLUDED.role,
			   relationship_type = EXCLUDED.relationship_type, notes = EXCLUDED.notes,
			   updated_at = now()`,
			profile.TenantID, profile.ChatID,
			profile.Name, profile.Gender, profile.Role, profile.RelationshipType, profile.Notes,
		)
	}
}

// ContactProfile returns the contact profile for a chat.
func (r *Repository) ContactProfile(tenantID, chatID string) (contracts.ContactProfile, bool) {
	if r.db != nil {
		var p contracts.ContactProfile
		p.TenantID = tenantID
		p.ChatID = chatID
		err := r.db.QueryRow(
			`SELECT COALESCE(name,''), COALESCE(gender,''), COALESCE(role,''), COALESCE(relationship_type,''), COALESCE(notes,'')
			   FROM contact_profiles WHERE tenant_id=$1 AND chat_id=$2`,
			tenantID, chatID,
		).Scan(&p.Name, &p.Gender, &p.Role, &p.RelationshipType, &p.Notes)
		if err == nil {
			return p, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.contactProfiles[chatKey(tenantID, chatID)]
	return p, ok
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

// OwnerTenantForBusinessConnection returns the owner tenant ID (format "tenant:user:USERID")
// for a given business_connection_id by looking at claimed chats in the DB.
// Used as a fallback when the telegram_business_connections record is missing.
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
	profile, _ := r.ContactProfile(tenantID, chatID)
	facts := r.MemoryFacts(tenantID, chatID, 20)
	feedback := r.RecentFeedback(tenantID, chatID, 10)
	score := contracts.ContextScore(profile, facts, feedback)
	return contracts.UnlockedModesForScore(score)
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

func contentHash(parts ...string) string {
	hash := fnv.New64a()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum64())
}

func placeholderEmbedding(text string, dimensions int) []float64 {
	embedding := make([]float64, dimensions)
	for i := 0; i < dimensions; i++ {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(text))
		_ = binary.Write(hash, binary.LittleEndian, uint64(i))
		value := hash.Sum64() % 2000000
		embedding[i] = (float64(value) / 1000000.0) - 1.0
	}
	return embedding
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func chatKey(tenantID, chatID string) string {
	return tenantID + ":" + chatID
}

func messageKey(tenantID, chatID, sourceMessageID string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, chatID, sourceMessageID)
}
