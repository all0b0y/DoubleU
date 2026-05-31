// Package store provides the repository used by the DoubleU backend.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
	tracepkg "doubleu/backend-go/internal/trace"
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
	// DraftType distinguishes regular reply drafts ("reply") from interactive question
	// cards ("question") where the owner picks one of several AI-suggested options.
	DraftType        string
	SuggestedOptions []string // non-empty when DraftType == "question"
	Status           DraftStatus
	OwnerChatID      string   // owner's bot chat ID (for editing the draft card)
	OwnerMessageID   string   // Telegram message_id of the draft card in owner's chat
	SentMessageID    string   // Telegram message_id of the reply sent to the contact
	ActionHistory    []string // sequence of modification actions taken before final approve
	// TraceID is the W3C trace-id under which this draft was generated.
	// Owner feedback (approve/reject/thumb) attaches a Langfuse score to it.
	TraceID   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SemanticFactRecord stores a typed, atomic fact in the Semantic Layer.
// It maps to the semantic_facts table and replaces the deprecated MemoryFactRecord.
type SemanticFactRecord struct {
	ID       int64
	TenantID string
	// ScopeKind: "chat" or "owner_global".
	ScopeKind string
	ChatID    string
	FactText  string
	// FactType: preference, biographical, commitment, event, relationship, health, taboo.
	FactType string
	// Subject: contact, owner, relationship, third_party.
	Subject    string
	Confidence float64
	// Source: llm_extracted, owner_confirmed, owner_authored.
	Source          string
	SourceMessageID string
	ExpiresAt       *time.Time
	CreatedAt       time.Time
}

// ActiveEpisodeRecord stores the runtime state of an open conversation episode.
// It maps to the active_episodes table.
type ActiveEpisodeRecord struct {
	TenantID           string
	ChatID             string
	StartedAt          time.Time
	LastMessageAt      time.Time
	MessageCount       int
	CurrentTopic       string
	OpenQuestionMsgID  string
	PendingOwnerAction string
}

// EpisodeSummaryRecord stores a compressed summary of a closed episode.
// It maps to the episode_summaries table and replaces daily_summary memory_facts.
type EpisodeSummaryRecord struct {
	ID            int64
	TenantID      string
	ChatID        string
	PeriodStart   time.Time
	PeriodEnd     time.Time
	Content       string
	ClosureReason string
	MessageCount  int
	CreatedAt     time.Time
}

// ExtractionLogRecord captures one async fact-extraction attempt for observability.
// It maps to the extraction_log table so a returned empty fact set is distinguishable
// from a silent failure (model used, verbatim LLM output, created IDs, skip count, errors).
type ExtractionLogRecord struct {
	TenantID        string
	ChatID          string
	InputMessageIDs []string
	ModelUsed       string
	RawOutput       string
	FactsCreated    []int64
	FactsSkipped    int
	FactsMerged     int
	Error           string
	DurationMS      int
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
	// Status tracks ownership lifecycle: "unclaimed" → awaiting owner decision,
	// "managed" → owner actively using this chat. Defaults to "managed".
	Status string
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

// SemanticFactUpsertResult describes the outcome of an UpsertSemanticFact call.
type SemanticFactUpsertResult struct {
	FactID int64
	// Action is "inserted" for a new fact or "occurrence_added" for an exact duplicate.
	Action string
}

// PendingHealthFactRecord is a health fact awaiting owner confirmation before
// being promoted to semantic_facts.  Maps to the pending_global_health_facts table.
type PendingHealthFactRecord struct {
	ID              int64
	TenantID        string
	ChatID          string
	SourceMessageID string
	FactText        string
	Confidence      float64
	Status          string // "pending", "accepted", "rejected", "expired"
}

// Repository stores state — in memory when no DB is configured, in Postgres otherwise.
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
	connectionByUser    map[string]string
	connectionTenants   map[string]string
	chats               map[string]ChatRecord
	unclaimedChats      map[string]UnclaimedChatRecord
	// New memory architecture fields
	semanticFacts    []SemanticFactRecord
	activeEpisodes   map[string]ActiveEpisodeRecord
	episodeSummaries []EpisodeSummaryRecord
	userSettings     map[string]contracts.UserSettings // keyed by tenantID
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
		unclaimedChats:      make(map[string]UnclaimedChatRecord),
		pendingInputs:       make(map[string]contracts.PendingInput),
		contactProfiles:     make(map[string]contracts.ContactProfile),
		messageCounts:       make(map[string]int),
		activeEpisodes:      make(map[string]ActiveEpisodeRecord),
		userSettings:        make(map[string]contracts.UserSettings),
	}
}

// NewPostgresRepository creates a repository backed by Postgres plus an in-memory cache.
// Connection pool is sized for moderate concurrent load: up to 25 open connections,
// 5 idle connections kept alive, with a 1-hour max lifetime to handle DB-side timeouts.
func NewPostgresRepository(databaseURL string, migrationPaths ...string) (*Repository, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)
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
		traceID := tracepkg.DeriveTraceID(event.EventID)
		for _, state := range record.States {
			r.insertState(event.TenantID, event.ChatID, event.SourceMessageID, state, traceID)
		}
		// Raw messages are NOT stored in memory tables — they live in `messages` only (Working Layer).
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
	// Production callers in HandleTelegramWebhook now run SaveMessage before any
	// AppendState, so this fallback only fires for no_memory chats (intentional —
	// the messages row is never persisted) and for the synthetic event constructed
	// on normalization failure (no usable tenant/chat/source_message_id triple).
	// In both cases skipping the DB write avoids an FK violation against messages.
	//
	// trace_id is derived from the stored event_id rather than threaded through
	// the call site: every webhook produces the same UUIDv5-derived trace_id so
	// state/audit rows for one event automatically share a trace.
	if r.db != nil && record.Event.EventID != "" {
		r.insertState(tenantID, chatID, sourceMessageID, state, tracepkg.DeriveTraceID(record.Event.EventID))
	}
}

