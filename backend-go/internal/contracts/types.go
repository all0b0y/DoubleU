// Package contracts contains Go representations of the frozen cross-service contracts.
package contracts

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Decision is the final or proposed action for an inbound message.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type Decision string

// IsValid reports whether the decision is part of the frozen contract.
func (d Decision) IsValid() bool {
	switch d {
	case DecisionDraft, DecisionAutoReply, DecisionIgnore, DecisionObserve, DecisionAskOwner, DecisionErrorFallback:
		return true
	default:
		return false
	}
}

// ToolCategory groups native tools by their execution and visibility semantics.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type ToolCategory string

// IsValid reports whether the category is part of the frozen contract.
func (c ToolCategory) IsValid() bool {
	switch c {
	case ToolCategoryRead, ToolCategorySchedule, ToolCategoryIntegration, ToolCategoryInteraction:
		return true
	default:
		return false
	}
}

// ChatMode controls the autonomy level for one tenant/chat pair.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type ChatMode string

// IsValid reports whether the chat mode is part of the frozen contract.
func (m ChatMode) IsValid() bool {
	switch m {
	case ChatModeDisabled, ChatModeSilent, ChatModeManual, ChatModeSemiAuto, ChatModeAuto:
		return true
	default:
		return false
	}
}

// PrivacyFlag is a policy flag attached to chat settings.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type PrivacyFlag string

// IsValid reports whether the privacy flag is part of the frozen contract.
func (f PrivacyFlag) IsValid() bool {
	switch f {
	case PrivacyNoMemory, PrivacyNoTraining, PrivacyPrivate, PrivacyNoSummary:
		return true
	default:
		return false
	}
}

// PrivacyFlags is the list of privacy flags on chat settings.
type PrivacyFlags []PrivacyFlag

// Contains reports whether the flag is present.
func (f PrivacyFlags) Contains(flag PrivacyFlag) bool {
	for _, existing := range f {
		if existing == flag {
			return true
		}
	}
	return false
}

// Add returns a copy with flag present exactly once.
func (f PrivacyFlags) Add(flag PrivacyFlag) PrivacyFlags {
	if f.Contains(flag) {
		return append(PrivacyFlags(nil), f...)
	}
	next := append(PrivacyFlags(nil), f...)
	return append(next, flag)
}

// Remove returns a copy with flag absent.
func (f PrivacyFlags) Remove(flag PrivacyFlag) PrivacyFlags {
	result := make(PrivacyFlags, 0, len(f))
	for _, existing := range f {
		if existing != flag {
			result = append(result, existing)
		}
	}
	return result
}

// IsValid reports whether every privacy flag is part of the frozen contract.
func (f PrivacyFlags) IsValid() bool {
	for _, flag := range f {
		if !flag.IsValid() {
			return false
		}
	}
	return true
}

// RiskLevel is the AI-classified risk level for a response.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type RiskLevel string

// IsValid reports whether the risk level is part of the frozen contract.
func (r RiskLevel) IsValid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	default:
		return false
	}
}

// TextFormat declares the delivery formatting requested by the AI service.
type TextFormat string

const (
	// TextFormatPlain sends the response as plain text with no parse_mode.
	TextFormatPlain TextFormat = "plain"
	// TextFormatRich sends the response through Telegram Rich Messages.
	TextFormatRich TextFormat = "rich"
)

// Normalize returns a supported text format, defaulting old/unknown values to plain.
func (f TextFormat) Normalize() TextFormat {
	if f == TextFormatRich {
		return TextFormatRich
	}
	return TextFormatPlain
}

// MessageState is an auditable lifecycle state for an inbound message.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type MessageState string

// LLMPurpose tags every LLM call with the pipeline stage it belongs to.
// Routed to LiteLLM as the X-LiteLLM-Purpose header and used by the
// InputSafetyScanner / OutputSafetyScanner to pick the right scanner set.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type LLMPurpose string

// IsValid reports whether the purpose is part of the frozen contract.
func (p LLMPurpose) IsValid() bool {
	switch p {
	case PurposeDraft, PurposeExtract, PurposeSummarize, PurposeEmbed, PurposeModerate, PurposeClassify:
		return true
	default:
		return false
	}
}

// ApprovalAction is the owner callback action.
// Allowed values are generated from contracts/contracts.json — see contract_enums.gen.go.
type ApprovalAction string

