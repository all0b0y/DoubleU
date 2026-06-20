// Package store provides the repository used by the DoubleU backend.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
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
	TextFormat           contracts.TextFormat
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
	// ScopeKind is always "chat": memory is strictly per-chat (migration 011
	// retired the owner_global scope). The column survives for provenance of
	// soft-deleted legacy rows.
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
	// SourceMessageIDs are the messages compressed into this summary —
	// provenance per design §6 (MEM-6).
	SourceMessageIDs []string
	CreatedAt        time.Time
	Embedding        []float64 // populated by the embedding worker; nil until then
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

// SemanticFactUpsertResult describes the outcome of an UpsertSemanticFact call.
type SemanticFactUpsertResult struct {
	FactID int64
	// Action is one of "inserted", "occurrence_added", "occurrence_added_race",
	// "superseded", or "rejected" — mirrors the upsert_semantic_fact v3 proc.
	Action string
	// SupersededFactID is the retired older fact when Action == "superseded";
	// 0 otherwise. The caller records the fact_relations(supersedes) edge.
	SupersededFactID int64
}

// FactRelationRecord represents a directed temporal edge between two semantic_facts rows.
// Maps to the fact_relations table (migration 009).
type FactRelationRecord struct {
	ID           int64
	TenantID     string
	FactIDFrom   int64  // the older / replaced fact
	FactIDTo     int64  // the newer / replacing fact
	RelationType string // supersedes | fulfills | contradicts | updates | related
	Confidence   float64
}

// AIRoleRecord is an owner-authored AI persona (роль).  Maps to the ai_roles
// table.  A role describes WHO the AI plays when answering on the owner's
// behalf in a chat ("менеджер Алексея", "техподдержка"); one role may be
// assigned to many chats via chat_settings.ai_role_id.
type AIRoleRecord struct {
	ID       int64
	TenantID string
	// Name is the short unique-per-tenant label the owner refers to the role by.
	Name string
	// Description is the owner's free-text persona description — the primary
	// prompt material.
	Description string
	// SpeakingStyle describes how the role talks (lexicon, tone, length).
	SpeakingStyle string
	// Goals describes what the role tries to achieve in conversations.
	Goals string
	// Constraints describes what the role must not do or discuss.
	Constraints string
	// Context is owner-supplied reference material (FAQ, pricing, docs —
	// pasted or extracted from files in the Mini App), injected into the
	// draft prompt. Maps to ai_roles.context_notes (migration 101).
	Context string
	// CustomFields are owner-defined extra role attributes (title + why +
	// optional context). Maps to ai_roles.custom_fields jsonb (migration 102).
	CustomFields []contracts.AIRoleCustomField
	// Nil delivery toggles inherit owner-level defaults; non-nil values override them.
	RichFormattingEnabled *bool
	StreamingEnabled      *bool
	// SpeakAsOwner mirrors contracts.AIRole.SpeakAsOwner. true (default) = the
	// AI impersonates the owner; false = the AI responds as the role persona.
	SpeakAsOwner bool
}

// Repository stores state — in memory when no DB is configured, in Postgres otherwise.
type Repository struct {
	mu           sync.Mutex
	db           *sql.DB
	chatSettings map[string]contracts.ChatSettings
	receiveKeys  map[string]struct{}
	sendKeys     map[string]SendRecord
	callbackKeys map[string]struct{}
	messages     map[string]MessageRecord
	// chatMessageKeys indexes message keys per chat in insertion order so the
	// in-memory RecentMessages path avoids an O(all messages) scan (ARCH-13).
	chatMessageKeys map[string][]string
	// profileSuggestions is the in-memory fallback for contact_profile_suggestions (MEM-4).
	profileSuggestions  []ProfileSuggestionRecord
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
	// userBots and tenantModes back bot-transport ingestion in memory mode
	// (user_bots table / tenants.ingestion_mode column in Postgres mode).
	userBots    map[string]UserBotRecord
	tenantModes map[string]contracts.IngestionMode
	// New memory architecture fields
	semanticFacts    []SemanticFactRecord
	activeEpisodes   map[string]ActiveEpisodeRecord
	episodeSummaries []EpisodeSummaryRecord
	userSettings     map[string]contracts.UserSettings // keyed by tenantID
	// AI roles (in-memory mode): role records by ID plus per-chat assignment.
	aiRoles       map[int64]AIRoleRecord
	aiRoleSeq     int64
	chatAIRoleIDs map[string]int64 // chatKey -> ai_roles.id
	// ctxScoreCache memoises ContextScoreForChat with a short TTL (ARCH-8):
	// the score gates mode selection and was recomputed (3 queries) per check.
	// Writes that affect the score delete the entry so reads stay fresh.
	ctxScoreCache sync.Map // chatKey -> ctxScoreEntry
	// logger reports memory-layer write failures (MEM-10). Defaults to
	// slog.Default(); override via SetLogger.
	logger *slog.Logger
	// halfvecOK is true when pgvector ≥ 0.7 provides the halfvec type, so
	// vector queries can target the HNSW expression indexes (MEM-7).
	halfvecOK bool
}

// SetLogger overrides the repository logger (MEM-10 write-path observability).
func (r *Repository) SetLogger(logger *slog.Logger) {
	if logger != nil {
		r.logger = logger
	}
}

// logWriteError records a failed memory-layer write. Memory writes are
// fire-and-forget by design, but the two silent-failure bugs found in the
// 2026-06-11 audit (lost summaries, never-saved embeddings) lived undetected
// precisely because these errors were discarded (MEM-10).
func (r *Repository) logWriteError(op, tenantID, chatID string, err error) {
	if err == nil {
		return
	}
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	// TODO(observability): also increment memory_write_errors_total{op} once a
	// counters mechanism exists in the observability package.
	logger.Warn("memory write failed", "op", op, "tenant_id", tenantID, "chat_id", chatID, "error", err)
}

// ctxScoreEntry is one cached context-score value with its computation time.
type ctxScoreEntry struct {
	score int
	at    time.Time
}

// ctxScoreTTL bounds context-score staleness for reads not covered by
// write-path invalidation (e.g. fact decay).
const ctxScoreTTL = 30 * time.Second

