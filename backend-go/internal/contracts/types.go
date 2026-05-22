// Package contracts contains Go representations of the frozen cross-service contracts.
package contracts

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Decision is the final or proposed action for an inbound message.
type Decision string

const (
	// DecisionDraft means a response should be prepared for owner approval.
	DecisionDraft Decision = "draft"
	// DecisionAutoReply means the response may be sent to the recipient.
	DecisionAutoReply Decision = "auto_reply"
	// DecisionIgnore means no response or owner draft should be produced.
	DecisionIgnore Decision = "ignore"
	// DecisionObserve means the message is observed without creating a reply.
	DecisionObserve Decision = "observe"
	// DecisionAskOwner means the owner must make the decision manually.
	DecisionAskOwner Decision = "ask_owner"
	// DecisionErrorFallback means processing failed and a safe fallback is required.
	DecisionErrorFallback Decision = "error_fallback"
)

// IsValid reports whether the decision is part of the frozen contract.
func (d Decision) IsValid() bool {
	switch d {
	case DecisionDraft, DecisionAutoReply, DecisionIgnore, DecisionObserve, DecisionAskOwner, DecisionErrorFallback:
		return true
	default:
		return false
	}
}

// ChatMode controls the autonomy level for one tenant/chat pair.
type ChatMode string

const (
	// ChatModeDisabled blocks all AI processing for the chat.
	ChatModeDisabled ChatMode = "disabled"
	// ChatModeSilent records and observes without replying.
	ChatModeSilent ChatMode = "silent"
	// ChatModeManual always requires owner approval before a reply.
	ChatModeManual ChatMode = "manual"
	// ChatModeSemiAuto allows low-risk auto replies above the semi-auto threshold.
	ChatModeSemiAuto ChatMode = "semi_auto"
	// ChatModeAuto allows low-risk auto replies above the auto threshold.
	ChatModeAuto ChatMode = "auto"
)

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
type PrivacyFlag string

const (
	// PrivacyNoMemory prevents persisted message memory beyond transient processing.
	PrivacyNoMemory PrivacyFlag = "no_memory"
	// PrivacyNoTraining prevents use of chat data for training.
	PrivacyNoTraining PrivacyFlag = "no_training"
	// PrivacyPrivate marks the chat private and disables autonomous replies.
	PrivacyPrivate PrivacyFlag = "private"
	// PrivacyNoSummary prevents summary generation.
	PrivacyNoSummary PrivacyFlag = "no_summary"
)

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
type RiskLevel string

const (
	// RiskLow is safe enough for autonomous handling when mode and privacy allow it.
	RiskLow RiskLevel = "low"
	// RiskMedium defaults to a draft.
	RiskMedium RiskLevel = "medium"
	// RiskHigh is draft-only.
	RiskHigh RiskLevel = "high"
	// RiskCritical must be ignored or escalated to the owner.
	RiskCritical RiskLevel = "critical"
)

// IsValid reports whether the risk level is part of the frozen contract.
func (r RiskLevel) IsValid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	default:
		return false
	}
}

// MessageState is an auditable lifecycle state for an inbound message.
type MessageState string

const (
	// StateReceived means the webhook reached the backend.
	StateReceived MessageState = "received"
	// StateNormalized means the Telegram update was normalized.
	StateNormalized MessageState = "normalized"
	// StatePolicyChecked means chat mode and privacy policy were evaluated.
	StatePolicyChecked MessageState = "policy_checked"
	// StateAIRequested means the Python AI client was called.
	StateAIRequested MessageState = "ai_requested"
	// StateAIGenerated means the Python AI client returned a proposal.
	StateAIGenerated MessageState = "ai_generated"
	// StateDecisionMade means the backend final decision engine completed.
	StateDecisionMade MessageState = "decision_made"
	// StateDraftSentToOwner means the owner-facing draft was created.
	StateDraftSentToOwner MessageState = "draft_sent_to_owner"
	// StateApprovedByOwner means an approval callback accepted a draft.
	StateApprovedByOwner MessageState = "approved_by_owner"
	// StateSentToRecipient means the reply was sent to the recipient.
	StateSentToRecipient MessageState = "sent_to_recipient"
	// StateIgnored means the message or draft was intentionally ignored.
	StateIgnored MessageState = "ignored"
	// StateFailed means processing failed.
	StateFailed MessageState = "failed"
)

// ApprovalAction is the owner callback action.
type ApprovalAction string