// IsValid reports whether the approval action is part of the frozen contract.
func (a ApprovalAction) IsValid() bool {
	switch a {
	case ApprovalApprove, ApprovalReject, ApprovalRegenerate, ApprovalEdit,
		ApprovalMakeShorter, ApprovalMakeMoreFormal, ApprovalNoMemory,
		ApprovalThumbUp, ApprovalThumbDown, ApprovalEditRequest, ApprovalAIRefine,
		ApprovalChooseOption:
		return true
	default:
		return false
	}
}

// callbackActionCodes maps short 2-char codes to ApprovalAction for Telegram's 64-byte callback_data limit.
var callbackActionCodes = map[string]ApprovalAction{
	"ap": ApprovalApprove,
	"rj": ApprovalReject,
	"rg": ApprovalRegenerate,
	"ed": ApprovalEditRequest,
	"ar": ApprovalAIRefine,
	"ss": ApprovalMakeShorter,
	"fm": ApprovalMakeMoreFormal,
	"nm": ApprovalNoMemory,
	"tu": ApprovalThumbUp,
	"td": ApprovalThumbDown,
}

var callbackCodeForAction = func() map[ApprovalAction]string {
	m := make(map[ApprovalAction]string, len(callbackActionCodes))
	for code, action := range callbackActionCodes {
		m[action] = code
	}
	return m
}()

// CallbackDataEncode encodes an approval action + draftID into a Telegram callback_data string (≤64 bytes).
// Format: "{code}:{draftIDwithoutPrefix}"
func CallbackDataEncode(action ApprovalAction, draftID string) string {
	code, ok := callbackCodeForAction[action]
	if !ok {
		code = "ap"
	}
	bare := strings.TrimPrefix(draftID, "draft:")
	return fmt.Sprintf("%s:%s", code, bare)
}

// CallbackDataDecode parses a Telegram callback_data string back into action + draftID + idempotencyKey.
// For ApprovalChooseOption callbacks use CallbackDataDecodeChoice instead.
func CallbackDataDecode(data string) (action ApprovalAction, draftID string, idempotencyKey string, ok bool) {
	idx := strings.Index(data, ":")
	if idx < 0 {
		return "", "", "", false
	}
	code := data[:idx]
	bare := data[idx+1:]
	action, ok = callbackActionCodes[code]
	if !ok {
		return "", "", "", false
	}
	draftID = "draft:" + bare
	idempotencyKey = draftID + ":" + string(action)
	return action, draftID, idempotencyKey, true
}

// CallbackDataEncodeForget encodes a forget-chat confirmation button (≤64 bytes).
// Format: "fg:{chatID}" (confirm) — the cancel button is plain "fgx".
func CallbackDataEncodeForget(chatID string) string {
	return "fg:" + chatID
}

// CallbackDataDecodeForget parses a forget-chat confirmation callback.
// ok is false for any other callback format.
func CallbackDataDecodeForget(data string) (chatID string, ok bool) {
	if !strings.HasPrefix(data, "fg:") {
		return "", false
	}
	chatID = data[len("fg:"):]
	return chatID, chatID != ""
}

// ForgetCancelCallbackData is the callback payload of the "cancel" button on
// the forget-chat confirmation card.
const ForgetCancelCallbackData = "fgx"

// CallbackDataEncodeSuggestion encodes a profile-suggestion review button
// (≤64 bytes). Format: "ps:a:{id}" (accept) / "ps:r:{id}" (reject).
func CallbackDataEncodeSuggestion(id int64, accept bool) string {
	verdict := "r"
	if accept {
		verdict = "a"
	}
	return fmt.Sprintf("ps:%s:%d", verdict, id)
}

// CallbackDataDecodeSuggestion parses a profile-suggestion review callback.
func CallbackDataDecodeSuggestion(data string) (id int64, accept bool, ok bool) {
	if !strings.HasPrefix(data, "ps:") {
		return 0, false, false
	}
	rest := data[3:]
	if len(rest) < 3 || rest[1] != ':' || (rest[0] != 'a' && rest[0] != 'r') {
		return 0, false, false
	}
	parsed, err := strconv.ParseInt(rest[2:], 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false, false
	}
	return parsed, rest[0] == 'a', true
}

// CallbackDataEncodeChoice encodes a question-option selection into a callback_data string (≤64 bytes).
// Format: "co:{N}:{bareID}" where N is the single-digit option index (0–9).
func CallbackDataEncodeChoice(draftID string, optionIdx int) string {
	bare := strings.TrimPrefix(draftID, "draft:")
	return fmt.Sprintf("co:%d:%s", optionIdx, bare)
}