// NewRepository creates an empty repository.
func NewRepository() *Repository {
	return &Repository{
		chatSettings:        make(map[string]contracts.ChatSettings),
		receiveKeys:         make(map[string]struct{}),
		sendKeys:            make(map[string]SendRecord),
		callbackKeys:        make(map[string]struct{}),
		messages:            make(map[string]MessageRecord),
		chatMessageKeys:     make(map[string][]string),
		drafts:              make(map[string]DraftRecord),
		draftsByMsg:         make(map[string]string),
		businessConnections: make(map[string]BusinessConnectionRecord),
		connectionByUser:    make(map[string]string),
		connectionTenants:   make(map[string]string),
		chats:               make(map[string]ChatRecord),
		unclaimedChats:      make(map[string]UnclaimedChatRecord),
		userBots:            make(map[string]UserBotRecord),
		tenantModes:         make(map[string]contracts.IngestionMode),
		pendingInputs:       make(map[string]contracts.PendingInput),
		contactProfiles:     make(map[string]contracts.ContactProfile),
		messageCounts:       make(map[string]int),
		activeEpisodes:      make(map[string]ActiveEpisodeRecord),
		userSettings:        make(map[string]contracts.UserSettings),
		aiRoles:             make(map[int64]AIRoleRecord),
		chatAIRoleIDs:       make(map[string]int64),
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
	// halfvec (pgvector ≥ 0.7) enables the HNSW expression indexes from
	// migration 015; queries emit halfvec casts only when the type exists.
	_ = db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'halfvec')`,
	).Scan(&repository.halfvecOK)
	return repository, nil
}

// vectorOrderExpr returns the cosine-distance ORDER BY expression for the
// given embedding column and query-vector placeholder. With halfvec available
// the expression matches the migration-015 HNSW indexes so the planner uses
// ANN; otherwise it stays on the plain vector operator (seq scan, MEM-7).
func (r *Repository) vectorOrderExpr(column, placeholder string) string {
	if r.halfvecOK {
		return fmt.Sprintf("(%s::halfvec(4096) <=> %s::halfvec(4096))", column, placeholder)
	}
	return fmt.Sprintf("(%s <=> %s::vector)", column, placeholder)
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
	record, existed := r.messages[key]
	record.Event = event
	r.messages[key] = record
	if !existed {
		// Per-chat insertion-order index so in-memory RecentMessages reads a
		// bounded slice instead of scanning every stored message (ARCH-13).
		ck := chatKey(event.TenantID, event.ChatID)
		r.chatMessageKeys[ck] = append(r.chatMessageKeys[ck], key)
	}
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
			   text_format, draft_type, suggested_options, status,
			   owner_chat_id, owner_message_id, sent_message_id, action_history,
			   trace_id, created_at, updated_at
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$18)
			 ON CONFLICT (draft_id) DO NOTHING`,
			draft.DraftID, draft.DecisionID, draft.TenantID, draft.ChatID,
			draft.BusinessConnectionID, draft.SourceMessageID, draft.SourceMessageText,
			draft.ResponseText, string(draft.TextFormat.Normalize()), valueOrDefault(draft.DraftType, "reply"),
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
			   text_format      = $3,
			   draft_type       = $4,
			   suggested_options= $5,
			   status           = $6,
			   owner_chat_id    = $7,
			   owner_message_id = $8,
			   sent_message_id  = $9,
			   action_history   = $10,
			   updated_at       = now()
			 WHERE draft_id = $1`,
			draft.DraftID, draft.ResponseText, string(draft.TextFormat.Normalize()),
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
	var textFormat string
	var opts pq.StringArray
	var history pq.StringArray
	err := r.db.QueryRow(
		`SELECT draft_id, COALESCE(decision_id,''), tenant_id, chat_id,
		        COALESCE(business_connection_id,''), COALESCE(source_message_id,''),
		        COALESCE(source_message_text,''), COALESCE(response_text,''),
		        COALESCE(text_format,'plain'), draft_type, suggested_options, status,
		        COALESCE(owner_chat_id,''), COALESCE(owner_message_id,''),
		        COALESCE(sent_message_id,''), action_history,
		        COALESCE(trace_id,''), created_at, updated_at
		   FROM drafts WHERE draft_id = $1`,
		draftID,
	).Scan(
		&d.DraftID, &d.DecisionID, &d.TenantID, &d.ChatID,
		&d.BusinessConnectionID, &d.SourceMessageID,
		&d.SourceMessageText, &d.ResponseText,
		&textFormat, &draftType, &opts, &status,
		&d.OwnerChatID, &d.OwnerMessageID,
		&d.SentMessageID, &history, &d.TraceID, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return DraftRecord{}, false
	}
	d.TextFormat = contracts.TextFormat(textFormat).Normalize()
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
	// Per-chat index (ARCH-13): only this chat's messages are touched instead
	// of scanning the whole message map on every call.
	keys := r.chatMessageKeys[chatKey(tenantID, chatID)]
	messages := make([]MessageRecord, 0, len(keys))
	for _, key := range keys {
		if record, ok := r.messages[key]; ok && record.Event.EventID != "" {
			messages = append(messages, record)
		}
	}
	// Insertion order is normally chronological; sort to keep the contract
	// strict for out-of-order saves (synthetic outgoing messages, tests).
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Event.CreatedAt.Before(messages[j].Event.CreatedAt)
	})
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return messages
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
	r.invalidateContextScore(feedback.TenantID, feedback.ChatID)
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
// Results are cached per chat with a short TTL (ARCH-8); writes that change
// the inputs (profile, facts, feedback) invalidate the entry immediately.
func (r *Repository) ContextScoreForChat(tenantID, chatID string) int {
	key := tenantID + "/" + chatID
	if v, ok := r.ctxScoreCache.Load(key); ok {
		if e, ok := v.(ctxScoreEntry); ok && time.Since(e.at) < ctxScoreTTL {
			return e.score
		}
	}
	profile, _ := r.ContactProfile(tenantID, chatID)
	facts := r.SemanticFacts(tenantID, chatID, 20)
	feedback := r.RecentFeedback(tenantID, chatID, 10)
	score := contracts.ContextScore(profile, facts, feedback)
	r.ctxScoreCache.Store(key, ctxScoreEntry{score: score, at: time.Now()})
	return score
}

// invalidateContextScore drops the cached score after a write that affects it.
func (r *Repository) invalidateContextScore(tenantID, chatID string) {
	r.ctxScoreCache.Delete(tenantID + "/" + chatID)
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

// FactPair is one pair of similar facts (an edge for reflection clustering).
type FactPair struct {
	IDA int64
	IDB int64
}

// ReflectionCandidateChats returns chat ids for a tenant that have at least
// minFacts active facts created since `since` and do NOT carry the no_memory
// privacy flag — the chats worth running sleep-time reflection on (#5).
// Read-only; returns nil on error or without a DB.
func (r *Repository) ReflectionCandidateChats(tenantID string, since time.Time, minFacts int) []string {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT sf.chat_id
		   FROM semantic_facts sf
		   LEFT JOIN chat_settings cs
		     ON cs.tenant_id = sf.tenant_id AND cs.chat_id = sf.chat_id
		  WHERE sf.tenant_id = $1
		    AND sf.scope_kind = 'chat'
		    AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		    AND sf.created_at >= $2
		    AND NOT ('no_memory' = ANY(COALESCE(cs.privacy_flags, ARRAY[]::public.privacy_flag[])::text[]))
		  GROUP BY sf.chat_id
		 HAVING count(*) >= $3`,
		tenantID, since, minFacts,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var chats []string
	for rows.Next() {
		var chatID string
		if rows.Scan(&chatID) == nil {
			chats = append(chats, chatID)
		}
	}
	return chats
}