// AppendAudit stores an audit event.
//
// When event.TraceID is empty but the parent message_states rows already carry
// a derived trace_id, we fill it from the stored event_id so audit and state
// rows for the same webhook always share a trace.
func (r *Repository) AppendAudit(event contracts.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// See AppendState — production callers persist the message row before any
	// audit write.  The skip below only triggers for no_memory chats (intentional)
	// and the synthetic normalization-error audit (no parent message row exists).
	msgRecord := r.messages[messageKey(event.TenantID, event.ChatID, event.SourceMessageID)]
	if event.TraceID == "" && msgRecord.Event.EventID != "" {
		event.TraceID = tracepkg.DeriveTraceID(msgRecord.Event.EventID)
	}
	r.audits = append(r.audits, event)
	if r.db == nil {
		return
	}
	if msgRecord.Event.EventID == "" {
		return
	}
	var traceArg any = nil
	if event.TraceID != "" {
		traceArg = event.TraceID
	}
	_, _ = r.db.Exec(
		`INSERT INTO audit_events (tenant_id, chat_id, source_message_id, decision, reason, trace_id)
		 VALUES ($1, $2, $3, $4::decision, $5, $6)
		 ON CONFLICT (tenant_id, chat_id, source_message_id, decision, reason) DO NOTHING`,
		event.TenantID,
		event.ChatID,
		event.SourceMessageID,
		string(event.Decision),
		event.Reason,
		traceArg,
	)
}

// Audits returns a copy of all audit events.
func (r *Repository) Audits() []contracts.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.AuditEvent(nil), r.audits...)
}

// SaveDraft stores an owner-facing draft in memory and, when a DB is configured,
// persists it to the drafts table so owner_message_id survives restarts.
func (r *Repository) SaveDraft(draft DraftRecord) {
	r.mu.Lock()
	r.drafts[draft.DraftID] = draft
	r.draftsByMsg[messageKey(draft.TenantID, draft.ChatID, draft.SourceMessageID)] = draft.DraftID
	r.mu.Unlock()
	if r.db != nil {
		var traceArg any = nil
		if draft.TraceID != "" {
			traceArg = draft.TraceID
		}
		_, _ = r.db.Exec(
			`INSERT INTO drafts (
			   draft_id, decision_id, tenant_id, chat_id, business_connection_id,
			   source_message_id, source_message_text, response_text,
			   draft_type, suggested_options, status,
			   owner_chat_id, owner_message_id, sent_message_id, action_history,
			   trace_id, created_at, updated_at
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$17)
			 ON CONFLICT (draft_id) DO NOTHING`,
			draft.DraftID, draft.DecisionID, draft.TenantID, draft.ChatID,
			draft.BusinessConnectionID, draft.SourceMessageID, draft.SourceMessageText,
			draft.ResponseText, valueOrDefault(draft.DraftType, "reply"),
			pq.Array(nilToEmpty(draft.SuggestedOptions)), string(valueOrDefault(string(draft.Status), "pending")),
			draft.OwnerChatID, draft.OwnerMessageID, draft.SentMessageID,
			pq.Array(nilToEmpty(draft.ActionHistory)), traceArg, draft.CreatedAt,
		)
	}
}

// UpdateDraft persists changes to an existing draft (status, owner message IDs, etc.).
// Callers are responsible for setting UpdatedAt before calling.
func (r *Repository) UpdateDraft(draft DraftRecord) {
	r.mu.Lock()
	r.drafts[draft.DraftID] = draft
	r.mu.Unlock()
	if r.db != nil {
		_, _ = r.db.Exec(
			`UPDATE drafts SET
			   response_text    = $2,
			   draft_type       = $3,
			   suggested_options= $4,
			   status           = $5,
			   owner_chat_id    = $6,
			   owner_message_id = $7,
			   sent_message_id  = $8,
			   action_history   = $9,
			   updated_at       = now()
			 WHERE draft_id = $1`,
			draft.DraftID, draft.ResponseText,
			valueOrDefault(draft.DraftType, "reply"),
			pq.Array(nilToEmpty(draft.SuggestedOptions)), string(valueOrDefault(string(draft.Status), "pending")),
			draft.OwnerChatID, draft.OwnerMessageID, draft.SentMessageID,
			pq.Array(nilToEmpty(draft.ActionHistory)),
		)
	}
}

// Draft returns a draft by ID. Falls back to the DB when not in memory (e.g. after restart).
func (r *Repository) Draft(draftID string) (DraftRecord, bool) {
	r.mu.Lock()
	draft, ok := r.drafts[draftID]
	r.mu.Unlock()
	if ok {
		return draft, true
	}
	if r.db != nil {
		d, dbOK := r.dbDraft(draftID)
		if dbOK {
			r.mu.Lock()
			r.drafts[d.DraftID] = d
			r.mu.Unlock()
		}
		return d, dbOK
	}
	return DraftRecord{}, false
}