// CallbackDataDecodeChoice parses a choice callback_data string ("co:{N}:{bareID}").
// Returns false if the data is not in choice format.
func CallbackDataDecodeChoice(data string) (optionIdx int, draftID string, idempotencyKey string, ok bool) {
	if !strings.HasPrefix(data, "co:") {
		return 0, "", "", false
	}
	rest := data[3:] // strip "co:"
	sep := strings.Index(rest, ":")
	if sep < 0 {
		return 0, "", "", false
	}
	n, err := strconv.Atoi(rest[:sep])
	if err != nil || n < 0 || n > 9 {
		return 0, "", "", false
	}
	bare := rest[sep+1:]
	draftID = "draft:" + bare
	idempotencyKey = draftID + ":choose_option:" + strconv.Itoa(n)
	return n, draftID, idempotencyKey, true
}

// CallbackDataEncodeAIButton encodes a contact-facing interaction option.
// The idempotency suffix is stable for the draft, so concurrent presses on
// different options still resolve to one logical answer.
func CallbackDataEncodeAIButton(draftID string, optionIdx int) string {
	bare := strings.TrimPrefix(draftID, "draft:")
	return fmt.Sprintf("aibtn:%s:%d:%s", bare, optionIdx, aiButtonIdem(bare))
}

// CallbackDataDecodeAIButton parses "aibtn:{draftId}:{optionIdx}:{idem}".
// Draft ids may contain colons, so separators are consumed from the right.
func CallbackDataDecodeAIButton(data string) (optionIdx int, draftID string, idempotencyKey string, ok bool) {
	if !strings.HasPrefix(data, "aibtn:") {
		return 0, "", "", false
	}
	rest := strings.TrimPrefix(data, "aibtn:")
	idemSep := strings.LastIndex(rest, ":")
	if idemSep <= 0 {
		return 0, "", "", false
	}
	idem := rest[idemSep+1:]
	rest = rest[:idemSep]
	indexSep := strings.LastIndex(rest, ":")
	if indexSep <= 0 {
		return 0, "", "", false
	}
	bare := rest[:indexSep]
	n, err := strconv.Atoi(rest[indexSep+1:])
	if err != nil || n < 0 || n > 3 || idem != aiButtonIdem(bare) {
		return 0, "", "", false
	}
	draftID = "draft:" + bare
	return n, draftID, draftID + ":aibtn:" + idem, true
}

func aiButtonIdem(value string) string {
	// Small deterministic FNV-1a suffix: compact enough for Telegram's 64-byte
	// callback_data cap and sufficient to reject malformed/stale payloads.
	var hash uint32 = 2166136261
	for index := 0; index < len(value); index++ {
		hash ^= uint32(value[index])
		hash *= 16777619
	}
	return strconv.FormatUint(uint64(hash), 36)
}

// ContactProfile is the owner-supplied description of a contact in a specific chat.
// It maps to the contact_profiles table and covers three concerns:
//  1. Собеседник — objective facts about the contact.
//  2. Я с ним — how the owner communicates with this contact (style).
//  3. Чат — what this conversation is about.
//
// Backwards-compat fields (Name, Notes) are preserved so existing UI code and
// API clients continue to work without changes.
type ContactProfile struct {
	TenantID string `json:"tenant_id"`
	ChatID   string `json:"chat_id"`

	// --- Собеседник (contact identity) ---
	// DisplayNames holds all known names/aliases for the contact.
	// DisplayNames[0] is the primary name; kept in sync with the Name field.
	DisplayNames     []string `json:"display_names,omitempty"`
	PreferredAddress string   `json:"preferred_address,omitempty"`
	// Gender: male, female, non_binary, unknown (or legacy: other, "").
	Gender string `json:"gender,omitempty"`
	// Role is a free-text descriptor (client, colleague, partner, …).
	Role string `json:"role,omitempty"`
	// RelationshipType: family, romantic, friendship, professional, acquaintance, unknown.
	// Legacy values (personal, business) are accepted at the API layer.
	RelationshipType string `json:"relationship_type,omitempty"`
	Language         string `json:"language,omitempty"`
	ContactTimezone  string `json:"contact_timezone,omitempty"`

	// --- Я с ним (communication style for this chat) ---
	// Tone: formal, neutral, casual. NULL means cold-start default by relationship_type.
	Tone string `json:"tone,omitempty"`
	// EmojiUsage: none, rare, frequent.
	EmojiUsage string `json:"emoji_usage,omitempty"`
	// AddressForm: ty (informal), vy (formal).
	AddressForm string `json:"address_form,omitempty"`
	// StyleNotes is a free-text complement to structured style fields.
	StyleNotes string `json:"style_notes,omitempty"`

	// --- Чат (conversation context) ---
	Topics       []string `json:"topics,omitempty"`
	ChatContext  string   `json:"chat_context,omitempty"`
	DoNotDiscuss []string `json:"do_not_discuss,omitempty"`

	// --- Backwards-compat fields ---
	// Name mirrors DisplayNames[0]; kept for existing UI and API consumers.
	Name string `json:"name,omitempty"`
	// Notes is a JSON string "{me_style, chat_context}" used by the current
	// Mini App profile screen. Derived from StyleNotes / ChatContext on read;
	// parsed into those fields on write.
	Notes string `json:"notes,omitempty"`
}