// SimilarFactPairs returns pairs of a chat's active recent facts whose embeddings
// are within (1-minSim) cosine distance — the edges the reflection clusterer
// groups (#5). Only facts that have embeddings participate. Read-only.
func (r *Repository) SimilarFactPairs(tenantID, chatID string, since time.Time, minSim float64) []FactPair {
	if r.db == nil {
		return nil
	}
	maxDist := 1.0 - minSim
	rows, err := r.db.Query(
		`SELECT a.fact_id, b.fact_id
		   FROM fact_embeddings a
		   JOIN fact_embeddings b
		     ON b.tenant_id = a.tenant_id AND b.chat_id = a.chat_id AND a.fact_id < b.fact_id
		   JOIN semantic_facts sfa ON sfa.id = a.fact_id
		     AND sfa.deleted_at IS NULL AND sfa.superseded_by_id IS NULL AND sfa.created_at >= $3
		   JOIN semantic_facts sfb ON sfb.id = b.fact_id
		     AND sfb.deleted_at IS NULL AND sfb.superseded_by_id IS NULL AND sfb.created_at >= $3
		  WHERE a.tenant_id = $1 AND a.chat_id = $2
		    AND (a.embedding <=> b.embedding) < $4`,
		tenantID, chatID, since, maxDist,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pairs []FactPair
	for rows.Next() {
		var p FactPair
		if rows.Scan(&p.IDA, &p.IDB) == nil {
			pairs = append(pairs, p)
		}
	}
	return pairs
}

// SemanticFactTextsByIDs returns id→fact_text for the given fact ids of a tenant,
// scoped defensively to the tenant. Used to materialise reflection clusters (#5).
func (r *Repository) SemanticFactTextsByIDs(tenantID string, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	if r.db == nil || len(ids) == 0 {
		return out
	}
	rows, err := r.db.Query(
		`SELECT id, fact_text FROM semantic_facts
		  WHERE tenant_id = $1 AND id = ANY($2)`,
		tenantID, pq.Array(ids),
	)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var text string
		if rows.Scan(&id, &text) == nil {
			out[id] = text
		}
	}
	return out
}

// ============================================================
// Semantic Layer (semantic_facts table)
// ============================================================