// dbDraft fetches a single draft row from the database.
func (r *Repository) dbDraft(draftID string) (DraftRecord, bool) {
	var d DraftRecord
	var status string
	var draftType string
	var opts pq.StringArray
	var history pq.StringArray
	err := r.db.QueryRow(
		`SELECT draft_id, COALESCE(decision_id,''), tenant_id, chat_id,
		        COALESCE(business_connection_id,''), COALESCE(source_message_id,''),
		        COALESCE(source_message_text,''), COALESCE(response_text,''),
		        draft_type, suggested_options, status,
		        COALESCE(owner_chat_id,''), COALESCE(owner_message_id,''),
		        COALESCE(sent_message_id,''), action_history,
		        COALESCE(trace_id,''), created_at, updated_at
		   FROM drafts WHERE draft_id = $1`,
		draftID,
	).Scan(
		&d.DraftID, &d.DecisionID, &d.TenantID, &d.ChatID,
		&d.BusinessConnectionID, &d.SourceMessageID,
		&d.SourceMessageText, &d.ResponseText,
		&draftType, &opts, &status,
		&d.OwnerChatID, &d.OwnerMessageID,
		&d.SentMessageID, &history, &d.TraceID, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return DraftRecord{}, false
	}
	d.DraftType = draftType
	d.Status = DraftStatus(status)
	d.SuggestedOptions = []string(opts)
	d.ActionHistory = []string(history)
	return d, true
}