// DisplayName returns the primary display name for the contact.
// It checks DisplayNames first, then falls back to the legacy Name field.
func (p ContactProfile) DisplayName() string {
	if len(p.DisplayNames) > 0 {
		return p.DisplayNames[0]
	}
	return p.Name
}

// ColdStartTone returns the default tone for a contact whose Tone field is empty,
// derived from the relationship type (§4.6 of architecture doc).
func (p ContactProfile) ColdStartTone() string {
	if p.Tone != "" {
		return p.Tone
	}
	switch p.RelationshipType {
	case "family", "romantic", "friendship", "personal":
		return "casual"
	case "professional", "business":
		return "formal"
	default:
		return "neutral"
	}
}

// ColdStartEmojiUsage returns the default emoji-usage level when EmojiUsage is empty.
func (p ContactProfile) ColdStartEmojiUsage() string {
	if p.EmojiUsage != "" {
		return p.EmojiUsage
	}
	switch p.RelationshipType {
	case "romantic", "friendship", "personal":
		return "frequent"
	case "professional", "business":
		return "none"
	default:
		return "rare"
	}
}

// ColdStartAddressForm returns the default address form when AddressForm is empty.
func (p ContactProfile) ColdStartAddressForm() string {
	if p.AddressForm != "" {
		return p.AddressForm
	}
	switch p.RelationshipType {
	case "family", "romantic", "friendship", "personal":
		return "ty"
	default:
		return "vy"
	}
}

// AIRole is the owner-authored persona the AI plays when answering on the
// owner's behalf in a chat ("менеджер", "техподдержка", любой выдуманный
// персонаж).  The AI reproduces this role — not the owner himself.  Resolved
// by the backend from ai_roles via chat_settings.ai_role_id and passed to the
// Python service on every process-message request.
type AIRole struct {
	// Name is the short label the owner gave the role.
	Name string `json:"name"`
	// Description is the free-text persona description — the primary prompt material.
	Description string `json:"description,omitempty"`
	// SpeakingStyle describes how the role talks (lexicon, tone, message length).
	SpeakingStyle string `json:"speaking_style,omitempty"`
	// Goals describes what the role tries to achieve in conversations.
	Goals string `json:"goals,omitempty"`
	// Constraints describes what the role must not do or discuss.
	Constraints string `json:"constraints,omitempty"`
	// Context is owner-supplied reference material for the role (FAQ, pricing,
	// docs — pasted or extracted from files in the Mini App). Injected into the
	// draft prompt so the persona answers with that knowledge.
	Context string `json:"context,omitempty"`
	// CustomFields are owner-defined extra role attributes beyond the fixed
	// fields above. Each carries a title, a "why it exists" description, and
	// optional context; all are injected into the persona prompt.
	CustomFields []AIRoleCustomField `json:"custom_fields,omitempty"`
	// MCPServers are the external MCP toolsets this role may call (#7). Resolved
	// from role_mcp_access + mcp_servers; empty means the role has no tools.
	MCPServers []MCPServerSpec `json:"mcp_servers,omitempty"`
	// SpeakAsOwner controls the AI persona frame. true (default) = impersonate
	// the owner — the contact perceives the reply as written by a human.
	// false = the AI responds as the role itself (assistant / companion
	// scenarios); the reply is clearly from the bot persona, not the owner.
	SpeakAsOwner bool `json:"speak_as_owner"`
	// ModelTier selects the LLM quality tier for this role's draft generation:
	// "fast" (low-latency), "balanced" (default), or "smart" (highest capability).
	// Empty string means fall back to the DRAFT task model (MODEL_DRAFT /
	// OPENROUTER_MODEL), preserving backward compatibility with existing roles.
	ModelTier string `json:"model_tier,omitempty"`
	// ModelName optionally pins a concrete model inside ModelTier. Empty keeps
	// the tier primary; stale values are ignored by Python model routing.
	ModelName string `json:"model_name,omitempty"`
	// ToolSettings contains per-role native-tool overrides. Missing keys inherit
	// the corresponding catalog entry's DefaultEnabled value.
	ToolSettings map[string]bool `json:"tool_settings,omitempty"`
}