const (
	// ApprovalApprove sends the draft to the recipient.
	ApprovalApprove ApprovalAction = "approve"
	// ApprovalReject rejects the draft.
	ApprovalReject ApprovalAction = "reject"
	// ApprovalRegenerate asks for a new draft.
	ApprovalRegenerate ApprovalAction = "regenerate"
	// ApprovalEdit replaces the draft text with owner-edited text.
	ApprovalEdit ApprovalAction = "edit"
	// ApprovalMakeShorter revises the draft to be shorter.
	ApprovalMakeShorter ApprovalAction = "make_shorter"
	// ApprovalMakeMoreFormal revises the draft to be more formal.
	ApprovalMakeMoreFormal ApprovalAction = "make_more_formal"
	// ApprovalNoMemory enables no-memory mode for the chat.
	ApprovalNoMemory ApprovalAction = "no_memory"
	// ApprovalThumbUp records positive feedback on a draft.
	ApprovalThumbUp ApprovalAction = "thumb_up"
	// ApprovalThumbDown records negative feedback and prompts text feedback.
	ApprovalThumbDown ApprovalAction = "thumb_down"
	// ApprovalEditRequest puts the bot into "waiting for direct text replacement" state.
	ApprovalEditRequest ApprovalAction = "edit_request"
	// ApprovalAIRefine puts the bot into "waiting for AI refinement instruction" state.
	ApprovalAIRefine ApprovalAction = "ai_refine"
)

// IsValid reports whether the approval action is part of the frozen contract.
func (a ApprovalAction) IsValid() bool {
	switch a {
	case ApprovalApprove, ApprovalReject, ApprovalRegenerate, ApprovalEdit,
		ApprovalMakeShorter, ApprovalMakeMoreFormal, ApprovalNoMemory,
		ApprovalThumbUp, ApprovalThumbDown, ApprovalEditRequest, ApprovalAIRefine:
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

// ContactProfile is the owner-supplied description of a contact in a specific chat.
type ContactProfile struct {
	TenantID         string `json:"tenant_id"`
	ChatID           string `json:"chat_id"`
	Name             string `json:"name,omitempty"`
	Gender           string `json:"gender,omitempty"`           // "male", "female", "other", ""
	Role             string `json:"role,omitempty"`             // "friend", "colleague", "client", "partner", "family"
	RelationshipType string `json:"relationship_type,omitempty"` // "formal", "informal", "business"
	Notes            string `json:"notes,omitempty"`
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
)

// PendingInput tracks that the bot is waiting for a free-text reply from the owner.
type PendingInput struct {
	TenantID  string           `json:"tenant_id"`
	ChatID    string           `json:"chat_id"`
	Type      PendingInputType `json:"type"`
	DraftID   string           `json:"draft_id"`
	ExpiresAt time.Time        `json:"expires_at"`
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
}

// ReceiveIdempotencyKey returns the contract receive idempotency key.
func (e NormalizedTelegramEvent) ReceiveIdempotencyKey() string {
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
	Rating       string `json:"rating"`        // "up" or "down"
	ResponseText string `json:"response_text"`
	FeedbackText string `json:"feedback_text,omitempty"`
}

// AIProcessMessageRequest is sent to the Python AI service.
type AIProcessMessageRequest struct {
	RequestID       string                  `json:"request_id"`
	Event           NormalizedTelegramEvent `json:"event"`
	ChatSettings    ChatSettings            `json:"chat_settings"`
	MemoryContext   []string                `json:"memory_context"`
	FeedbackContext []FeedbackItem          `json:"feedback_context,omitempty"`
	ContactProfile  *ContactProfile         `json:"contact_profile,omitempty"`
}

// TokenUsage contains model input and output token counts.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
	MemoryUsed   []string   `json:"memory_used"`
}

// AuditEvent records the policy outcome for an inbound message.
type AuditEvent struct {
	AuditID         string    `json:"audit_id"`
	TenantID        string    `json:"tenant_id"`
	ChatID          string    `json:"chat_id"`
	SourceMessageID string    `json:"source_message_id"`
	Decision        Decision  `json:"decision"`
	Reason          string    `json:"reason"`
	CreatedAt       time.Time `json:"created_at"`
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
}

// ChatSummaryResponse is returned by the Python AI service.
type ChatSummaryResponse struct {
	Summary          string     `json:"summary"`
	ActionItems      []string   `json:"action_items"`
	ModelUsed        string     `json:"model_used"`
	Usage            TokenUsage `json:"usage"`
	BlockedByPrivacy bool       `json:"blocked_by_privacy"`
}

// ContextScore computes a 0–100 richness score for a chat based on available knowledge.
// It is used to determine which autonomy modes are unlocked for this chat.
func ContextScore[F any, FB any](profile ContactProfile, facts []F, feedback []FB) int {
	score := 0
	if profile.Gender != "" {
		score += 15
	}
	if profile.Role != "" {
		score += 20
	}
	if profile.RelationshipType != "" {
		score += 15
	}
	if profile.Name != "" {
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
// silent and manual are always available; semi_auto requires ≥50; auto requires ≥80.
func UnlockedModesForScore(score int) []ChatMode {
	modes := []ChatMode{ChatModeSilent, ChatModeManual}
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

// SemiAutoMinConfidence is the minimum confidence for semi-auto replies.
const SemiAutoMinConfidence = 0.85

// AutoMinConfidence is the minimum confidence for auto replies.
const AutoMinConfidence = 0.92

func formatInt64(v int64) string {
	return strconv.FormatInt(v, 10)
}