// DraftByMessage returns the latest draft for a message.
// Falls back to the DB when the draft is not in memory (e.g. after restart).
func (r *Repository) DraftByMessage(tenantID, chatID, sourceMessageID string) (DraftRecord, bool) {
	r.mu.Lock()
	draftID, ok := r.draftsByMsg[messageKey(tenantID, chatID, sourceMessageID)]
	r.mu.Unlock()
	if ok {
		return r.Draft(draftID)
	}
	if r.db != nil {
		var id string
		err := r.db.QueryRow(
			`SELECT draft_id FROM drafts
			  WHERE tenant_id = $1 AND chat_id = $2 AND source_message_id = $3
			  ORDER BY created_at DESC LIMIT 1`,
			tenantID, chatID, sourceMessageID,
		).Scan(&id)
		if err == nil {
			return r.Draft(id)
		}
	}
	return DraftRecord{}, false
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
// Status defaults to "managed" when not set; status never downgrades from "managed" on conflict.
func (r *Repository) SaveChat(record ChatRecord) {
	if record.Status == "" {
		record.Status = "managed"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chats[chatKey(record.TenantID, record.ChatID)] = record
	if r.db != nil {
		r.ensureTenant(record.TenantID, "")
		_, _ = r.db.Exec(
			`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, chat_type, display_name, status)
			 VALUES ($1, $2, $3, $4, $5, $6)
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

func (r *Repository) insertState(tenantID, chatID, sourceMessageID string, state contracts.MessageState, traceID string) {
	var traceArg any = nil
	if traceID != "" {
		traceArg = traceID
	}
	_, _ = r.db.Exec(
		`INSERT INTO message_states (tenant_id, chat_id, source_message_id, state, trace_id)
		 VALUES ($1, $2, $3, $4::message_state, $5)
		 ON CONFLICT (tenant_id, chat_id, source_message_id, state, actor) DO NOTHING`,
		tenantID,
		chatID,
		sourceMessageID,
		string(state),
		traceArg,
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
		        c.updated_at
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
		) == nil {
			chats = append(chats, chat)
		}
	}
	return chats
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

// ============================================================
// Contact Profile (Per-Chat Identity Layer)
// ============================================================

// SaveContactProfile stores the owner-supplied contact profile for a chat.
// It maps both the legacy fields (Name, Notes) and the new architecture fields
// (DisplayNames, StyleNotes, ChatContext, Tone, EmojiUsage, AddressForm, …) to
// the contact_profiles table.
func (r *Repository) SaveContactProfile(profile contracts.ContactProfile) {
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

// ContextScoreForChat computes the 0–100 richness score for a chat.
// It now uses SemanticFacts instead of the deprecated MemoryFacts.
func (r *Repository) ContextScoreForChat(tenantID, chatID string) int {
	profile, _ := r.ContactProfile(tenantID, chatID)
	facts := r.SemanticFacts(tenantID, chatID, 20)
	feedback := r.RecentFeedback(tenantID, chatID, 10)
	return contracts.ContextScore(profile, facts, feedback)
}

// MessagesByDateRange returns all messages for a chat in the half-open interval [from, to).
func (r *Repository) MessagesByDateRange(tenantID, chatID string, from, to time.Time) []MessageRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at
			   FROM messages
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND received_at >= $3 AND received_at < $4
			  ORDER BY received_at ASC`,
			tenantID, chatID, from, to,
		)
		if err == nil {
			defer rows.Close()
			messages := make([]MessageRecord, 0)
			for rows.Next() {
				var event contracts.NormalizedTelegramEvent
				if rows.Scan(
					&event.EventID, &event.TenantID, &event.ChatID,
					&event.TelegramUpdateID, &event.BusinessConnectionID,
					&event.SourceMessageID, &event.SenderID,
					&event.MessageText, &event.MessageType, &event.CreatedAt,
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
		e := record.Event
		if e.TenantID == tenantID && e.ChatID == chatID && e.EventID != "" &&
			!e.CreatedAt.Before(from) && e.CreatedAt.Before(to) {
			messages = append(messages, record)
		}
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Event.CreatedAt.Before(messages[j].Event.CreatedAt)
	})
	return messages
}

// AllActiveTenantIDs returns all non-staging tenant IDs that have at least one chat.
func (r *Repository) AllActiveTenantIDs() []string {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT DISTINCT tenant_id FROM chats WHERE tenant_id NOT LIKE 'staging:%'`,
		)
		if err == nil {
			defer rows.Close()
			result := make([]string, 0)
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					result = append(result, id)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, chat := range r.chats {
		if !strings.HasPrefix(chat.TenantID, "staging:") {
			if _, ok := seen[chat.TenantID]; !ok {
				seen[chat.TenantID] = struct{}{}
				result = append(result, chat.TenantID)
			}
		}
	}
	return result
}

// AvailableModesForChat returns the modes the owner is allowed to select for this chat.
func (r *Repository) AvailableModesForChat(tenantID, chatID string) []contracts.ChatMode {
	return contracts.UnlockedModesForScore(r.ContextScoreForChat(tenantID, chatID))
}

// ============================================================
// Semantic Layer (semantic_facts table)
// ============================================================

// SaveSemanticFact inserts a typed semantic fact.
// On exact-duplicate (same normalised hash), the fact is silently ignored.
// Returns the fact ID (may be 0 in memory-only mode).
func (r *Repository) SaveSemanticFact(fact SemanticFactRecord) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.semanticFacts = append(r.semanticFacts, fact)
	if r.db == nil {
		return 0
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO semantic_facts (
		   tenant_id, scope_kind, chat_id, fact_text, normalized_fact_hash,
		   fact_type, subject, confidence, source, source_message_id, expires_at
		 ) VALUES (
		   $1, $2, NULLIF($3,''), $4,
		   normalize_fact_text($4),
		   $5, $6, $7, $8, NULLIF($9,''), $10
		 )
		 ON CONFLICT (tenant_id, scope_kind, COALESCE(chat_id,''), fact_type, subject, normalized_fact_hash)
		   WHERE deleted_at IS NULL AND superseded_by_id IS NULL
		 DO NOTHING
		 RETURNING id`,
		fact.TenantID, fact.ScopeKind, fact.ChatID, fact.FactText,
		fact.FactType, fact.Subject, fact.Confidence, fact.Source,
		fact.SourceMessageID, nullableTime(fact.ExpiresAt),
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// UpsertSemanticFact deduplicates using the upsert_semantic_fact stored proc when a DB
// is configured.  The proc applies three-tier dedup logic (exact hash → high cosine → supersede)
// with a pg_advisory_xact_lock to prevent races.  In-memory mode falls back to SaveSemanticFact
// with linear dedup on normalized text.
func (r *Repository) UpsertSemanticFact(fact SemanticFactRecord) SemanticFactUpsertResult {
	if r.db != nil {
		var id int64
		err := r.db.QueryRow(
			`SELECT upsert_semantic_fact($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			fact.TenantID,
			fact.ScopeKind,
			nullableString(fact.ChatID),
			fact.FactText,
			fact.FactType,
			fact.Subject,
			fact.Confidence,
			fact.Source,
			fact.SourceMessageID,
			nullableTime(fact.ExpiresAt),
		).Scan(&id)
		if err != nil {
			// Proc failed (e.g. constraint violation for blocked health facts) — do not persist.
			return SemanticFactUpsertResult{Action: "rejected"}
		}
		r.mu.Lock()
		r.semanticFacts = append(r.semanticFacts, fact)
		r.mu.Unlock()
		if id == 0 {
			return SemanticFactUpsertResult{Action: "occurrence_added"}
		}
		return SemanticFactUpsertResult{FactID: id, Action: "inserted"}
	}
	id := r.SaveSemanticFact(fact)
	if id == 0 {
		return SemanticFactUpsertResult{Action: "occurrence_added"}
	}
	return SemanticFactUpsertResult{FactID: id, Action: "inserted"}
}

// SavePendingHealthFact queues a health fact for owner confirmation before it is promoted
// to semantic_facts.  When a DB is configured it writes to pending_global_health_facts.
// The in-memory store does not track pending health facts (they are ephemeral between restarts
// and require the confirmation UI which is DB-backed).
func (r *Repository) SavePendingHealthFact(fact PendingHealthFactRecord) {
	if fact.Status == "" {
		fact.Status = "pending"
	}
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`INSERT INTO pending_global_health_facts
		   (tenant_id, chat_id, source_message_id, fact_text, confidence, status)
		 VALUES ($1, $2, $3, $4, $5, 'pending')`,
		fact.TenantID, fact.ChatID, fact.SourceMessageID, fact.FactText, fact.Confidence,
	)
}

// SemanticFacts returns active (non-deleted, non-superseded) chat-scoped semantic facts.
func (r *Repository) SemanticFacts(tenantID, chatID string, limit int) []SemanticFactRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
			        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
			   FROM semantic_facts
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND scope_kind = 'chat'
			    AND deleted_at IS NULL AND superseded_by_id IS NULL
			  ORDER BY created_at DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			return scanSemanticFacts(rows)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]SemanticFactRecord, 0)
	for i := len(r.semanticFacts) - 1; i >= 0 && len(result) < limit; i-- {
		f := r.semanticFacts[i]
		if f.TenantID == tenantID && f.ChatID == chatID && f.ScopeKind == "chat" {
			result = append(result, f)
		}
	}
	return result
}

// GlobalSemanticFacts returns active owner-global semantic facts visible across all chats.
func (r *Repository) GlobalSemanticFacts(tenantID string, limit int) []SemanticFactRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
			        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
			   FROM semantic_facts
			  WHERE tenant_id = $1
			    AND scope_kind = 'owner_global'
			    AND deleted_at IS NULL AND superseded_by_id IS NULL
			  ORDER BY created_at DESC
			  LIMIT $2`,
			tenantID, limit,
		)
		if err == nil {
			defer rows.Close()
			return scanSemanticFacts(rows)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]SemanticFactRecord, 0)
	for i := len(r.semanticFacts) - 1; i >= 0 && len(result) < limit; i-- {
		f := r.semanticFacts[i]
		if f.TenantID == tenantID && f.ScopeKind == "owner_global" {
			result = append(result, f)
		}
	}
	return result
}

func scanSemanticFacts(rows *sql.Rows) []SemanticFactRecord {
	result := make([]SemanticFactRecord, 0)
	for rows.Next() {
		var f SemanticFactRecord
		var expiresAt sql.NullTime
		if rows.Scan(
			&f.ID, &f.TenantID, &f.ScopeKind, &f.ChatID, &f.FactText,
			&f.FactType, &f.Subject, &f.Confidence, &f.Source,
			&f.SourceMessageID, &expiresAt, &f.CreatedAt,
		) == nil {
			if expiresAt.Valid {
				f.ExpiresAt = &expiresAt.Time
			}
			result = append(result, f)
		}
	}
	return result
}

// ============================================================
// Runtime State Layer (active_episodes table)
// ============================================================

// UpsertActiveEpisode creates or updates the active episode for a chat.
// On first message: inserts a new episode. On subsequent messages: updates last_message_at
// and message_count. The episode is closed (deleted) externally when silence > 6h or
// message_count > 40.
func (r *Repository) UpsertActiveEpisode(episode ActiveEpisodeRecord) {
	r.mu.Lock()
	r.activeEpisodes[chatKey(episode.TenantID, episode.ChatID)] = episode
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`INSERT INTO active_episodes (
		   tenant_id, chat_id, started_at, last_message_at, message_count,
		   current_topic, open_question_message_id, pending_owner_action, updated_at
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
		   last_message_at         = EXCLUDED.last_message_at,
		   message_count           = active_episodes.message_count + 1,
		   current_topic           = COALESCE(EXCLUDED.current_topic, active_episodes.current_topic),
		   open_question_message_id = COALESCE(EXCLUDED.open_question_message_id, active_episodes.open_question_message_id),
		   pending_owner_action    = COALESCE(EXCLUDED.pending_owner_action, active_episodes.pending_owner_action),
		   updated_at              = now()`,
		episode.TenantID, episode.ChatID,
		episode.StartedAt, episode.LastMessageAt, episode.MessageCount,
		nullableString(episode.CurrentTopic),
		nullableString(episode.OpenQuestionMsgID),
		nullableString(episode.PendingOwnerAction),
	)
}

// ActiveEpisodeByChat returns the current open episode for a chat, if one exists.
func (r *Repository) ActiveEpisodeByChat(tenantID, chatID string) (ActiveEpisodeRecord, bool) {
	if r.db != nil {
		var ep ActiveEpisodeRecord
		var currentTopic sql.NullString
		var openQuestion sql.NullString
		var pendingAction sql.NullString
		err := r.db.QueryRow(
			`SELECT tenant_id, chat_id, started_at, last_message_at, message_count,
			        current_topic, open_question_message_id, pending_owner_action
			   FROM active_episodes
			  WHERE tenant_id = $1 AND chat_id = $2`,
			tenantID, chatID,
		).Scan(
			&ep.TenantID, &ep.ChatID,
			&ep.StartedAt, &ep.LastMessageAt, &ep.MessageCount,
			&currentTopic, &openQuestion, &pendingAction,
		)
		if err == nil {
			ep.CurrentTopic = currentTopic.String
			ep.OpenQuestionMsgID = openQuestion.String
			ep.PendingOwnerAction = pendingAction.String
			return ep, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.activeEpisodes[chatKey(tenantID, chatID)]
	return ep, ok
}

// CloseActiveEpisode deletes the active episode record, signalling that it has been
// materialised into episode_summaries.
func (r *Repository) CloseActiveEpisode(tenantID, chatID string) {
	r.mu.Lock()
	delete(r.activeEpisodes, chatKey(tenantID, chatID))
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`DELETE FROM active_episodes WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
}

// LogExtraction records one async fact-extraction attempt to the extraction_log table.
// Best-effort: failures are swallowed because logging must never break the extraction
// path. No-op when running without a DB (unit tests use the in-memory repository).
func (r *Repository) LogExtraction(rec ExtractionLogRecord) {
	if r.db == nil {
		return
	}
	rawOutput := rec.RawOutput
	if len(rawOutput) > 4000 {
		rawOutput = rawOutput[:4000]
	}
	_, _ = r.db.Exec(
		`INSERT INTO extraction_log (
		   tenant_id, chat_id, input_message_ids, model_used, raw_output,
		   facts_created, facts_skipped, facts_merged, error, duration_ms
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.TenantID, rec.ChatID,
		pq.Array(nilToEmpty(rec.InputMessageIDs)),
		rec.ModelUsed,
		// raw_output is JSONB; store the verbatim text as a JSON string value.
		rawOutputJSON(rawOutput),
		pq.Array(rec.FactsCreated),
		rec.FactsSkipped, rec.FactsMerged,
		nullableString(rec.Error),
		rec.DurationMS,
	)
}

// rawOutputJSON wraps arbitrary extractor text as a valid JSON value for the
// JSONB raw_output column. The LLM output is usually a JSON array already, but it
// can be an error string or malformed; encoding as a JSON string is always valid.
func rawOutputJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "\"\""
	}
	return string(b)
}

// ============================================================
// Episodic Layer (episode_summaries table)
// ============================================================

// SaveEpisodeSummary stores a compressed summary of a closed conversation episode.
func (r *Repository) SaveEpisodeSummary(summary EpisodeSummaryRecord) {
	r.mu.Lock()
	r.episodeSummaries = append(r.episodeSummaries, summary)
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, _ = r.db.Exec(
		`INSERT INTO episode_summaries (
		   tenant_id, chat_id, period_start, period_end, content,
		   message_count, closure_reason
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		summary.TenantID, summary.ChatID,
		summary.PeriodStart, summary.PeriodEnd,
		summary.Content, summary.MessageCount,
		valueOrDefault(summary.ClosureReason, "manual"),
	)
}

// EpisodeSummaries returns recent episode summaries for a chat, newest first.
func (r *Repository) EpisodeSummaries(tenantID, chatID string, limit int) []EpisodeSummaryRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, chat_id, period_start, period_end,
			        content, closure_reason, message_count, created_at
			   FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND deleted_at IS NULL
			  ORDER BY period_end DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			result := make([]EpisodeSummaryRecord, 0)
			for rows.Next() {
				var s EpisodeSummaryRecord
				if rows.Scan(
					&s.ID, &s.TenantID, &s.ChatID,
					&s.PeriodStart, &s.PeriodEnd,
					&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
				) == nil {
					result = append(result, s)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]EpisodeSummaryRecord, 0)
	for i := len(r.episodeSummaries) - 1; i >= 0 && len(result) < limit; i-- {
		s := r.episodeSummaries[i]
		if s.TenantID == tenantID && s.ChatID == chatID {
			result = append(result, s)
		}
	}
	return result
}

// float64SliceToVector converts a Go float64 slice to a pgvector literal string.
// Format: "[0.1, 0.2, ...]" — distinct from the PostgreSQL array literal "{0.1, ...}".
// Used to pass query vectors to ORDER BY embedding <=> $n::vector clauses.
func float64SliceToVector(v []float64) string {
	if len(v) == 0 {
		return "[]"
	}
	b := make([]byte, 0, len(v)*12+2)
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, f, 'f', -1, 64)
	}
	b = append(b, ']')
	return string(b)
}

// hardIncludeFactsByScope returns taboo and health facts for a given scope unconditionally.
// Design §8.2: taboo and health facts must always appear in context regardless of similarity.
func (r *Repository) hardIncludeFactsByScope(tenantID, chatID, scopeKind string) []SemanticFactRecord {
	if r.db == nil {
		return nil
	}
	var rows *sql.Rows
	var err error
	if scopeKind == "chat" {
		rows, err = r.db.Query(
			`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
			        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
			   FROM semantic_facts
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND scope_kind = 'chat'
			    AND fact_type IN ('taboo', 'health')
			    AND deleted_at IS NULL AND superseded_by_id IS NULL
			  ORDER BY created_at DESC`,
			tenantID, chatID,
		)
	} else {
		rows, err = r.db.Query(
			`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
			        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
			   FROM semantic_facts
			  WHERE tenant_id = $1
			    AND scope_kind = 'owner_global'
			    AND fact_type IN ('taboo', 'health')
			    AND deleted_at IS NULL AND superseded_by_id IS NULL
			  ORDER BY created_at DESC`,
			tenantID,
		)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanSemanticFacts(rows)
}

// SemanticFactsByVector returns chat-scoped facts ranked by cosine similarity to queryVector.
// Taboo and health facts are always included regardless of similarity (design §8.2).
// The remaining slots are filled by top-K similarity-ranked facts with commitment (÷1.5) and
// preference (÷1.2) effective-distance boosts. Falls back to SemanticFacts when queryVector
// is empty or the fact_embeddings join yields no rows (pgvector unavailable).
func (r *Repository) SemanticFactsByVector(tenantID, chatID string, queryVector []float64, limit int) []SemanticFactRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.SemanticFacts(tenantID, chatID, limit)
	}

	// Hard-include: taboo + health always in context (design §8.2).
	hardFacts := r.hardIncludeFactsByScope(tenantID, chatID, "chat")

	// Top-K by cosine similarity, excluding hard-include types (they are already in).
	remaining := limit - len(hardFacts)
	if remaining < 1 {
		remaining = 1
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(
		`SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,''), sf.fact_text, sf.fact_type,
		        sf.subject, sf.confidence, sf.source, COALESCE(sf.source_message_id,''), sf.expires_at, sf.created_at
		   FROM semantic_facts sf
		   JOIN fact_embeddings fe ON fe.fact_id = sf.id
		  WHERE sf.tenant_id = $1 AND sf.chat_id = $2
		    AND sf.scope_kind = 'chat'
		    AND sf.fact_type NOT IN ('taboo', 'health')
		    AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		  ORDER BY (fe.embedding <=> $3::vector) *
		           CASE sf.fact_type
		             WHEN 'commitment' THEN 0.667
		             WHEN 'preference' THEN 0.833
		             ELSE 1.0 END ASC
		  LIMIT $4`,
		tenantID, chatID, vec, remaining,
	)
	if err != nil {
		// pgvector unavailable — fall back to recency-ordered query.
		return r.SemanticFacts(tenantID, chatID, limit)
	}
	defer rows.Close()
	simFacts := scanSemanticFacts(rows)
	return append(hardFacts, simFacts...)
}

// GlobalSemanticFactsByVector returns owner-global facts ranked by cosine similarity to
// queryVector. Taboo and health global facts are always included (design §8.2).
// Falls back to GlobalSemanticFacts when queryVector is empty or pgvector is unavailable.
func (r *Repository) GlobalSemanticFactsByVector(tenantID string, queryVector []float64, limit int) []SemanticFactRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.GlobalSemanticFacts(tenantID, limit)
	}

	hardFacts := r.hardIncludeFactsByScope(tenantID, "", "owner_global")

	remaining := limit - len(hardFacts)
	if remaining < 1 {
		remaining = 1
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(
		`SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,''), sf.fact_text, sf.fact_type,
		        sf.subject, sf.confidence, sf.source, COALESCE(sf.source_message_id,''), sf.expires_at, sf.created_at
		   FROM semantic_facts sf
		   JOIN fact_embeddings fe ON fe.fact_id = sf.id
		  WHERE sf.tenant_id = $1
		    AND sf.scope_kind = 'owner_global'
		    AND sf.fact_type NOT IN ('taboo', 'health')
		    AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		  ORDER BY (fe.embedding <=> $2::vector) *
		           CASE sf.fact_type
		             WHEN 'commitment' THEN 0.667
		             WHEN 'preference' THEN 0.833
		             ELSE 1.0 END ASC
		  LIMIT $3`,
		tenantID, vec, remaining,
	)
	if err != nil {
		return r.GlobalSemanticFacts(tenantID, limit)
	}
	defer rows.Close()
	simFacts := scanSemanticFacts(rows)
	return append(hardFacts, simFacts...)
}

// EpisodeSummariesByVector returns episode summaries ranked by cosine similarity to
// queryVector. Falls back to EpisodeSummaries (recency) when queryVector is empty or
// the embedding column has no data (pgvector unavailable or summaries not yet embedded).
func (r *Repository) EpisodeSummariesByVector(tenantID, chatID string, queryVector []float64, limit int) []EpisodeSummaryRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at
		   FROM episode_summaries
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND embedding IS NOT NULL
		    AND deleted_at IS NULL
		  ORDER BY embedding <=> $3::vector ASC
		  LIMIT $4`,
		tenantID, chatID, vec, limit,
	)
	if err != nil {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	defer rows.Close()
	result := make([]EpisodeSummaryRecord, 0, limit)
	for rows.Next() {
		var s EpisodeSummaryRecord
		if rows.Scan(
			&s.ID, &s.TenantID, &s.ChatID,
			&s.PeriodStart, &s.PeriodEnd,
			&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
		) == nil {
			result = append(result, s)
		}
	}
	// If no embedded summaries found, fall back to recency.
	if len(result) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	return result
}

// EpisodeSummaryExistsForDate reports whether an episode summary covering the given calendar
// date already exists. Used by the daily scheduler to avoid duplicate runs.
func (r *Repository) EpisodeSummaryExistsForDate(tenantID, chatID string, date time.Time) bool {
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := startOfDay.Add(24 * time.Hour)
	if r.db != nil {
		var exists bool
		_ = r.db.QueryRow(
			`SELECT EXISTS(
			   SELECT 1 FROM episode_summaries
			    WHERE tenant_id = $1 AND chat_id = $2
			      AND period_start >= $3 AND period_end <= $4
			      AND deleted_at IS NULL
			 )`,
			tenantID, chatID, startOfDay, endOfDay,
		).Scan(&exists)
		return exists
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			!s.PeriodStart.Before(startOfDay) && !s.PeriodEnd.After(endOfDay) {
			return true
		}
	}
	return false
}

// ============================================================
// User Settings (User Layer)
// ============================================================

// UserSettings returns the owner settings for a tenant, if they have been configured.
func (r *Repository) UserSettings(tenantID string) (contracts.UserSettings, bool) {
	if r.db != nil {
		var s contracts.UserSettings
		var secLangs pq.StringArray
		err := r.db.QueryRow(
			`SELECT us.user_id, us.tenant_id,
			        COALESCE(us.display_name,''), COALESCE(us.bio,''), COALESCE(us.timezone,''),
			        us.primary_language, us.secondary_languages,
			        us.default_mode_for_new_chats, us.learning_enabled, us.language_for_drafts
			   FROM user_settings us
			  WHERE us.tenant_id = $1
			  LIMIT 1`,
			tenantID,
		).Scan(
			&s.UserID, &s.TenantID,
			&s.DisplayName, &s.Bio, &s.Timezone,
			&s.PrimaryLanguage, &secLangs,
			&s.DefaultModeForNewChats, &s.LearningEnabled, &s.LanguageForDrafts,
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
		   default_mode_for_new_chats, learning_enabled, language_for_drafts
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (user_id) DO UPDATE SET
		   display_name            = EXCLUDED.display_name,
		   bio                     = EXCLUDED.bio,
		   timezone                = EXCLUDED.timezone,
		   primary_language        = EXCLUDED.primary_language,
		   secondary_languages     = EXCLUDED.secondary_languages,
		   default_mode_for_new_chats = EXCLUDED.default_mode_for_new_chats,
		   learning_enabled        = EXCLUDED.learning_enabled,
		   language_for_drafts     = EXCLUDED.language_for_drafts,
		   updated_at              = now()`,
		settings.UserID, settings.TenantID,
		settings.DisplayName, settings.Bio, settings.Timezone,
		settings.PrimaryLanguage, pq.Array(nilToEmpty(settings.SecondaryLanguages)),
		settings.DefaultModeForNewChats, settings.LearningEnabled, settings.LanguageForDrafts,
	)
}

// ============================================================
// Helpers
// ============================================================

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

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// nilToEmpty normalises a nil string slice to an empty (non-nil) slice so that
// pq.Array serialises it as `'{}'::text[]` instead of SQL NULL.  Required for
// columns declared `TEXT[] NOT NULL DEFAULT '{}'`, because `pq.Array(nil)` emits
// NULL and overrides the column DEFAULT, causing NOT NULL constraint failures.
func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// FactsWithoutEmbeddings returns up to limit semantic fact IDs + texts that have no embedding yet.
// Used by the embedding worker to find facts to batch-embed.
func (r *Repository) FactsWithoutEmbeddings(limit int) []SemanticFactRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,''), sf.fact_text,
		        sf.fact_type, sf.subject, sf.confidence, sf.source,
		        COALESCE(sf.source_message_id,''), sf.created_at
		   FROM semantic_facts sf
		  WHERE NOT EXISTS (SELECT 1 FROM fact_embeddings fe WHERE fe.fact_id = sf.id)
		    AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		  ORDER BY sf.created_at ASC
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]SemanticFactRecord, 0)
	for rows.Next() {
		var f SemanticFactRecord
		if rows.Scan(
			&f.ID, &f.TenantID, &f.ScopeKind, &f.ChatID, &f.FactText,
			&f.FactType, &f.Subject, &f.Confidence, &f.Source,
			&f.SourceMessageID, &f.CreatedAt,
		) == nil {
			result = append(result, f)
		}
	}
	return result
}

// SaveFactEmbedding persists an embedding vector for a semantic fact.
func (r *Repository) SaveFactEmbedding(factID int64, tenantID, scopeKind, chatID string, embedding []float64, model string) {
	if r.db == nil {
		return
	}
	// Encode as PostgreSQL array literal for compatibility with both FLOAT8[] and vector columns.
	_, _ = r.db.Exec(
		`INSERT INTO fact_embeddings (fact_id, tenant_id, scope_kind, chat_id, embedding_model, embedding)
		 VALUES ($1, $2, $3, $4, $5, $6::float8[])
		 ON CONFLICT (fact_id) DO UPDATE SET
		   embedding       = EXCLUDED.embedding,
		   embedding_model = EXCLUDED.embedding_model`,
		factID, tenantID, scopeKind, nullableString(chatID), model, float64SliceToArray(embedding),
	)
}

// float64SliceToArray converts a Go float64 slice to a PostgreSQL array literal string.
func float64SliceToArray(v []float64) string {
	if len(v) == 0 {
		return "{}"
	}
	b := make([]byte, 0, len(v)*12+2)
	b = append(b, '{')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, f, 'f', -1, 64)
	}
	b = append(b, '}')
	return string(b)
}

// GreetedToday returns true when the owner has sent a greeting in this chat today.
// When a DB is configured it calls the view_chat_greeted_today SQL function which
// uses the messages table as the authoritative source — consistent with query results.
// The in-memory fallback scans today's messages for greeting words.
func (r *Repository) GreetedToday(tenantID, chatID, ownerID string) bool {
	if r.db != nil {
		var result bool
		err := r.db.QueryRow(
			`SELECT view_chat_greeted_today($1, $2, $3)`,
			tenantID, chatID, ownerID,
		).Scan(&result)
		if err == nil {
			return result
		}
	}
	// In-memory fallback: scan today's messages for owner greeting words.
	if ownerID == "" {
		return false
	}
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	greetings := []string{"привет", "здравствуй", "добрый", "hi ", "hello", "hey ", "хай"}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.messages {
		if rec.Event.TenantID != tenantID || rec.Event.ChatID != chatID {
			continue
		}
		if rec.Event.SenderID != ownerID {
			continue
		}
		if rec.Event.CreatedAt.UTC().Before(todayStart) {
			continue
		}
		lower := strings.ToLower(rec.Event.MessageText)
		for _, g := range greetings {
			if strings.Contains(lower, g) {
				return true
			}
		}
	}
	return false
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

// OpenCommitments returns active commitment facts for a chat (non-expired, non-deleted).
// Uses the view_open_commitments SQL function when a DB is configured.
func (r *Repository) OpenCommitments(tenantID, chatID string) []SemanticFactRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT fact_id, fact_text, expires_at FROM view_open_commitments($1, $2)`,
			tenantID, chatID,
		)
		if err == nil {
			defer rows.Close()
			result := make([]SemanticFactRecord, 0)
			for rows.Next() {
				var f SemanticFactRecord
				var expiresAt *time.Time
				if rows.Scan(&f.ID, &f.FactText, &expiresAt) == nil {
					f.TenantID = tenantID
					f.ChatID = chatID
					f.FactType = "commitment"
					f.ExpiresAt = expiresAt
					result = append(result, f)
				}
			}
			return result
		}
	}
	// In-memory fallback: filter semantic facts for non-expired commitments.
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	result := make([]SemanticFactRecord, 0)
	for _, f := range r.semanticFacts {
		if f.TenantID != tenantID || f.ChatID != chatID || f.FactType != "commitment" {
			continue
		}
		if f.ExpiresAt != nil && f.ExpiresAt.Before(now) {
			continue
		}
		result = append(result, f)
	}
	return result
}

func chatKey(tenantID, chatID string) string {
	return tenantID + ":" + chatID
}

func messageKey(tenantID, chatID, sourceMessageID string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, chatID, sourceMessageID)
}