// normalizeFactKey approximates the DB normalize_fact_text hash for the
// in-memory fallback: lowercase, collapsed whitespace.
func normalizeFactKey(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// SaveSemanticFact stores a fact in the in-memory fallback only. Every DB
// write goes through the upsert_semantic_fact stored procedure (MEM-11): a
// direct INSERT path here would bypass dedup and the advisory lock.
// Returns the synthetic in-memory ID, or 0 when an exact duplicate exists.
func (r *Repository) SaveSemanticFact(fact SemanticFactRecord) int64 {
	if r.db != nil {
		return r.UpsertSemanticFact(fact, nil).FactID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := normalizeFactKey(fact.FactText)
	for _, existing := range r.semanticFacts {
		if existing.TenantID == fact.TenantID && existing.ChatID == fact.ChatID &&
			existing.FactType == fact.FactType && existing.Subject == fact.Subject &&
			normalizeFactKey(existing.FactText) == key {
			return 0
		}
	}
	r.aiRoleSeq++ // reuse the in-memory sequence; IDs only need to be unique per process
	fact.ID = r.aiRoleSeq
	r.semanticFacts = append(r.semanticFacts, fact)
	return fact.ID
}

// UpsertSemanticFact deduplicates a fact via the upsert_semantic_fact v3 stored
// procedure when a DB is configured (MEM-1): exact-hash tier always runs; the
// semantic tiers run only when the candidate embedding is provided. The proc
// returns (action, fact_id, superseded_fact_id) so no follow-up query is needed.
// In-memory mode falls back to hash-style dedup on normalised text.
//
// The in-memory semanticFacts slice is NOT appended on the DB path (ARCH-13):
// it exists solely as the no-DB fallback and previously grew unboundedly for
// the lifetime of the process.
func (r *Repository) UpsertSemanticFact(fact SemanticFactRecord, embedding []float64) SemanticFactUpsertResult {
	if r.db != nil {
		var embeddingArg any
		if len(embedding) > 0 {
			embeddingArg = float64SliceToVector(embedding)
		}
		var result SemanticFactUpsertResult
		var superseded *int64
		err := r.db.QueryRow(
			`SELECT action, fact_id, superseded_fact_id
			   FROM upsert_semantic_fact($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
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
			embeddingArg,
			"qwen/qwen3-embedding-8b",
		).Scan(&result.Action, &result.FactID, &superseded)
		if err != nil {
			r.logWriteError("upsert_semantic_fact", fact.TenantID, fact.ChatID, err)
			return SemanticFactUpsertResult{Action: "rejected"}
		}
		if superseded != nil {
			result.SupersededFactID = *superseded
		}
		r.invalidateContextScore(fact.TenantID, fact.ChatID)
		return result
	}
	id := r.SaveSemanticFact(fact)
	r.invalidateContextScore(fact.TenantID, fact.ChatID)
	if id == 0 {
		return SemanticFactUpsertResult{Action: "occurrence_added"}
	}
	return SemanticFactUpsertResult{FactID: id, Action: "inserted"}
}

// SaveAIRole creates or updates (by tenant+name) an owner-authored AI role and
// returns the stored record with its ID populated.
func (r *Repository) SaveAIRole(role AIRoleRecord) AIRoleRecord {
	if r.db != nil {
		err := r.db.QueryRow(
			`INSERT INTO ai_roles (
			   tenant_id, name, description, speaking_style, goals, constraints_text,
			   context_notes, custom_fields, rich_formatting_enabled, streaming_enabled,
			   speak_as_owner
			 )
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			 ON CONFLICT (tenant_id, name) DO UPDATE SET
			   description      = EXCLUDED.description,
			   speaking_style   = EXCLUDED.speaking_style,
			   goals            = EXCLUDED.goals,
			   constraints_text = EXCLUDED.constraints_text,
			   context_notes    = EXCLUDED.context_notes,
			   custom_fields    = EXCLUDED.custom_fields,
			   rich_formatting_enabled = COALESCE(EXCLUDED.rich_formatting_enabled, ai_roles.rich_formatting_enabled),
			   streaming_enabled       = COALESCE(EXCLUDED.streaming_enabled, ai_roles.streaming_enabled),
			   speak_as_owner   = EXCLUDED.speak_as_owner,
			   updated_at       = now()
			 RETURNING id`,
			role.TenantID, role.Name, role.Description, role.SpeakingStyle, role.Goals, role.Constraints, role.Context,
			encodeRoleCustomFields(role.CustomFields), nullableBoolArg(role.RichFormattingEnabled), nullableBoolArg(role.StreamingEnabled),
			role.SpeakAsOwner,
		).Scan(&role.ID)
		if err != nil {
			return AIRoleRecord{}
		}
		return role
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.aiRoles {
		if existing.TenantID == role.TenantID && strings.EqualFold(existing.Name, role.Name) {
			role.ID = id
			r.aiRoles[id] = role
			return role
		}
	}
	r.aiRoleSeq++
	role.ID = r.aiRoleSeq
	r.aiRoles[role.ID] = role
	return role
}

// aiRoleColumns is the shared SELECT projection for ai_roles, kept in one place
// so every read returns the same fields (including the migration-102 jsonb).
const aiRoleColumns = `id, tenant_id, name, description, speaking_style, goals, constraints_text, COALESCE(context_notes,''), COALESCE(custom_fields,'[]'::jsonb), rich_formatting_enabled, streaming_enabled, speak_as_owner`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

// scanAIRole reads one ai_roles row in aiRoleColumns order, decoding the
// custom_fields jsonb into the typed slice.
func scanAIRole(s rowScanner) (AIRoleRecord, error) {
	var role AIRoleRecord
	var customFields []byte
	var richFormatting sql.NullBool
	var streaming sql.NullBool
	if err := s.Scan(&role.ID, &role.TenantID, &role.Name, &role.Description,
		&role.SpeakingStyle, &role.Goals, &role.Constraints, &role.Context, &customFields,
		&richFormatting, &streaming, &role.SpeakAsOwner); err != nil {
		return AIRoleRecord{}, err
	}
	role.CustomFields = decodeRoleCustomFields(customFields)
	role.RichFormattingEnabled = boolPtrFromNull(richFormatting)
	role.StreamingEnabled = boolPtrFromNull(streaming)
	return role, nil
}

// decodeRoleCustomFields unmarshals the custom_fields jsonb; a malformed or
// empty value yields nil so callers always get a usable (possibly empty) slice.
func decodeRoleCustomFields(raw []byte) []contracts.AIRoleCustomField {
	if len(raw) == 0 {
		return nil
	}
	var out []contracts.AIRoleCustomField
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// encodeRoleCustomFields marshals custom fields for storage; nil/empty becomes
// the jsonb empty array so the NOT NULL column always has a valid value.
func encodeRoleCustomFields(fields []contracts.AIRoleCustomField) []byte {
	if len(fields) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// AIRoles returns all roles defined by the tenant, ordered by name.
func (r *Repository) AIRoles(tenantID string) []AIRoleRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT `+aiRoleColumns+`
			   FROM ai_roles WHERE tenant_id = $1 ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return nil
		}
		defer rows.Close()
		return scanAIRoles(rows)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]AIRoleRecord, 0, len(r.aiRoles))
	for _, role := range r.aiRoles {
		if role.TenantID == tenantID {
			result = append(result, role)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// AIRoleByName returns the tenant's role with the given name (case-insensitive).
func (r *Repository) AIRoleByName(tenantID, name string) (AIRoleRecord, bool) {
	if r.db != nil {
		role, err := scanAIRole(r.db.QueryRow(
			`SELECT `+aiRoleColumns+`
			   FROM ai_roles WHERE tenant_id = $1 AND lower(name) = lower($2)`,
			tenantID, name,
		))
		if err != nil {
			return AIRoleRecord{}, false
		}
		return role, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, role := range r.aiRoles {
		if role.TenantID == tenantID && strings.EqualFold(role.Name, name) {
			return role, true
		}
	}
	return AIRoleRecord{}, false
}

// AssignAIRoleToChat binds a role to a chat; roleID 0 clears the assignment.
// The chat_settings row is created with defaults when it does not exist yet so
// the assignment survives for chats the owner has not configured otherwise.
func (r *Repository) AssignAIRoleToChat(tenantID, chatID string, roleID int64) {
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO chat_settings (tenant_id, chat_id, mode, ai_role_id)
			 VALUES ($1, $2, 'manual'::chat_mode, NULLIF($3, 0))
			 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
			   ai_role_id = NULLIF($3, 0),
			   updated_at = now()`,
			tenantID, chatID, roleID,
		)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if roleID == 0 {
		delete(r.chatAIRoleIDs, chatKey(tenantID, chatID))
		return
	}
	r.chatAIRoleIDs[chatKey(tenantID, chatID)] = roleID
}

// AIRoleForChat resolves the role assigned to a chat, if any.
func (r *Repository) AIRoleForChat(tenantID, chatID string) (AIRoleRecord, bool) {
	if r.db != nil {
		role, err := scanAIRole(r.db.QueryRow(
			`SELECT ar.id, ar.tenant_id, ar.name, ar.description, ar.speaking_style, ar.goals,
			        ar.constraints_text, COALESCE(ar.context_notes,''), COALESCE(ar.custom_fields,'[]'::jsonb),
			        ar.rich_formatting_enabled, ar.streaming_enabled
			   FROM chat_settings cs
			   JOIN ai_roles ar ON ar.id = cs.ai_role_id
			  WHERE cs.tenant_id = $1 AND cs.chat_id = $2`,
			tenantID, chatID,
		))
		if err != nil {
			return AIRoleRecord{}, false
		}
		return role, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	roleID, ok := r.chatAIRoleIDs[chatKey(tenantID, chatID)]
	if !ok {
		return AIRoleRecord{}, false
	}
	role, ok := r.aiRoles[roleID]
	return role, ok
}

func scanAIRoles(rows *sql.Rows) []AIRoleRecord {
	result := make([]AIRoleRecord, 0)
	for rows.Next() {
		if role, err := scanAIRole(rows); err == nil {
			result = append(result, role)
		}
	}
	return result
}

// AIRoleByID returns the tenant's role with the given ID.
func (r *Repository) AIRoleByID(tenantID string, roleID int64) (AIRoleRecord, bool) {
	if r.db != nil {
		role, err := scanAIRole(r.db.QueryRow(
			`SELECT `+aiRoleColumns+`
			   FROM ai_roles WHERE tenant_id = $1 AND id = $2`,
			tenantID, roleID,
		))
		if err != nil {
			return AIRoleRecord{}, false
		}
		return role, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	role, ok := r.aiRoles[roleID]
	if !ok || role.TenantID != tenantID {
		return AIRoleRecord{}, false
	}
	return role, true
}

// RoleMCPServers returns the enabled MCP servers a role may call, with each
// server's per-role tool allow-list (#7). Read-only; nil without a DB.
func (r *Repository) RoleMCPServers(roleID int64) []contracts.MCPServerSpec {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT s.name, s.transport, s.url, COALESCE(s.auth_header,''), a.allowed_tools
		   FROM role_mcp_access a
		   JOIN mcp_servers s ON s.id = a.mcp_server_id
		  WHERE a.role_id = $1 AND s.enabled`,
		roleID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var specs []contracts.MCPServerSpec
	for rows.Next() {
		var spec contracts.MCPServerSpec
		var allowed pq.StringArray
		if rows.Scan(&spec.Name, &spec.Transport, &spec.URL, &spec.AuthHeader, &allowed) == nil {
			spec.AllowedTools = []string(allowed)
			specs = append(specs, spec)
		}
	}
	return specs
}

// SaveMCPServer upserts a tenant's MCP server (by tenant+name) and returns its
// id. Minimal seeding helper; owner-facing registration is deferred (#7).
func (r *Repository) SaveMCPServer(tenantID, name, transport, url, authHeader string) int64 {
	if r.db == nil {
		return 0
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO mcp_servers (tenant_id, name, transport, url, auth_header)
		 VALUES ($1,$2,$3,$4, NULLIF($5,''))
		 ON CONFLICT (tenant_id, name)
		 DO UPDATE SET transport = EXCLUDED.transport, url = EXCLUDED.url,
		               auth_header = EXCLUDED.auth_header, enabled = true
		 RETURNING id`,
		tenantID, name, transport, url, authHeader,
	).Scan(&id)
	if err != nil {
		r.logWriteError("save_mcp_server", tenantID, "", err)
		return 0
	}
	return id
}

// SetRoleMCPAccess grants a role access to a server with an optional tool
// allow-list (nil/empty = all tools). Minimal seeding helper (#7).
func (r *Repository) SetRoleMCPAccess(roleID, serverID int64, allowedTools []string) {
	if r.db == nil {
		return
	}
	var allowed any
	if len(allowedTools) > 0 {
		allowed = pq.Array(allowedTools)
	}
	_, err := r.db.Exec(
		`INSERT INTO role_mcp_access (role_id, mcp_server_id, allowed_tools)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (role_id, mcp_server_id) DO UPDATE SET allowed_tools = EXCLUDED.allowed_tools`,
		roleID, serverID, allowed,
	)
	if err != nil {
		r.logWriteError("set_role_mcp_access", "", "", err)
	}
}

// UpdateAIRole rewrites an existing role addressed by ID. Unlike SaveAIRole
// (which upserts by tenant+name and would create a second role on rename),
// this is the rename-safe path. Returns false when the role does not exist.
func (r *Repository) UpdateAIRole(role AIRoleRecord) bool {
	if r.db != nil {
		res, err := r.db.Exec(
			`UPDATE ai_roles
			    SET name = $3, description = $4, speaking_style = $5,
			        goals = $6, constraints_text = $7, context_notes = $8,
			        custom_fields = $9, rich_formatting_enabled = $10,
			        streaming_enabled = $11, speak_as_owner = $12, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			role.TenantID, role.ID, role.Name, role.Description,
			role.SpeakingStyle, role.Goals, role.Constraints, role.Context,
			encodeRoleCustomFields(role.CustomFields),
			nullableBoolArg(role.RichFormattingEnabled), nullableBoolArg(role.StreamingEnabled),
			role.SpeakAsOwner,
		)
		if err != nil {
			r.logWriteError("update_ai_role", role.TenantID, "", err)
			return false
		}
		n, _ := res.RowsAffected()
		return n > 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.aiRoles[role.ID]
	if !ok || existing.TenantID != role.TenantID {
		return false
	}
	r.aiRoles[role.ID] = role
	return true
}

// DeleteAIRole removes a role and clears its assignment from every chat in the
// same transaction, so no chat_settings row is left pointing at a missing role.
// Deleting an absent role is a no-op (idempotent).
func (r *Repository) DeleteAIRole(tenantID string, roleID int64) error {
	r.mu.Lock()
	if role, ok := r.aiRoles[roleID]; ok && role.TenantID == tenantID {
		delete(r.aiRoles, roleID)
		for key, id := range r.chatAIRoleIDs {
			if id == roleID {
				delete(r.chatAIRoleIDs, key)
			}
		}
	}
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE chat_settings SET ai_role_id = NULL, updated_at = now()
		  WHERE tenant_id = $1 AND ai_role_id = $2`,
		tenantID, roleID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("delete_ai_role_unassign", tenantID, "", err)
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM ai_roles WHERE tenant_id = $1 AND id = $2`,
		tenantID, roleID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("delete_ai_role", tenantID, "", err)
		return err
	}
	return tx.Commit()
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
	_, err := r.db.Exec(
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
	r.logWriteError("upsert_active_episode", episode.TenantID, episode.ChatID, err)
}

// UpdateEpisodeRuntime sets runtime-state fields on the active episode without
// touching message_count (MEM-5): UpsertActiveEpisode increments the counter on
// conflict, so using it for post-AI runtime writes would inflate the count.
// Empty arguments leave the corresponding field unchanged.
func (r *Repository) UpdateEpisodeRuntime(tenantID, chatID, topic, openQuestionMsgID, pendingOwnerAction string) {
	r.mu.Lock()
	key := chatKey(tenantID, chatID)
	if ep, ok := r.activeEpisodes[key]; ok {
		if topic != "" {
			ep.CurrentTopic = topic
		}
		if openQuestionMsgID != "" {
			ep.OpenQuestionMsgID = openQuestionMsgID
		}
		if pendingOwnerAction != "" {
			ep.PendingOwnerAction = pendingOwnerAction
		}
		r.activeEpisodes[key] = ep
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`UPDATE active_episodes
		    SET current_topic            = COALESCE(NULLIF($3,''), current_topic),
		        open_question_message_id = COALESCE(NULLIF($4,''), open_question_message_id),
		        pending_owner_action     = COALESCE(NULLIF($5,''), pending_owner_action),
		        updated_at               = now()
		  WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID, topic, openQuestionMsgID, pendingOwnerAction,
	)
	r.logWriteError("update_episode_runtime", tenantID, chatID, err)
}

// ClearEpisodeRuntimeState nulls open_question_message_id and
// pending_owner_action on the active episode (MEM-5). A separate method is
// required because UpsertActiveEpisode COALESCEs those columns and therefore
// cannot reset them.
func (r *Repository) ClearEpisodeRuntimeState(tenantID, chatID string) {
	r.mu.Lock()
	key := chatKey(tenantID, chatID)
	if ep, ok := r.activeEpisodes[key]; ok {
		ep.OpenQuestionMsgID = ""
		ep.PendingOwnerAction = ""
		r.activeEpisodes[key] = ep
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`UPDATE active_episodes
		    SET open_question_message_id = NULL,
		        pending_owner_action = NULL,
		        updated_at = now()
		  WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
	r.logWriteError("clear_episode_runtime_state", tenantID, chatID, err)
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
	_, err := r.db.Exec(
		`DELETE FROM active_episodes WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
	r.logWriteError("close_active_episode", tenantID, chatID, err)
}

// LogExtraction records one async fact-extraction attempt to the extraction_log table.
// Best-effort: a failure must never break the extraction path, but it is logged
// (MEM-10) so a broken diagnostics table is itself observable.
// No-op when running without a DB (unit tests use the in-memory repository).
func (r *Repository) LogExtraction(rec ExtractionLogRecord) {
	if r.db == nil {
		return
	}
	rawOutput := rec.RawOutput
	if len(rawOutput) > 4000 {
		rawOutput = rawOutput[:4000]
	}
	_, err := r.db.Exec(
		`INSERT INTO extraction_log (
		   tenant_id, chat_id, input_message_ids, model_used, raw_output,
		   facts_created, facts_skipped, facts_merged, error, duration_ms
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.TenantID, rec.ChatID,
		pq.Array(nilToEmpty(rec.InputMessageIDs)),
		rec.ModelUsed,
		// raw_output is JSONB; store the verbatim text as a JSON string value.
		rawOutputJSON(rawOutput),
		pq.Array(nilToEmptyInt64(rec.FactsCreated)),
		rec.FactsSkipped, rec.FactsMerged,
		nullableString(rec.Error),
		rec.DurationMS,
	)
	r.logWriteError("log_extraction", rec.TenantID, rec.ChatID, err)
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
// Returns the inserted row ID (0 in memory-only mode), used by the embedding worker.
func (r *Repository) SaveEpisodeSummary(summary EpisodeSummaryRecord) int64 {
	r.mu.Lock()
	r.episodeSummaries = append(r.episodeSummaries, summary)
	r.mu.Unlock()
	if r.db == nil {
		return 0
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO episode_summaries (
		   tenant_id, chat_id, period_start, period_end, content,
		   message_count, closure_reason, source_message_ids
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		summary.TenantID, summary.ChatID,
		summary.PeriodStart, summary.PeriodEnd,
		summary.Content, summary.MessageCount,
		valueOrDefault(summary.ClosureReason, "manual"),
		pq.Array(nilToEmpty(summary.SourceMessageIDs)),
	).Scan(&id)
	// id=0 already signals failure to callers; the log makes the cause visible
	// (a CHECK-constraint violation here once silently dropped all summaries — MEM-10).
	r.logWriteError("save_episode_summary", summary.TenantID, summary.ChatID, err)
	return id
}

// SaveEpisodeSummaryEmbedding persists the embedding vector for an episode summary row.
// Called by the embedding worker after the Python service generates the vector.
func (r *Repository) SaveEpisodeSummaryEmbedding(id int64, tenantID string, embedding []float64, model string) {
	if r.db == nil || len(embedding) == 0 {
		return
	}
	vec := float64SliceToVector(embedding)
	_, err := r.db.Exec(
		`UPDATE episode_summaries
		    SET embedding = $1::vector, embedding_model = $2
		  WHERE id = $3 AND tenant_id = $4`,
		vec, model, id, tenantID,
	)
	// A silent failure here once made the worker re-embed the same rows forever
	// (missing column, fixed in migration 012) — keep it observable (MEM-10).
	r.logWriteError("save_episode_summary_embedding", tenantID, "", err)
}

// EpisodeSummariesWithoutEmbeddings returns up to limit episode summary rows that
// have no embedding vector yet. Used by the embedding worker for batch embedding.
func (r *Repository) EpisodeSummariesWithoutEmbeddings(limit int) []EpisodeSummaryRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at
		   FROM episode_summaries
		  WHERE embedding IS NULL
		    AND deleted_at IS NULL
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil
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
	return result
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
func (r *Repository) hardIncludeChatFacts(tenantID, chatID string) []SemanticFactRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
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
	hardFacts := r.hardIncludeChatFacts(tenantID, chatID)

	// Top-K by cosine similarity, excluding hard-include types (they are already in).
	remaining := limit - len(hardFacts)
	if remaining < 1 {
		remaining = 1
	}
	vec := float64SliceToVector(queryVector)
	// Two-stage ranking (MEM-7): the inner ANN query orders purely by cosine
	// distance — the only shape the HNSW expression index can serve — over an
	// oversampled K×3 candidate set; the type-boost re-sort happens outside.
	// A boost-multiplied ORDER BY on the index expression would force a seq scan.
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT id, tenant_id, scope_kind, chat_id, fact_text, fact_type,
		        subject, confidence, source, source_message_id, expires_at, created_at
		   FROM (
		     SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,'') AS chat_id,
		            sf.fact_text, sf.fact_type, sf.subject, sf.confidence, sf.source,
		            COALESCE(sf.source_message_id,'') AS source_message_id, sf.expires_at, sf.created_at,
		            %s AS dist
		       FROM semantic_facts sf
		       JOIN fact_embeddings fe ON fe.fact_id = sf.id
		      WHERE sf.tenant_id = $1 AND sf.chat_id = $2
		        AND sf.scope_kind = 'chat'
		        AND sf.fact_type NOT IN ('taboo', 'health')
		        AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
		        AND fe.embedding IS NOT NULL
		      ORDER BY %s ASC
		      LIMIT $4 * 3
		   ) candidates
		  ORDER BY dist * CASE fact_type
		             WHEN 'commitment' THEN 0.667
		             WHEN 'preference' THEN 0.833
		             ELSE 1.0 END ASC
		  LIMIT $4`,
		r.vectorOrderExpr("fe.embedding", "$3"),
		r.vectorOrderExpr("fe.embedding", "$3"),
	),
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

// SemanticFactsByBM25 returns chat-scoped facts ranked by full-text (BM25) relevance to query.
// Uses the 'simple' text-search configuration so it handles Russian and English without
// a language-specific dictionary. Returns nil when the fact_tsv column is absent (pre-migration).
func (r *Repository) SemanticFactsByBM25(tenantID, chatID, query string, limit int) []SemanticFactRecord {
	if r.db == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
		        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
		   FROM semantic_facts
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND scope_kind = 'chat'
		    AND fact_tsv @@ plainto_tsquery('simple', $3)
		    AND deleted_at IS NULL AND superseded_by_id IS NULL
		  ORDER BY ts_rank_cd(fact_tsv, plainto_tsquery('simple', $3)) DESC
		  LIMIT $4`,
		tenantID, chatID, query, limit,
	)
	if err != nil {
		return nil // column absent or query error — caller falls back to vector-only
	}
	defer rows.Close()
	return scanSemanticFacts(rows)
}

// SemanticFactsByIDs returns facts with the given IDs in the order the IDs are provided.
// Used by the reranking path to assemble the final fact list from reranker-ordered IDs.
func (r *Repository) SemanticFactsByIDs(tenantID string, ids []int64) []SemanticFactRecord {
	if r.db == nil || len(ids) == 0 {
		return nil
	}
	// Fetch all requested facts in one round-trip; preserve requested order below.
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, tenantID)
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	query := fmt.Sprintf(
		`SELECT id, tenant_id, scope_kind, COALESCE(chat_id,''), fact_text, fact_type,
		        subject, confidence, source, COALESCE(source_message_id,''), expires_at, created_at
		   FROM semantic_facts
		  WHERE tenant_id = $1 AND id IN (%s)
		    AND deleted_at IS NULL`,
		strings.Join(placeholders, ","),
	)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	byID := make(map[int64]SemanticFactRecord)
	for _, f := range scanSemanticFacts(rows) {
		byID[f.ID] = f
	}
	// Return in the caller-specified order (reranker-sorted).
	result := make([]SemanticFactRecord, 0, len(ids))
	for _, id := range ids {
		if f, ok := byID[id]; ok {
			result = append(result, f)
		}
	}
	return result
}

// RankFactsByCosine reorders the given fact IDs by cosine distance to queryVector
// (closest first), truncated to limit. Facts without an embedding row are absent
// from the result — callers backfill them from their pre-ranked order. Replaces
// the listwise LLM reranker in the hot retrieval path (ARCH-5): the embeddings
// already exist in fact_embeddings, so the final ordering is one cheap SQL query.
func (r *Repository) RankFactsByCosine(tenantID string, ids []int64, queryVector []float64, limit int) []SemanticFactRecord {
	if r.db == nil || len(ids) == 0 || len(queryVector) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+3)
	args = append(args, tenantID, float64SliceToVector(queryVector))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, id)
	}
	args = append(args, limit)
	query := fmt.Sprintf(
		`SELECT sf.id, sf.tenant_id, sf.scope_kind, COALESCE(sf.chat_id,''), sf.fact_text, sf.fact_type,
		        sf.subject, sf.confidence, sf.source, COALESCE(sf.source_message_id,''), sf.expires_at, sf.created_at
		   FROM semantic_facts sf
		   JOIN fact_embeddings fe ON fe.fact_id = sf.id
		  WHERE sf.tenant_id = $1 AND sf.id IN (%s)
		    AND sf.deleted_at IS NULL
		  ORDER BY %s ASC
		  LIMIT $%d`,
		strings.Join(placeholders, ","), r.vectorOrderExpr("fe.embedding", "$2"), len(ids)+3,
	)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanSemanticFacts(rows)
}

// SaveFactRelation persists a directed temporal edge between two semantic_facts rows.
// Idempotent: ON CONFLICT DO NOTHING so duplicate calls are safe.
func (r *Repository) SaveFactRelation(rel FactRelationRecord) {
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`INSERT INTO fact_relations (tenant_id, fact_id_from, fact_id_to, relation_type, confidence)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (fact_id_from, fact_id_to, relation_type) DO NOTHING`,
		rel.TenantID, rel.FactIDFrom, rel.FactIDTo, rel.RelationType, rel.Confidence,
	)
	r.logWriteError("save_fact_relation", rel.TenantID, "", err)
}

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
	Status         string // pending | accepted | rejected | expired
	CreatedAt      time.Time
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
			switch resolved.FieldName {
			case "relationship_type":
				profile.RelationshipType = resolved.SuggestedValue
			case "preferred_address":
				profile.PreferredAddress = resolved.SuggestedValue
			case "language":
				profile.Language = resolved.SuggestedValue
			case "tone":
				profile.Tone = resolved.SuggestedValue
			case "gender":
				profile.Gender = resolved.SuggestedValue
			}
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
		// Ensure the profile row exists, then set the single whitelisted column.
		if _, err := tx.Exec(
			`INSERT INTO contact_profiles (tenant_id, chat_id)
			 VALUES ($1, $2) ON CONFLICT (tenant_id, chat_id) DO NOTHING`,
			tenantID, chatID,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		// fieldName is whitelist-validated above — safe in identifier position.
		if _, err := tx.Exec(
			fmt.Sprintf(`UPDATE contact_profiles SET %s = $3, updated_at = now()
			  WHERE tenant_id = $1 AND chat_id = $2`, fieldName),
			tenantID, chatID, value,
		); err != nil {
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

// ============================================================
// Deletion contract (§10.2, MEM-2): hard delete, never soft.
// ============================================================

// insertForgetAuditTx writes the GDPR audit row inside the deletion transaction.
// audit_events carries a NOT NULL FK to messages with ON DELETE CASCADE, so an
// audit row referencing a deleted message would vanish with it; instead a
// synthetic marker message is inserted first and the audit row points at it.
// This is why the audit insert happens after the deletes — atomicity within
// the transaction still guarantees "no deletion without an audit record".
func insertForgetAuditTx(tx *sql.Tx, tenantID, chatID, auditType string) error {
	markerID := fmt.Sprintf("%s:%d", auditType, time.Now().UnixNano())
	if _, err := tx.Exec(
		`INSERT INTO messages (
		   tenant_id, chat_id, source_message_id, event_id, telegram_update_id,
		   business_connection_id, sender_id, message_text, message_type
		 ) VALUES ($1, $2, $3, $3, 0, '', 'system', '', 'system')`,
		tenantID, chatID, markerID,
	); err != nil {
		return err
	}
	_, err := tx.Exec(
		`INSERT INTO audit_events (tenant_id, chat_id, source_message_id, decision, reason)
		 VALUES ($1, $2, $3, 'ignore'::decision, $4)`,
		tenantID, chatID, markerID, auditType,
	)
	return err
}

// forgetChatTx hard-deletes every memory artifact of one chat inside tx.
// Health facts go first as their own statement (§10.3); FK cascades from
// semantic_facts cover fact_embeddings / fact_occurrences / fact_relations,
// and the messages delete cascades message_states + audit_events. The chats
// row and chat_settings survive — the owner forgets the memory, not the chat.
func forgetChatTx(tx *sql.Tx, tenantID, chatID string) error {
	statements := []string{
		// §10.3: health facts deleted by a dedicated first statement.
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND chat_id = $2 AND fact_type = 'health'`,
		// Cascades: fact_embeddings, fact_occurrences, fact_relations (FK ON DELETE CASCADE).
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM episode_summaries WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM active_episodes WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM extraction_log WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM contact_profile_suggestions WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM contact_profiles WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM draft_feedback WHERE tenant_id = $1 AND chat_id = $2`,
		`DELETE FROM drafts WHERE tenant_id = $1 AND chat_id = $2`,
		// Cascades: message_states, audit_events (FK ON DELETE CASCADE).
		`DELETE FROM messages WHERE tenant_id = $1 AND chat_id = $2`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt, tenantID, chatID); err != nil {
			return err
		}
	}
	return insertForgetAuditTx(tx, tenantID, chatID, "memory_forget_chat")
}

// forgetChatMemoryLocked scrubs the in-memory mirrors for one chat. Caller holds r.mu.
func (r *Repository) forgetChatMemoryLocked(tenantID, chatID string) {
	key := chatKey(tenantID, chatID)
	for _, msgKey := range r.chatMessageKeys[key] {
		delete(r.messages, msgKey)
	}
	delete(r.chatMessageKeys, key)
	delete(r.contactProfiles, key)
	delete(r.activeEpisodes, key)
	delete(r.pendingInputs, key)
	delete(r.messageCounts, key)
	for id, d := range r.drafts {
		if d.TenantID == tenantID && d.ChatID == chatID {
			delete(r.drafts, id)
		}
	}
	keepFeedback := r.draftFeedbacks[:0]
	for _, fb := range r.draftFeedbacks {
		if !(fb.TenantID == tenantID && fb.ChatID == chatID) {
			keepFeedback = append(keepFeedback, fb)
		}
	}
	r.draftFeedbacks = keepFeedback
	keepFacts := r.semanticFacts[:0]
	for _, f := range r.semanticFacts {
		if !(f.TenantID == tenantID && f.ChatID == chatID) {
			keepFacts = append(keepFacts, f)
		}
	}
	r.semanticFacts = keepFacts
	keepSummaries := r.episodeSummaries[:0]
	for _, s := range r.episodeSummaries {
		if !(s.TenantID == tenantID && s.ChatID == chatID) {
			keepSummaries = append(keepSummaries, s)
		}
	}
	r.episodeSummaries = keepSummaries
	keepSuggestions := r.profileSuggestions[:0]
	for _, s := range r.profileSuggestions {
		if !(s.TenantID == tenantID && s.ChatID == chatID) {
			keepSuggestions = append(keepSuggestions, s)
		}
	}
	r.profileSuggestions = keepSuggestions
	r.invalidateContextScore(tenantID, chatID)
}

// ForgetChat hard-deletes all memory of one chat (§10.2): facts, embeddings,
// occurrences, relations, episode summaries, active episode, extraction log,
// profile + suggestions, drafts, feedback, messages and their states/audits.
// One transaction; an audit marker row records the operation. The chat row
// itself and its settings remain so the chat keeps working with a clean slate.
func (r *Repository) ForgetChat(tenantID, chatID string) error {
	if tenantID == "" || chatID == "" {
		return fmt.Errorf("tenant_id and chat_id are required")
	}
	r.mu.Lock()
	r.forgetChatMemoryLocked(tenantID, chatID)
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := forgetChatTx(tx, tenantID, chatID); err != nil {
		_ = tx.Rollback()
		r.logWriteError("forget_chat", tenantID, chatID, err)
		return err
	}
	return tx.Commit()
}

// UnlearnFact hard-deletes a single semantic fact (§10.2). FK cascades remove
// its embeddings, occurrences, and relations; the audit marker records the act.
func (r *Repository) UnlearnFact(tenantID string, factID int64) error {
	if tenantID == "" || factID == 0 {
		return fmt.Errorf("tenant_id and fact_id are required")
	}
	r.mu.Lock()
	chatID := ""
	keep := r.semanticFacts[:0]
	for _, f := range r.semanticFacts {
		if f.TenantID == tenantID && f.ID == factID {
			chatID = f.ChatID
			continue
		}
		keep = append(keep, f)
	}
	r.semanticFacts = keep
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if chatID == "" {
		// The in-memory mirror may not know the fact; resolve its chat for the audit row.
		_ = tx.QueryRow(
			`SELECT COALESCE(chat_id,'') FROM semantic_facts WHERE tenant_id = $1 AND id = $2`,
			tenantID, factID,
		).Scan(&chatID)
	}
	if _, err := tx.Exec(
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND id = $2`,
		tenantID, factID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("unlearn_fact", tenantID, chatID, err)
		return err
	}
	if chatID != "" {
		if err := insertForgetAuditTx(tx, tenantID, chatID, "memory_unlearn_fact"); err != nil {
			_ = tx.Rollback()
			r.logWriteError("unlearn_fact_audit", tenantID, chatID, err)
			return err
		}
	}
	if chatID != "" {
		r.invalidateContextScore(tenantID, chatID)
	}
	return tx.Commit()
}

// ForgetAll hard-deletes every chat-derived artifact of a tenant (§10.2):
// each chat's memory (per ForgetChat), then the chats themselves (cascading
// chat_settings), business connections, unclaimed chats, idempotency records,
// user settings, and AI roles. The tenants/users rows survive together with
// one synthetic audit marker recording the erasure.
func (r *Repository) ForgetAll(tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	r.mu.Lock()
	for key, chat := range r.chats {
		if chat.TenantID == tenantID {
			r.forgetChatMemoryLocked(tenantID, chat.ChatID)
			delete(r.chats, key)
		}
	}
	for key, settings := range r.chatSettings {
		if settings.TenantID == tenantID {
			delete(r.chatSettings, key)
		}
	}
	for key, uc := range r.unclaimedChats {
		if uc.OwnerTenantID == tenantID {
			delete(r.unclaimedChats, key)
		}
	}
	for id, conn := range r.businessConnections {
		if conn.TenantID == tenantID {
			delete(r.businessConnections, id)
			delete(r.connectionByUser, conn.UserID)
			delete(r.connectionTenants, id)
		}
	}
	delete(r.userSettings, tenantID)
	for id, role := range r.aiRoles {
		if role.TenantID == tenantID {
			delete(r.aiRoles, id)
		}
	}
	r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	statements := []string{
		// §10.3: tenant-wide health facts first.
		`DELETE FROM semantic_facts WHERE tenant_id = $1 AND fact_type = 'health'`,
		`DELETE FROM semantic_facts WHERE tenant_id = $1`,
		`DELETE FROM extraction_log WHERE tenant_id = $1`,
		// Cascades from chats: chat_settings, messages (→ states, audits), drafts,
		// draft_feedback, contact_profiles, suggestions, episodes, summaries.
		`DELETE FROM chats WHERE tenant_id = $1`,
		`DELETE FROM unclaimed_chats WHERE owner_tenant_id = $1`,
		`DELETE FROM telegram_business_connections WHERE tenant_id = $1`,
		`DELETE FROM idempotency_records WHERE tenant_id = $1`,
		`DELETE FROM user_settings WHERE tenant_id = $1`,
		`DELETE FROM ai_roles WHERE tenant_id = $1`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt, tenantID); err != nil {
			_ = tx.Rollback()
			r.logWriteError("forget_all", tenantID, "", err)
			return err
		}
	}
	// Synthetic chat row so the audit marker has a valid FK chain.
	if _, err := tx.Exec(
		`INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, display_name, chat_type, status)
		 VALUES ($1, 'forget_all', 'forget_all', 'GDPR erasure marker', 'system', 'archived')
		 ON CONFLICT DO NOTHING`,
		tenantID,
	); err != nil {
		_ = tx.Rollback()
		r.logWriteError("forget_all_marker", tenantID, "", err)
		return err
	}
	if err := insertForgetAuditTx(tx, tenantID, "forget_all", "memory_forget_all"); err != nil {
		_ = tx.Rollback()
		r.logWriteError("forget_all_audit", tenantID, "", err)
		return err
	}
	return tx.Commit()
}

// FactRelationsByFactID returns all temporal edges where fact_id_from = factID.
func (r *Repository) FactRelationsByFactID(tenantID string, factID int64) []FactRelationRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, fact_id_from, fact_id_to, relation_type, confidence
		   FROM fact_relations
		  WHERE tenant_id = $1 AND fact_id_from = $2`,
		tenantID, factID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []FactRelationRecord
	for rows.Next() {
		var rel FactRelationRecord
		if rows.Scan(&rel.ID, &rel.TenantID, &rel.FactIDFrom, &rel.FactIDTo, &rel.RelationType, &rel.Confidence) == nil {
			result = append(result, rel)
		}
	}
	return result
}

// EpisodeSummariesByVector returns episode summaries ranked by cosine similarity to
// queryVector. Falls back to EpisodeSummaries (recency) when queryVector is empty or
// the embedding column has no data (pgvector unavailable or summaries not yet embedded).
func (r *Repository) EpisodeSummariesByVector(tenantID, chatID string, queryVector []float64, limit int) []EpisodeSummaryRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at
		   FROM episode_summaries
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND embedding IS NOT NULL
		    AND deleted_at IS NULL
		  ORDER BY %s ASC
		  LIMIT $4`,
		r.vectorOrderExpr("embedding", "$3"),
	),
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

// EpisodeSummaryOverlaps reports whether any summary already covers part of
// the [start, end) period. This is the episode-closure idempotency check
// (MEM-6): the previous one-summary-per-day rule silently dropped the second
// episode of a day.
func (r *Repository) EpisodeSummaryOverlaps(tenantID, chatID string, start, end time.Time) bool {
	if r.db != nil {
		var exists bool
		err := r.db.QueryRow(
			`SELECT EXISTS(
			   SELECT 1 FROM episode_summaries
			    WHERE tenant_id = $1 AND chat_id = $2
			      AND period_end > $3 AND period_start < $4
			      AND deleted_at IS NULL
			 )`,
			tenantID, chatID, start, end,
		).Scan(&exists)
		if err == nil {
			return exists
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			s.PeriodEnd.After(start) && s.PeriodStart.Before(end) {
			return true
		}
	}
	return false
}

// LatestCoveredEnd returns the latest period_end among summaries overlapping
// [start, end), or the zero time when none overlap. The daily fallback uses it
// to summarise only the part of the day no episode summary covered (MEM-6).
func (r *Repository) LatestCoveredEnd(tenantID, chatID string, start, end time.Time) time.Time {
	if r.db != nil {
		var covered sql.NullTime
		err := r.db.QueryRow(
			`SELECT MAX(period_end) FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND period_end > $3 AND period_start < $4
			    AND deleted_at IS NULL`,
			tenantID, chatID, start, end,
		).Scan(&covered)
		if err == nil {
			if covered.Valid {
				return covered.Time
			}
			return time.Time{}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest time.Time
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			s.PeriodEnd.After(start) && s.PeriodStart.Before(end) &&
			s.PeriodEnd.After(latest) {
			latest = s.PeriodEnd
		}
	}
	return latest
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

// ============================================================
// Helpers
// ============================================================

func boolPtrFromNull(value sql.NullBool) *bool {
	if !value.Valid {
		return nil
	}
	v := value.Bool
	return &v
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func nullableBoolArg(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
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

func nilToEmptyInt64(s []int64) []int64 {
	if s == nil {
		return []int64{}
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
	_, err := r.db.Exec(
		`INSERT INTO fact_embeddings (fact_id, tenant_id, scope_kind, chat_id, embedding_model, embedding)
		 VALUES ($1, $2, $3, $4, $5, $6::float8[])
		 ON CONFLICT (fact_id) DO UPDATE SET
		   embedding       = EXCLUDED.embedding,
		   embedding_model = EXCLUDED.embedding_model`,
		factID, tenantID, scopeKind, nullableString(chatID), model, float64SliceToArray(embedding),
	)
	r.logWriteError("save_fact_embedding", tenantID, chatID, err)
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

// ConsolidateFacts calls the consolidate_facts stored procedure for a tenant.
// Returns the number of facts cleaned up (expired + soft-deleted). Safe to call
// repeatedly; all operations are idempotent.
func (r *Repository) ConsolidateFacts(tenantID string) int {
	if r.db == nil {
		return 0
	}
	var cleaned int
	_ = r.db.QueryRow(`SELECT consolidate_facts($1)`, tenantID).Scan(&cleaned)
	return cleaned
}

func chatKey(tenantID, chatID string) string {
	return tenantID + ":" + chatID
}

func messageKey(tenantID, chatID, sourceMessageID string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, chatID, sourceMessageID)
}
