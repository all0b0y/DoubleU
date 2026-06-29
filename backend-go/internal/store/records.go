package store

import (
	"time"

	"doubleu/backend-go/internal/contracts"
)

// MessageRecord stores operational state for a normalized message.
type MessageRecord struct {
	Event       contracts.NormalizedTelegramEvent
	States      []contracts.MessageState
	Quarantined bool // persist for audit; excluded from every memory surface when true
}

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
	SuggestedOptions []string // owner choices for "question"; contact buttons for interaction "reply"
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

// ScheduledJobRecord is one deferred owner/contact action created by AI or the owner.
type ScheduledJobRecord struct {
	ID             int64          `json:"id"`
	TenantID       string         `json:"tenant_id"`
	ChatID         string         `json:"chat_id"`
	BotID          string         `json:"bot_id,omitempty"`
	Kind           string         `json:"kind"`
	Target         string         `json:"target"`
	FireAt         time.Time      `json:"fire_at"`
	Payload        map[string]any `json:"payload"`
	Status         string         `json:"status"`
	CreatedBy      string         `json:"created_by"`
	IdempotencyKey string         `json:"idempotency_key"`
	CreatedAt      time.Time      `json:"created_at"`
	SentAt         *time.Time     `json:"sent_at,omitempty"`
}

// FormAnswer pairs one questionnaire question with the contact's answer to it.
type FormAnswer struct {
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
}

// InteractionFormRecord is the persisted state of a step-by-step questionnaire
// (interaction.ask_questions). Questions are asked one at a time; Answers grows
// as the contact replies (by button tap or free text) and len(Answers) equals
// CurrentIndex. Exactly one form per chat may be active at a time.
type InteractionFormRecord struct {
	ID              int64                `json:"id"`
	TenantID        string               `json:"tenant_id"`
	ChatID          string               `json:"chat_id"`
	BotID           string               `json:"bot_id,omitempty"`
	Questions       []contracts.Question `json:"questions"`
	CurrentIndex    int                  `json:"current_index"`
	Answers         []FormAnswer         `json:"answers"`
	Status          string               `json:"status"`
	SourceMessageID string               `json:"source_message_id,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	ExpiresAt       time.Time            `json:"expires_at"`
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
	// ModelTier is the LLM quality tier for draft generation: "fast",
	// "balanced" (default), or "smart". Empty = use the DRAFT task model.
	ModelTier string
	// ModelName optionally pins a concrete model inside ModelTier. Empty keeps
	// the tier primary.
	ModelName string
	// ToolSettings stores explicit per-role native-tool overrides. Missing keys
	// inherit defaults from the tool catalog (migration 113).
	ToolSettings map[string]bool
}