// MCPServerSpec is one MCP server a role may call, with its per-role tool
// allow-list. Sent to the (internal) Python AI service, which opens the
// connection. AllowedTools empty = every tool the server exposes; non-empty =
// only those (deny-by-default). AuthHeader is an optional Authorization value.
type MCPServerSpec struct {
	Name         string   `json:"name"`
	Transport    string   `json:"transport"` // "http" (streamable) or "sse"
	URL          string   `json:"url"`
	AuthHeader   string   `json:"auth_header,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// AIRoleCustomField is an owner-defined extra attribute on a role. Title and
// Description are required; Context is optional reference text for that field.
type AIRoleCustomField struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Context     string `json:"context,omitempty"`
}

// UserSettings stores per-owner identity and behaviour preferences (User Layer).
type UserSettings struct {
	UserID             string   `json:"user_id"`
	TenantID           string   `json:"tenant_id"`
	DisplayName        string   `json:"display_name,omitempty"`
	Bio                string   `json:"bio,omitempty"`
	Timezone           string   `json:"timezone,omitempty"`
	PrimaryLanguage    string   `json:"primary_language"`
	SecondaryLanguages []string `json:"secondary_languages"`
	// DefaultModeForNewChats: off, observe, manual, semi_auto, auto.
	DefaultModeForNewChats string `json:"default_mode_for_new_chats"`
	LearningEnabled        bool   `json:"learning_enabled"`
	RichFormattingEnabled  bool   `json:"rich_formatting_enabled"`
	StreamingEnabled       bool   `json:"streaming_enabled"`
	// LanguageForDrafts: auto (detect from contact) or primary (always use PrimaryLanguage).
	LanguageForDrafts string `json:"language_for_drafts"`
}

// SemanticFact is a typed, atomic piece of knowledge about the conversation
// or the owner, stored in the Semantic Layer (semantic_facts table).
type SemanticFact struct {
	ID int64 `json:"id"`
	// ScopeKind is always "chat": memory is strictly per-chat (migration 011
	// retired the owner_global scope).
	ScopeKind string `json:"scope_kind"`
	TenantID  string `json:"tenant_id"`
	ChatID    string `json:"chat_id,omitempty"`
	FactText  string `json:"fact_text"`
	// FactType: preference, biographical, commitment, event, relationship, health, taboo.
	FactType string `json:"fact_type"`
	// Subject: contact, owner, relationship, third_party.
	Subject    string  `json:"subject"`
	Confidence float64 `json:"confidence"`
	// Source: llm_extracted, owner_confirmed, owner_authored.
	Source          string     `json:"source"`
	SourceMessageID string     `json:"source_message_id,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// ActiveEpisode represents the open (not yet summarised) episode for a chat
// (Runtime State Layer, active_episodes table).
type ActiveEpisode struct {
	TenantID           string    `json:"tenant_id"`
	ChatID             string    `json:"chat_id"`
	StartedAt          time.Time `json:"started_at"`
	LastMessageAt      time.Time `json:"last_message_at"`
	MessageCount       int       `json:"message_count"`
	CurrentTopic       string    `json:"current_topic,omitempty"`
	OpenQuestionMsgID  string    `json:"open_question_message_id,omitempty"`
	PendingOwnerAction string    `json:"pending_owner_action,omitempty"`
}

// EpisodeSummary is a compressed record of a closed conversation episode
// (Episodic Layer, episode_summaries table).
type EpisodeSummary struct {
	ID            int64     `json:"id"`
	TenantID      string    `json:"tenant_id"`
	ChatID        string    `json:"chat_id"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	Content       string    `json:"content"`
	ClosureReason string    `json:"closure_reason"`
	MessageCount  int       `json:"message_count"`
	CreatedAt     time.Time `json:"created_at"`
}

// DraftFeedback stores owner feedback on a generated draft.
type DraftFeedback struct {
	DraftID      string    `json:"draft_id"`
	TenantID     string    `json:"tenant_id"`
	ChatID       string    `json:"chat_id"`
	Rating       string    `json:"rating"` // "up", "down"
	FeedbackText string    `json:"feedback_text,omitempty"`
	ResponseText string    `json:"response_text"`
	ModelUsed    string    `json:"model_used"`
	CreatedAt    time.Time `json:"created_at"`
}

// PendingInputType describes what the bot is waiting for from the owner.
type PendingInputType string

const (
	// PendingInputEdit means the bot is waiting for the owner to write the replacement text directly.
	PendingInputEdit PendingInputType = "edit"
	// PendingInputFeedback means the bot is waiting for optional text feedback after a thumb-down.
	PendingInputFeedback PendingInputType = "feedback"
	// PendingInputAIRefine means the bot is waiting for an instruction for the AI to refine the draft.
	PendingInputAIRefine PendingInputType = "ai_refine"
	// PendingInputForgetAll means the bot is waiting for the confirmation phrase
	// before erasing all tenant memory (§10.2 deletion contract).
	PendingInputForgetAll PendingInputType = "forget_all"
)

// ForgetAllConfirmationPhrase must be typed verbatim to confirm /forget_all.
const ForgetAllConfirmationPhrase = "УДАЛИТЬ ВСЁ"

// PendingInput tracks that the bot is waiting for a free-text reply from the owner.
type PendingInput struct {
	TenantID  string           `json:"tenant_id"`
	ChatID    string           `json:"chat_id"`
	Type      PendingInputType `json:"type"`
	DraftID   string           `json:"draft_id"`
	ExpiresAt time.Time        `json:"expires_at"`
}

// Transport values for NormalizedTelegramEvent.Transport.
const (
	// TransportBusiness marks events ingested via the Telegram Business webhook.
	TransportBusiness = "business"
	// TransportBot marks events ingested via a user-supplied bot (long polling).
	TransportBot = "bot"
)

// IngestionMode selects which Telegram channel feeds a tenant's inbound pipeline.
type IngestionMode string

const (
	// IngestionModeBot ingests only via the tenant's user-supplied bot.
	IngestionModeBot IngestionMode = "bot"
	// IngestionModePersonal ingests only via the Telegram Business webhook.
	IngestionModePersonal IngestionMode = "personal"
	// IngestionModeFull keeps both channels active.
	IngestionModeFull IngestionMode = "full"
)

// ParseIngestionMode validates a raw mode string; ok is false for unknown values.
func ParseIngestionMode(raw string) (IngestionMode, bool) {
	switch IngestionMode(raw) {
	case IngestionModeBot, IngestionModePersonal, IngestionModeFull:
		return IngestionMode(raw), true
	}
	return "", false
}

// AllowsBot reports whether bot-transport events may be processed in this mode.
func (m IngestionMode) AllowsBot() bool {
	return m == IngestionModeBot || m == IngestionModeFull
}

// AllowsBusiness reports whether business-transport events may be processed in this mode.
func (m IngestionMode) AllowsBusiness() bool {
	return m == IngestionModePersonal || m == IngestionModeFull
}

// NormalizedTelegramEvent is the canonical inbound Telegram event.
type NormalizedTelegramEvent struct {
	EventID              string    `json:"event_id"`
	TenantID             string    `json:"tenant_id"`
	ChatID               string    `json:"chat_id"`
	ChatDisplayName      string    `json:"chat_display_name,omitempty"`
	ChatType             string    `json:"chat_type,omitempty"`
	TelegramUpdateID     int64     `json:"telegram_update_id"`
	BusinessConnectionID string    `json:"business_connection_id"`
	SourceMessageID      string    `json:"source_message_id"`
	SenderID             string    `json:"sender_id"`
	MessageText          string    `json:"message_text"`
	MessageType          string    `json:"message_type"`
	CreatedAt            time.Time `json:"created_at"`
	// Transport identifies the ingestion channel ("business" or "bot").
	// Empty means "business" so events stored before the field existed keep working.
	Transport string `json:"transport,omitempty"`
	// BotID is the Telegram bot identifier; set only when Transport == "bot".
	BotID string `json:"bot_id,omitempty"`
}

// ReceiveIdempotencyKey returns the contract receive idempotency key.
// update_id sequences are independent per source: business webhooks scope the
// key by connection id, bot polling scopes it by bot id.
func (e NormalizedTelegramEvent) ReceiveIdempotencyKey() string {
	if e.Transport == TransportBot {
		return formatInt64(e.TelegramUpdateID) + ":bot:" + e.BotID
	}
	return formatInt64(e.TelegramUpdateID) + ":" + e.BusinessConnectionID
}

// ChatSettings stores per-chat autonomy and privacy settings.
type ChatSettings struct {
	TenantID     string       `json:"tenant_id"`
	ChatID       string       `json:"chat_id"`
	Mode         ChatMode     `json:"mode"`
	PrivacyFlags PrivacyFlags `json:"privacy_flags"`
}

// WithDefaults returns settings with a conservative default mode.
func (s ChatSettings) WithDefaults() ChatSettings {
	if s.Mode == "" {
		s.Mode = ChatModeManual
	}
	if s.PrivacyFlags == nil {
		s.PrivacyFlags = PrivacyFlags{}
	}
	return s
}

// FeedbackItem carries one owner rating on a past draft for inclusion in the AI context.
type FeedbackItem struct {
	Rating       string `json:"rating"` // "up" or "down"
	ResponseText string `json:"response_text"`
	FeedbackText string `json:"feedback_text,omitempty"`
}

// AIProcessMessageRequest is sent to the Python AI service.
type AIProcessMessageRequest struct {
	RequestID string `json:"request_id"`
	// TraceID is the deterministic W3C trace-id derived from Event.EventID.
	// Propagated to Python via the traceparent header and persisted on every
	// state/audit row produced for this webhook so all spans correlate.
	TraceID      string                  `json:"trace_id,omitempty"`
	Event        NormalizedTelegramEvent `json:"event"`
	ChatSettings ChatSettings            `json:"chat_settings"`
	// MemoryContext is the assembled prompt context (Working + Episodic + Semantic layers).
	MemoryContext   []string        `json:"memory_context"`
	FeedbackContext []FeedbackItem  `json:"feedback_context,omitempty"`
	ContactProfile  *ContactProfile `json:"contact_profile,omitempty"`
	// UserSettings provides the owner's identity and preferences to the AI.
	UserSettings *UserSettings `json:"user_settings,omitempty"`
	// ActiveEpisode gives the AI awareness of the current open conversation episode.
	ActiveEpisode *ActiveEpisode `json:"active_episode,omitempty"`
	// AIRole is the persona assigned to this chat; nil means the default
	// assistant persona (reply in the owner's observed style).
	AIRole *AIRole `json:"ai_role,omitempty"`
}

// TokenUsage contains model input and output token counts.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ResponsePlan marks Options as a deferred contact-facing response form.
type ResponsePlan struct {
	Shape   string   `json:"shape"`
	Options []string `json:"options"`
}

// ScheduledJob is a deferred job produced by the Python scheduler tool.
// Go owns persistence and dispatch through store.ScheduledJobRecord.
type ScheduledJob struct {
	Kind           string         `json:"kind"`
	Target         string         `json:"target"`
	FireAt         time.Time      `json:"fire_at"`
	Payload        map[string]any `json:"payload"`
	CreatedBy      string         `json:"created_by,omitempty"`
	IdempotencyKey string         `json:"idempotency_key"`
}

// AIProcessMessageResponse is returned by the Python AI service.
type AIProcessMessageResponse struct {
	Decision     Decision   `json:"decision"`
	ResponseText string     `json:"response_text"`
	Confidence   float64    `json:"confidence"`
	RiskLevel    RiskLevel  `json:"risk_level"`
	Intent       string     `json:"intent"`
	ModelUsed    string     `json:"model_used"`
	Usage        TokenUsage `json:"usage"`
	TextFormat   TextFormat `json:"text_format,omitempty"`
	MemoryUsed   []string   `json:"memory_used"`
	// Flagged is set by the Phase 3 output-safety layer when a draft contains
	// toxicity, sensitive content, or schema-invalid output the repair attempt
	// could not salvage.  Drafts are still returned; the bot UI annotates them.
	Flagged bool `json:"flagged,omitempty"`
	// OwnerNote is an owner-facing service note (e.g. the critical-risk warning).
	// It is rendered on the owner's card only and must NEVER be sent to the
	// contact; ResponseText stays empty when this is set (ARCH-1).
	OwnerNote string `json:"owner_note,omitempty"`
	// Options are AI-suggested short replies for Intent == "choice_question".
	Options []string `json:"options,omitempty"`
	// ResponsePlan is set only for interaction-tool output. Ordinary
	// choice_question options remain an owner-facing card.
	ResponsePlan *ResponsePlan `json:"response_plan,omitempty"`
	// ScheduledJobs are validated deferred jobs from AI tools. The backend
	// persists them through the repository so Python remains stateless by default.
	ScheduledJobs []ScheduledJob `json:"scheduled_jobs,omitempty"`
	// Topic is a 3-7 word label of the current conversation produced by the
	// same draft call (replaces the per-message /ai/tag-topic call — ARCH-4).
	Topic string `json:"topic,omitempty"`
}

// AuditEvent records the policy outcome for an inbound message.
type AuditEvent struct {
	AuditID         string   `json:"audit_id"`
	TenantID        string   `json:"tenant_id"`
	ChatID          string   `json:"chat_id"`
	SourceMessageID string   `json:"source_message_id"`
	Decision        Decision `json:"decision"`
	Reason          string   `json:"reason"`
	// TraceID is the W3C trace-id this audit row belongs to.  Joins audit
	// rows back to Langfuse spans and message_states for the same event.
	TraceID   string    `json:"trace_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ApprovalCallbackPayload is the callback payload sent by owner approval controls.
type ApprovalCallbackPayload struct {
	Action          ApprovalAction `json:"action"`
	TenantID        string         `json:"tenant_id"`
	ChatID          string         `json:"chat_id"`
	SourceMessageID string         `json:"source_message_id"`
	DraftID         string         `json:"draft_id"`
	DecisionID      string         `json:"decision_id"`
	IdempotencyKey  string         `json:"idempotency_key"`
	EditedText      string         `json:"edited_text,omitempty"`
}

// ChatSummaryRequest is sent to the Python AI service to summarize a chat.
type ChatSummaryRequest struct {
	Messages     []string     `json:"messages"`
	PrivacyFlags PrivacyFlags `json:"privacy_flags"`
	TraceID      string       `json:"trace_id,omitempty"`
}

// ChatSummaryResponse is returned by the Python AI service.
type ChatSummaryResponse struct {
	Summary     string   `json:"summary"`
	ActionItems []string `json:"action_items"`
	// Recommendations are proactive suggestions for the owner based on the contact's interests
	// and conversation history. Populated by the Python AI service when relevant.
	Recommendations  []string   `json:"recommendations,omitempty"`
	ModelUsed        string     `json:"model_used"`
	Usage            TokenUsage `json:"usage"`
	BlockedByPrivacy bool       `json:"blocked_by_privacy"`
}

// SynthesizePatternRequest asks the AI service to condense a cluster of related
// facts into one durable behaviour pattern (sleep-time reflection, #5).
type SynthesizePatternRequest struct {
	ClusterFacts []string     `json:"cluster_facts"`
	TenantID     string       `json:"tenant_id"`
	ChatID       string       `json:"chat_id"`
	PrivacyFlags PrivacyFlags `json:"privacy_flags"`
	TraceID      string       `json:"trace_id,omitempty"`
}

// SynthesizePatternResponse carries the synthesised pattern. An empty Pattern
// means the cluster held no durable signal and nothing should be written.
type SynthesizePatternResponse struct {
	Pattern          string     `json:"pattern"`
	Subject          string     `json:"subject"`
	Confidence       float64    `json:"confidence"`
	ModelUsed        string     `json:"model_used"`
	Usage            TokenUsage `json:"usage"`
	BlockedByPrivacy bool       `json:"blocked_by_privacy"`
}

// ContextScore computes a 0–100 richness score for a chat based on available knowledge.
// It is used to determine which autonomy modes are unlocked for this chat.
// Scoring accounts for the richer ContactProfile schema: style fields add additional weight.
func ContextScore[F any, FB any](profile ContactProfile, facts []F, feedback []FB) int {
	score := 0
	if profile.Gender != "" {
		score += 10
	}
	if profile.Role != "" {
		score += 15
	}
	if profile.RelationshipType != "" {
		score += 15
	}
	if profile.DisplayName() != "" {
		score += 10
	}
	// Style fields signal the owner has personalised this contact's profile.
	if profile.Tone != "" || profile.EmojiUsage != "" || profile.AddressForm != "" {
		score += 10
	}
	if len(facts) >= 3 {
		score += 20
	} else if len(facts) > 0 {
		score += 5
	}
	if len(feedback) > 0 {
		score += 10
	}
	return score
}

// UnlockedModesForScore returns the chat modes available at a given context score.
// disabled, silent, and manual are always available; semi_auto requires ≥50; auto requires ≥80.
func UnlockedModesForScore(score int) []ChatMode {
	modes := []ChatMode{ChatModeDisabled, ChatModeSilent, ChatModeManual}
	if score >= 50 {
		modes = append(modes, ChatModeSemiAuto)
	}
	if score >= 80 {
		modes = append(modes, ChatModeAuto)
	}
	return modes
}

// SendIdempotencyKey returns the contract send idempotency key.
func SendIdempotencyKey(tenantID, chatID, sourceMessageID, decisionID string) string {
	return tenantID + ":" + chatID + ":" + sourceMessageID + ":" + decisionID
}

func formatInt64(v int64) string {
	return strconv.FormatInt(v, 10)
}
