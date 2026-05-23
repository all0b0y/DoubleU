// Package core contains backend business workflows and final policy decisions.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/bot"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// DecisionEngine applies backend-owned policy to AI proposals.
type DecisionEngine struct{}

// NewDecisionEngine creates a final decision engine.
func NewDecisionEngine() DecisionEngine {
	return DecisionEngine{}
}

// Decide returns the backend-authoritative decision for an AI proposal.
func (DecisionEngine) Decide(settings contracts.ChatSettings, proposal contracts.AIProcessMessageResponse) (contracts.Decision, string) {
	settings = settings.WithDefaults()
	if !settings.Mode.IsValid() {
		return contracts.DecisionErrorFallback, "invalid_chat_mode"
	}
	if !settings.PrivacyFlags.IsValid() {
		return contracts.DecisionErrorFallback, "invalid_privacy_flags"
	}
	if !proposal.RiskLevel.IsValid() {
		return contracts.DecisionDraft, "invalid_risk_level"
	}
	if proposal.Decision == contracts.DecisionErrorFallback {
		return contracts.DecisionErrorFallback, "ai_error_fallback"
	}
	if proposal.Decision == contracts.DecisionIgnore {
		return contracts.DecisionIgnore, "ai_requested_ignore"
	}
	if settings.Mode == contracts.ChatModeDisabled {
		return contracts.DecisionIgnore, "chat_disabled"
	}
	if settings.Mode == contracts.ChatModeSilent {
		return contracts.DecisionObserve, "chat_silent"
	}
	if proposal.RiskLevel == contracts.RiskCritical {
		if strings.TrimSpace(proposal.ResponseText) == "" {
			return contracts.DecisionIgnore, "critical_risk_no_safe_draft"
		}
		return contracts.DecisionAskOwner, "critical_risk_owner_required"
	}
	if proposal.RiskLevel == contracts.RiskHigh {
		return contracts.DecisionDraft, "high_risk_draft_only"
	}
	if proposal.RiskLevel == contracts.RiskMedium {
		return contracts.DecisionDraft, "medium_risk_draft_default"
	}
	if settings.Mode == contracts.ChatModeManual {
		return contracts.DecisionDraft, "manual_mode_owner_required"
	}
	if settings.PrivacyFlags.Contains(contracts.PrivacyPrivate) {
		return contracts.DecisionDraft, "private_chat_blocks_auto_reply"
	}
	if proposal.Decision != contracts.DecisionAutoReply {
		return contracts.DecisionDraft, "ai_did_not_request_auto_reply"
	}
	switch settings.Mode {
	case contracts.ChatModeSemiAuto:
		if proposal.Confidence >= contracts.SemiAutoMinConfidence {
			return contracts.DecisionAutoReply, "semi_auto_confidence_met"
		}
		return contracts.DecisionDraft, "semi_auto_confidence_below_threshold"
	case contracts.ChatModeAuto:
		if proposal.Confidence >= contracts.AutoMinConfidence {
			return contracts.DecisionAutoReply, "auto_confidence_met"
		}
		return contracts.DecisionDraft, "auto_confidence_below_threshold"
	default:
		return contracts.DecisionDraft, "safe_default_draft"
	}
}

// Service coordinates Telegram inbound handling, AI calls, audit, and approvals.
type Service struct {
	repository *store.Repository
	ai         aiclient.Client
	sender     telegram.Sender
	botLayer   *bot.Layer
	engine     DecisionEngine
	logger     *slog.Logger
	now        func() time.Time
	debounce   sync.Map      // map[chatKey]string — latest event_id per chat for typing-delay debounce
	replyDelay time.Duration // wait before calling AI (lets users finish sending multi-message bursts)
	// notify pushes real-time events to connected Mini App clients.
	// When set, bot draft cards are suppressed — the Mini App is the primary UI.
	notify func(tenantID, eventType string, payload any)
}

// Option configures a Service.
type Option func(*Service)

// WithReplyDelay configures how long the service waits before calling the AI,
// giving users time to finish sending multi-message bursts before a draft is created.
func WithReplyDelay(d time.Duration) Option {
	return func(service *Service) {
		service.replyDelay = d
	}
}

// WithClock configures the service clock.
func WithClock(clock func() time.Time) Option {
	return func(service *Service) {
		service.now = clock
	}
}

// WithLogger configures the structured backend logger.
func WithLogger(logger *slog.Logger) Option {
	return func(service *Service) {
		if logger != nil {
			service.logger = logger
		}
	}
}

// WithNotifier configures a real-time notification callback for the Mini App.
// When set, new draft cards are NOT sent via bot — the Mini App is the primary UI.
func WithNotifier(notify func(tenantID, eventType string, payload any)) Option {
	return func(service *Service) {
		service.notify = notify
	}
}

// NewService creates a backend core service.
func NewService(repository *store.Repository, ai aiclient.Client, sender telegram.Sender, options ...Option) *Service {
	if repository == nil {
		repository = store.NewRepository()
	}
	if sender == nil {
		sender = telegram.NoopSender{}
	}
	service := &Service{
		repository: repository,
		ai:         ai,
		sender:     sender,
		botLayer:   bot.NewLayer(repository),
		engine:     NewDecisionEngine(),
		logger:     slog.Default(),
		now:        time.Now,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// InboundResult describes the result of one Telegram webhook.
type InboundResult struct {
	Event     contracts.NormalizedTelegramEvent `json:"event"`
	Decision  contracts.Decision                `json:"decision"`
	Reason    string                            `json:"reason"`
	Duplicate bool                              `json:"duplicate"`
	DraftID   string                            `json:"draft_id,omitempty"`
}

// ApprovalResult describes the result of one owner callback.
type ApprovalResult struct {
	Action    contracts.ApprovalAction `json:"action"`
	Duplicate bool                     `json:"duplicate"`
	DraftID   string                   `json:"draft_id,omitempty"`
	Status    string                   `json:"status"`
}

// HandleTelegramWebhook normalizes and processes a Telegram webhook payload.
func (s *Service) HandleTelegramWebhook(ctx context.Context, payload []byte, tenantHint string) (InboundResult, error) {
	s.logger.Info("telegram webhook received",
		"tenant_hint", tenantHint,
		"payload_size_bytes", len(payload),
		"update_summary", telegramUpdateSummary(payload),
	)
	if connection, ok, err := telegram.ParseBusinessConnectionUpdate(payload, s.now()); err != nil {
		s.logger.Error("telegram business connection parse failed", "error", err)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "business_connection_parse_error:" + err.Error()}, err
	} else if ok {
		return s.handleBusinessConnection(ctx, connection), nil
	}
	if cbq, ok, err := telegram.ParseCallbackQueryUpdate(payload); err != nil {
		s.logger.Error("telegram callback query parse failed", "error", err)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "callback_query_parse_error:" + err.Error()}, err
	} else if ok {
		return s.handleCallbackQuery(ctx, cbq), nil
	}
	if command, ok, err := telegram.ParseBotCommandUpdate(payload); err != nil {
		s.logger.Error("telegram command parse failed", "error", err)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "bot_command_parse_error:" + err.Error()}, err
	} else if ok {
		return s.handleBotCommand(ctx, command), nil
	}
	if ownerMsg, ok, err := telegram.ParseOwnerMessageUpdate(payload); err == nil && ok {
		return s.handleOwnerMessage(ctx, ownerMsg), nil
	}

	event, err := telegram.NormalizeUpdate(payload, tenantHint, s.now())
	if err != nil {
		audit := s.auditEvent(contracts.NormalizedTelegramEvent{
			TenantID:        valueOrDefault(tenantHint, "unknown"),
			ChatID:          "unknown",
			SourceMessageID: "unknown",
		}, contracts.DecisionErrorFallback, "normalization_error:"+err.Error())
		s.repository.AppendAudit(audit)
		s.logger.Error("telegram business message normalization failed", "error", err, "tenant_hint", tenantHint)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: audit.Reason}, err
	}
	if tenantHint == "" {
		if connection, ok := s.repository.BusinessConnection(event.BusinessConnectionID); ok {
			event.TenantID = connection.TenantID
		} else if tenantID, ok := s.repository.TenantForBusinessConnection(event.BusinessConnectionID); ok {
			event.TenantID = tenantID
		}
	}
	connection, hasConnection := s.repository.BusinessConnection(event.BusinessConnectionID)
	ownerID := ""
	if hasConnection {
		ownerID = connection.UserID
	}
	if event.ChatDisplayName == "" {
		event.ChatDisplayName = anonymousChatName(event.ChatID)
	}

	// Gate: only process chats the owner has explicitly added.
	// New contacts go to a staging tenant so they appear in "Предложения" without AI handling.
	if event.BusinessConnectionID != "" && !s.repository.IsChatClaimed(event.TenantID, event.ChatID) {
		stagingTenant := "staging:" + event.BusinessConnectionID
		s.repository.SaveChat(store.ChatRecord{
			TenantID:             stagingTenant,
			ChatID:               event.ChatID,
			DisplayName:          event.ChatDisplayName,
			ChatType:             event.ChatType,
			BusinessConnectionID: event.BusinessConnectionID,
			LastSourceMessageID:  event.SourceMessageID,
			LastSeenAt:           s.now(),
		})
		s.logger.Info("unclaimed business chat staged for owner review",
			"chat_id", event.ChatID,
			"tenant_id", event.TenantID,
			"staging_tenant", stagingTenant,
		)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "chat_not_claimed"}, nil
	}

	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateReceived)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateNormalized)
	settings := s.repository.ChatSettings(event.TenantID, event.ChatID)
	s.repository.SaveChat(store.ChatRecord{
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		DisplayName:          event.ChatDisplayName,
		ChatType:             event.ChatType,
		BusinessConnectionID: event.BusinessConnectionID,
		LastSourceMessageID:  event.SourceMessageID,
		LastSeenAt:           s.now(),
	})
	s.logger.Info("telegram chat observed and stored",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"chat_name", event.ChatDisplayName,
		"chat_type", event.ChatType,
		"business_connection_id", event.BusinessConnectionID,
		"source_message_id", event.SourceMessageID,
	)
	s.logger.Info("telegram business message received",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"chat_name", event.ChatDisplayName,
		"business_connection_id", event.BusinessConnectionID,
		"source_message_id", event.SourceMessageID,
	)
	if duplicate := s.repository.MarkReceive(event.ReceiveIdempotencyKey()); duplicate {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StatePolicyChecked)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateIgnored)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "duplicate_receive"))
		s.logger.Info("telegram duplicate update ignored", "tenant_id", event.TenantID, "chat_id", event.ChatID, "source_message_id", event.SourceMessageID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_receive", Duplicate: true}, nil
	}

	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		s.repository.SaveMessage(event)
	}
	s.repository.IncrementChatMessageCount(event.TenantID, event.ChatID)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StatePolicyChecked)
	if isOwnerAuthoredMessage(event, ownerID) {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "owner_message_observed"))
		s.logger.Info("owner-authored business message stored as context without ai reply",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"source_message_id", event.SourceMessageID,
			"sender_id", event.SenderID,
		)
		// Periodically extract owner writing style from their messages.
		// Fire-and-forget: style facts improve future drafts but are not critical for this response.
		go s.extractOwnerStyleIfNeeded(event)
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_observed"}, nil
	}

	if hasConnection && !connection.CanReply {
		draft := s.createDraft(event, event.EventID, "Telegram не дал боту право отвечать от имени владельца. Включи reply permission в Telegram Settings -> Chat Automation или ответь вручную.")
		s.repository.SaveDraft(draft)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionAskOwner, "business_connection_cannot_reply"))
		s.logger.Info("ai analysis blocked by telegram reply permission", "tenant_id", event.TenantID, "chat_id", event.ChatID, "can_reply", connection.CanReply)
		return InboundResult{Event: event, Decision: contracts.DecisionAskOwner, Reason: "business_connection_cannot_reply", DraftID: draft.DraftID}, nil
	}

	if settings.Mode == contracts.ChatModeDisabled {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateIgnored)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "chat_disabled"))
		s.logger.Info("ai skipped disabled chat", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "chat_disabled"}, nil
	}
	if settings.Mode == contracts.ChatModeSilent {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "chat_silent"))
		s.logger.Info("ai observing silent chat without reply", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "chat_silent"}, nil
	}
	if s.ai == nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "ai_client_unavailable"))
		s.logger.Error("ai client unavailable", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "ai_client_unavailable"}, nil
	}

	// Typing delay: pause before calling the AI so that any follow-up messages the user sends
	// in the same burst are already saved to context by the time we generate a draft.
	// Every message still gets its own draft — we do NOT skip any messages.
	if s.replyDelay > 0 {
		select {
		case <-time.After(s.replyDelay):
		case <-ctx.Done():
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "context_cancelled"}, ctx.Err()
		}
	}

	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIRequested)
	s.logger.Info("ai analysis started", "tenant_id", event.TenantID, "chat_id", event.ChatID, "mode", settings.Mode)
	request := contracts.AIProcessMessageRequest{
		RequestID:       event.EventID,
		Event:           event,
		ChatSettings:    settings,
		MemoryContext:   s.conversationContext(event, ownerID, 20),
		FeedbackContext: s.feedbackContext(event.TenantID, event.ChatID, 5),
	}
	if profile, ok := s.repository.ContactProfile(event.TenantID, event.ChatID); ok {
		request.ContactProfile = &profile
	}
	s.logger.Info("ai process-message request prepared",
		"request_id", request.RequestID,
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"source_message_id", event.SourceMessageID,
		"message_type", event.MessageType,
		"message_text", truncateForLog(event.MessageText, 1000),
		"mode", settings.Mode,
		"privacy_flags", settings.PrivacyFlags,
		"memory_context_count", len(request.MemoryContext),
	)
	proposal, err := s.ai.ProcessMessage(ctx, request)
	if err != nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "ai_process_error:"+err.Error()))
		s.logger.Error("ai analysis failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "ai_process_error"}, nil
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIGenerated)
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		s.saveExtractedFacts(event, proposal.MemoryUsed)
	}
	s.logger.Info("ai analysis completed",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"proposed_decision", proposal.Decision,
		"risk_level", proposal.RiskLevel,
		"confidence", proposal.Confidence,
		"model_used", proposal.ModelUsed,
		"intent", proposal.Intent,
		"response_text", truncateForLog(proposal.ResponseText, 1000),
		"usage_input_tokens", proposal.Usage.InputTokens,
		"usage_output_tokens", proposal.Usage.OutputTokens,
		"memory_used_count", len(proposal.MemoryUsed),
	)

	decision, reason := s.engine.Decide(settings, proposal)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDecisionMade)
	s.logger.Info("backend final decision made", "tenant_id", event.TenantID, "chat_id", event.ChatID, "decision", decision, "reason", reason)
	result := InboundResult{Event: event, Decision: decision, Reason: reason}
	switch decision {
	case contracts.DecisionAutoReply:
		if _, err := s.sendRecipient(ctx, event, event.EventID, proposal.ResponseText); err != nil {
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
			s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "send_error:"+err.Error()))
			s.logger.Error("telegram send failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "send_error"}, nil
		}
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateSentToRecipient)
		s.logger.Info("telegram auto reply sent", "tenant_id", event.TenantID, "chat_id", event.ChatID, "source_message_id", event.SourceMessageID)
	case contracts.DecisionDraft, contracts.DecisionAskOwner:
		draft := s.createDraft(event, event.EventID, proposal.ResponseText)
		s.repository.SaveDraft(draft)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
		s.logger.Info("owner draft prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID, "decision", decision)
		result.DraftID = draft.DraftID
		if s.notify != nil {
			s.notify(draft.TenantID, "new_draft", map[string]any{
				"chat_id":       draft.ChatID,
				"draft_id":      draft.DraftID,
				"response_text": draft.ResponseText,
				"source_text":   draft.SourceMessageText,
				"created_at":    draft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			})
		} else {
			s.sendDraftToOwner(ctx, event, draft, ownerID)
		}
	case contracts.DecisionIgnore:
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateIgnored)
	case contracts.DecisionErrorFallback:
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
	}
	s.repository.AppendAudit(s.auditEvent(event, decision, reason))
	return result, nil
}

func telegramUpdateSummary(payload []byte) map[string]any {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return map[string]any{"parse_error": err.Error()}
	}
	summary := make(map[string]any)
	if updateID, ok := raw["update_id"]; ok {
		var id int64
		if err := json.Unmarshal(updateID, &id); err == nil {
			summary["update_id"] = id
		}
	}
	for _, key := range []string{
		"message",
		"business_connection",
		"business_message",
		"edited_business_message",
		"deleted_business_messages",
		"callback_query",
	} {
		if _, ok := raw[key]; ok {
			summary["kind"] = key
			break
		}
	}
	if _, ok := summary["kind"]; !ok {
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		summary["keys"] = keys
	}
	return summary
}

func (s *Service) conversationContext(event contracts.NormalizedTelegramEvent, ownerID string, limit int) []string {
	records := s.repository.RecentMessages(event.TenantID, event.ChatID, limit)
	now := s.now().UTC()
	context := make([]string, 0, len(records)+12)

	// First line: current date so the AI can reason about time gaps between messages.
	context = append(context, fmt.Sprintf("[CURRENT DATE: %s]", now.Format("2006-01-02")))

	// Prepend persisted memory facts. Owner style facts are labeled separately so the AI
	// can use them to construct the persona; contact facts describe the interlocutor.
	for _, fact := range s.repository.MemoryFacts(event.TenantID, event.ChatID, 20) {
		if fact.FactType == "owner_style" {
			context = append(context, fmt.Sprintf("[OWNER STYLE] %s", fact.FactText))
		} else {
			context = append(context, fmt.Sprintf("[FACT] %s: %s", fact.FactType, fact.FactText))
		}
	}

	for _, record := range records {
		if record.Event.SourceMessageID == event.SourceMessageID {
			continue
		}
		text := strings.TrimSpace(record.Event.MessageText)
		if text == "" {
			continue
		}
		role := "CONTACT"
		if isOwnerAuthoredMessage(record.Event, ownerID) {
			role = "OWNER"
		}
		// Include message date so AI can detect day boundaries and same-day conversations.
		msgDate := record.Event.CreatedAt.UTC().Format("2006-01-02 15:04")
		context = append(context, fmt.Sprintf("[%s] %s: %s", msgDate, role, text))
	}
	return context
}

// feedbackContext returns recent draft feedback for this chat to send to the AI.
func (s *Service) feedbackContext(tenantID, chatID string, limit int) []contracts.FeedbackItem {
	feedbacks := s.repository.RecentFeedback(tenantID, chatID, limit)
	items := make([]contracts.FeedbackItem, 0, len(feedbacks))
	for _, fb := range feedbacks {
		items = append(items, contracts.FeedbackItem{
			Rating:       fb.Rating,
			ResponseText: fb.ResponseText,
			FeedbackText: fb.FeedbackText,
		})
	}
	return items
}

// extractOwnerStyleIfNeeded asks the AI to extract style cues from the owner's message
// and persists them as owner_style memory facts. Called as a goroutine so it never blocks
// the inbound message path. Skips extraction if enough style facts already exist.
func (s *Service) extractOwnerStyleIfNeeded(event contracts.NormalizedTelegramEvent) {
	const styleFactThreshold = 5
	existing := s.repository.MemoryFacts(event.TenantID, event.ChatID, 20)
	styleCount := 0
	for _, f := range existing {
		if f.FactType == "owner_style" {
			styleCount++
		}
	}
	// Only extract every 5th message or when we have fewer facts than the threshold.
	msgCount := s.repository.ChatMessageCount(event.TenantID, event.ChatID)
	if styleCount >= styleFactThreshold && msgCount%5 != 0 {
		return
	}
	if s.ai == nil || strings.TrimSpace(event.MessageText) == "" {
		return
	}
	// Build a minimal request to the Python extract-memory endpoint via ProcessMessage.
	// We reuse the existing memory extractor pathway on the Python side.
	styleRequest := contracts.AIProcessMessageRequest{
		RequestID:    event.EventID + ":owner_style",
		Event:        event,
		ChatSettings: contracts.ChatSettings{TenantID: event.TenantID, ChatID: event.ChatID, Mode: contracts.ChatModeSilent},
		MemoryContext: []string{"[OWNER STYLE EXTRACTION] Extract concise style descriptors from the OWNER message below."},
	}
	proposal, err := s.ai.ProcessMessage(context.Background(), styleRequest)
	if err != nil || len(proposal.MemoryUsed) == 0 {
		return
	}
	for _, fact := range proposal.MemoryUsed {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		s.repository.SaveMemoryFact(store.MemoryFactRecord{
			TenantID:        event.TenantID,
			ChatID:          event.ChatID,
			FactType:        "owner_style",
			FactText:        fact,
			Confidence:      0.80,
			SourceMessageID: event.SourceMessageID,
		})
	}
}

// saveExtractedFacts persists memory facts returned by the AI service.
func (s *Service) saveExtractedFacts(event contracts.NormalizedTelegramEvent, facts []string) {
	if len(facts) == 0 || s.repository == nil {
		return
	}
	for _, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		s.repository.SaveMemoryFact(store.MemoryFactRecord{
			TenantID:        event.TenantID,
			ChatID:          event.ChatID,
			FactType:        "extracted",
			FactText:        fact,
			Confidence:      0.75,
			SourceMessageID: event.SourceMessageID,
		})
	}
}

func isOwnerAuthoredMessage(event contracts.NormalizedTelegramEvent, ownerID string) bool {
	return ownerID != "" && event.SenderID == ownerID
}

func anonymousChatName(chatID string) string {
	sum := sha256.Sum256([]byte(chatID))
	return fmt.Sprintf("Собеседник %x", sum[:3])
}

func (s *Service) handleBusinessConnection(ctx context.Context, update telegram.BusinessConnectionUpdate) InboundResult {
	if duplicate := s.repository.MarkReceive(update.ReceiveIdempotencyKey); duplicate {
		event := onboardingEvent(update.UserID, update.UserChatID, "business_connection:"+update.ID, update.UpdateID)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "duplicate_business_connection"))
		s.logger.Info("duplicate business connection update ignored", "business_connection_id", update.ID, "user_id", update.UserID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_business_connection", Duplicate: true}
	}
	tenantID := tenantForUser(update.UserID)
	s.repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: update.ID,
		TenantID:     tenantID,
		UserID:       update.UserID,
		UserChatID:   update.UserChatID,
		CanReply:     update.CanReply,
		IsEnabled:    update.IsEnabled,
		UpdatedAt:    s.now(),
	})
	event := onboardingEvent(tenantID, update.UserChatID, "business_connection:"+update.ID, update.UpdateID)
	reason := "business_connection_enabled"
	if !update.IsEnabled {
		reason = "business_connection_disabled"
	}
	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, reason))
	s.logger.Info("telegram business connection updated", "tenant_id", tenantID, "business_connection_id", update.ID, "can_reply", update.CanReply, "is_enabled", update.IsEnabled)
	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID:        tenantID,
		ChatID:          update.UserChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      event.EventID,
		Text:            bot.BusinessConnectionText(update),
	})
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: reason}
}

func (s *Service) handleSummaryCommand(ctx context.Context, command telegram.BotCommandUpdate, tenantID string, event contracts.NormalizedTelegramEvent) InboundResult {
	selector := strings.TrimSpace(command.Arguments)
	if selector == "" {
		selector = "1"
	}
	chat, ok := s.repository.ChatBySelector(tenantID, selector)
	if !ok {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Чат не найден. Используй /chats чтобы увидеть список.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_chat_not_found"}
	}

	settings := s.repository.ChatSettings(tenantID, chat.ChatID)
	if settings.PrivacyFlags.Contains(contracts.PrivacyNoSummary) {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Для этого чата отключены сводки (флаг no_summary).",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_blocked_by_privacy"}
	}

	messages := s.repository.RecentMessages(tenantID, chat.ChatID, 50)
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		if t := strings.TrimSpace(m.Event.MessageText); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Нет сообщений для суммаризации в этом чате.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_no_messages"}
	}

	summ, err := func() (contracts.ChatSummaryResponse, error) {
		if summarizer, ok := s.ai.(aiclient.ChatSummarizer); ok {
			return summarizer.SummarizeChat(ctx, contracts.ChatSummaryRequest{
				Messages:     texts,
				PrivacyFlags: settings.PrivacyFlags,
			})
		}
		return contracts.ChatSummaryResponse{}, fmt.Errorf("summarizer not available")
	}()

	replyText := ""
	if err != nil || summ.Summary == "" {
		replyText = fmt.Sprintf("📊 Чат «%s»: %d сообщений. Сводка недоступна.", chatDisplayName(chat), len(texts))
	} else {
		replyText = fmt.Sprintf("📊 Сводка чата «%s»:\n\n%s", chatDisplayName(chat), summ.Summary)
		if len(summ.ActionItems) > 0 {
			replyText += "\n\n📋 Действия:\n"
			for _, item := range summ.ActionItems {
				replyText += "• " + item + "\n"
			}
		}
	}

	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID: tenantID,
		ChatID:   command.ChatID,
		Text:     replyText,
	})
	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "summary_command"))
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_command"}
}

func chatDisplayName(chat store.ChatRecord) string {
	if chat.DisplayName != "" {
		return chat.DisplayName
	}
	return "chat " + chat.ChatID
}

func (s *Service) handleBotCommand(ctx context.Context, command telegram.BotCommandUpdate) InboundResult {
	tenantID := tenantForUser(command.SenderID)
	event := onboardingEvent(tenantID, command.ChatID, command.MessageID, command.UpdateID)
	if duplicate := s.repository.MarkReceive(command.ReceiveIdempotencyKey); duplicate {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "duplicate_bot_command"))
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_bot_command", Duplicate: true}
	}

	response := s.botLayer.HandleCommand(command, tenantID)

	if response.Reason == "summary_needs_ai" {
		return s.handleSummaryCommand(ctx, command, tenantID, event)
	}

	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, response.Reason))
	s.logger.Info("telegram bot command handled",
		"tenant_id", tenantID,
		"chat_id", command.ChatID,
		"command", command.Command,
		"reason", response.Reason,
		"visible_chats_count", len(s.repository.ChatsForTenant(tenantID)),
		"response_preview", truncateForLog(response.Text, 1000),
	)
	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID:        tenantID,
		ChatID:          command.ChatID,
		SourceMessageID: command.MessageID,
		DecisionID:      event.EventID,
		Text:            response.Text,
		ReplyMarkup:     response.ReplyMarkup,
	})
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: response.Reason}
}

// HandleApprovalCallback applies an owner approval callback.
func (s *Service) HandleApprovalCallback(ctx context.Context, payload contracts.ApprovalCallbackPayload) (ApprovalResult, error) {
	if payload.IdempotencyKey == "" {
		return ApprovalResult{}, fmt.Errorf("missing idempotency_key")
	}
	if !payload.Action.IsValid() {
		return ApprovalResult{}, fmt.Errorf("invalid action %q", payload.Action)
	}
	if duplicate := s.repository.MarkCallback(payload.IdempotencyKey); duplicate {
		return ApprovalResult{Action: payload.Action, Duplicate: true, DraftID: payload.DraftID, Status: "duplicate"}, nil
	}
	if payload.Action == contracts.ApprovalNoMemory {
		settings := s.repository.ChatSettings(payload.TenantID, payload.ChatID)
		if settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
			settings.PrivacyFlags = settings.PrivacyFlags.Remove(contracts.PrivacyNoMemory)
			s.repository.SaveChatSettings(settings)
			return ApprovalResult{Action: payload.Action, Status: "no_memory_disabled"}, nil
		}
		settings.PrivacyFlags = settings.PrivacyFlags.Add(contracts.PrivacyNoMemory)
		s.repository.SaveChatSettings(settings)
		return ApprovalResult{Action: payload.Action, Status: "no_memory_enabled"}, nil
	}

	draft, ok := s.repository.Draft(payload.DraftID)
	if !ok {
		return ApprovalResult{}, fmt.Errorf("draft not found")
	}
	now := s.now()
	switch payload.Action {
	case contracts.ApprovalApprove:
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateApprovedByOwner)
		sentMsgID, err := s.sendRecipient(ctx, contracts.NormalizedTelegramEvent{
			TenantID:             draft.TenantID,
			ChatID:               draft.ChatID,
			BusinessConnectionID: draft.BusinessConnectionID,
			SourceMessageID:      draft.SourceMessageID,
		}, draft.DecisionID, draft.ResponseText)
		if err != nil {
			s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
			return ApprovalResult{}, err
		}
		draft.Status = store.DraftSent
		draft.SentMessageID = sentMsgID
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateSentToRecipient)
		// Sending = implicit positive feedback: the owner approved this exact text.
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "up",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalReject:
		draft.Status = store.DraftRejected
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateIgnored)
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalEdit:
		if strings.TrimSpace(payload.EditedText) == "" {
			return ApprovalResult{}, fmt.Errorf("edited_text is required for edit action")
		}
		draft.ResponseText = payload.EditedText
		draft.ActionHistory = append(draft.ActionHistory, "edit")
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalRegenerate:
		draft.ActionHistory = append(draft.ActionHistory, "regenerate")
		s.repository.UpdateDraft(draft)
		updated, err := s.regenerateDraft(ctx, draft)
		if err != nil {
			return ApprovalResult{}, err
		}
		return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
	case contracts.ApprovalMakeShorter:
		draft.ActionHistory = append(draft.ActionHistory, "shorter")
		s.repository.UpdateDraft(draft)
		updated := s.reviseDraft(ctx, draft, payload.Action)
		return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
	case contracts.ApprovalMakeMoreFormal:
		draft.ActionHistory = append(draft.ActionHistory, "formal")
		s.repository.UpdateDraft(draft)
		updated := s.reviseDraft(ctx, draft, payload.Action)
		return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
	case contracts.ApprovalEditRequest, contracts.ApprovalAIRefine:
		// Pending input is set in handleCallbackQuery — no-op here.
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalThumbUp:
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "up",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		s.stripRatingButtons(ctx, draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: "feedback_saved"}, nil
	case contracts.ApprovalThumbDown:
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "down",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		s.stripRatingButtons(ctx, draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: "feedback_saved"}, nil
	default:
		return ApprovalResult{}, fmt.Errorf("unsupported action %q", payload.Action)
	}
}

// removeDraftCard deletes the draft card from the owner's bot chat after it's been acted upon.
func (s *Service) removeDraftCard(ctx context.Context, draft store.DraftRecord) {
	if draft.OwnerChatID == "" || draft.OwnerMessageID == "" {
		return
	}
	_ = s.sender.DeleteMessage(ctx, draft.OwnerChatID, draft.OwnerMessageID)
}

// stripRatingButtons edits the draft card to remove 👍👎 buttons after the owner has rated it.
func (s *Service) stripRatingButtons(ctx context.Context, draft store.DraftRecord) {
	if draft.OwnerChatID == "" || draft.OwnerMessageID == "" {
		return
	}
	keyboard := bot.DraftApprovalKeyboardNoRating(draft.DraftID, s.draftNoMemory(draft))
	_ = s.sender.EditMessageReplyMarkup(ctx, draft.OwnerChatID, draft.OwnerMessageID, keyboard)
}

// draftNoMemory reports whether the no_memory flag is active for the draft's chat.
func (s *Service) draftNoMemory(draft store.DraftRecord) bool {
	settings := s.repository.ChatSettings(draft.TenantID, draft.ChatID)
	return settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory)
}

// Repository returns the service repository.
func (s *Service) Repository() *store.Repository {
	return s.repository
}

func (s *Service) regenerateDraft(ctx context.Context, draft store.DraftRecord) (store.DraftRecord, error) {
	event := s.resolveSourceEvent(draft)
	settings := s.repository.ChatSettings(draft.TenantID, draft.ChatID)
	ownerID := ""
	if connection, ok := s.repository.BusinessConnection(draft.BusinessConnectionID); ok {
		ownerID = connection.UserID
	}
	regenRequest := contracts.AIProcessMessageRequest{
		RequestID:     draft.DecisionID + ":regenerate",
		Event:         event,
		ChatSettings:  settings,
		MemoryContext: s.conversationContext(event, ownerID, 20),
	}
	if profile, ok := s.repository.ContactProfile(draft.TenantID, draft.ChatID); ok {
		regenRequest.ContactProfile = &profile
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIRequested)
	proposal, err := s.ai.ProcessMessage(ctx, regenRequest)
	if err != nil {
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
		return store.DraftRecord{}, err
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIGenerated)
	draft.ResponseText = proposal.ResponseText
	draft.UpdatedAt = s.now()
	s.repository.UpdateDraft(draft)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
	return draft, nil
}

// regenerateDraftWithHint regenerates a draft using an owner-supplied instruction hint.
// The hint is injected into the memory context so the AI applies it ("make it shorter", "be more formal", etc.).
func (s *Service) regenerateDraftWithHint(ctx context.Context, draft store.DraftRecord, hint string) (store.DraftRecord, error) {
	event := s.resolveSourceEvent(draft)
	settings := s.repository.ChatSettings(draft.TenantID, draft.ChatID)
	ownerID := ""
	if connection, ok := s.repository.BusinessConnection(draft.BusinessConnectionID); ok {
		ownerID = connection.UserID
	}
	baseContext := s.conversationContext(event, ownerID, 20)
	// Inject the owner's hint and the current draft as context so the AI knows what to change.
	hintContext := append(baseContext,
		"[CURRENT DRAFT] "+draft.ResponseText,
		"[OWNER INSTRUCTION] "+hint,
	)
	hintRequest := contracts.AIProcessMessageRequest{
		RequestID:       draft.DecisionID + ":hint",
		Event:           event,
		ChatSettings:    settings,
		MemoryContext:   hintContext,
		FeedbackContext: s.feedbackContext(draft.TenantID, draft.ChatID, 5),
	}
	if profile, ok := s.repository.ContactProfile(draft.TenantID, draft.ChatID); ok {
		hintRequest.ContactProfile = &profile
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIRequested)
	proposal, err := s.ai.ProcessMessage(ctx, hintRequest)
	if err != nil {
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
		return store.DraftRecord{}, err
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIGenerated)
	draft.ActionHistory = append(draft.ActionHistory, "ai_refine")
	draft.ResponseText = proposal.ResponseText
	draft.UpdatedAt = s.now()
	s.repository.UpdateDraft(draft)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
	return draft, nil
}

func (s *Service) reviseDraft(ctx context.Context, draft store.DraftRecord, action contracts.ApprovalAction) store.DraftRecord {
	if reviser, ok := s.ai.(aiclient.DraftReviser); ok {
		if revised, err := reviser.ReviseDraft(ctx, aiclient.DraftRevisionRequest{Action: action, Text: draft.ResponseText}); err == nil && strings.TrimSpace(revised.Text) != "" {
			draft.ResponseText = revised.Text
		}
	} else {
		draft.ResponseText = localDraftRevision(draft.ResponseText, action)
	}
	draft.UpdatedAt = s.now()
	s.repository.UpdateDraft(draft)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
	return draft
}

// resolveSourceEvent returns the NormalizedTelegramEvent for the draft's source message.
// It first tries the message repository (available when no_memory is off), then falls back
// to reconstructing a minimal event from the draft's own fields so that regen and hint
// operations work even when the message was never persisted (no_memory flag).
func (s *Service) resolveSourceEvent(draft store.DraftRecord) contracts.NormalizedTelegramEvent {
	if message, ok := s.repository.Message(draft.TenantID, draft.ChatID, draft.SourceMessageID); ok {
		return message.Event
	}
	return contracts.NormalizedTelegramEvent{
		EventID:              draft.DecisionID,
		TenantID:             draft.TenantID,
		ChatID:               draft.ChatID,
		BusinessConnectionID: draft.BusinessConnectionID,
		SourceMessageID:      draft.SourceMessageID,
		MessageText:          draft.SourceMessageText,
		MessageType:          "text",
		CreatedAt:            draft.CreatedAt,
	}
}

func (s *Service) createDraft(event contracts.NormalizedTelegramEvent, decisionID, text string) store.DraftRecord {
	now := s.now()
	return store.DraftRecord{
		DraftID:              "draft:" + decisionID,
		DecisionID:           decisionID,
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      event.SourceMessageID,
		SourceMessageText:    event.MessageText,
		ResponseText:         text,
		Status:               store.DraftPending,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
}

func (s *Service) notifyDraftStatus(draft store.DraftRecord) {
	if s.notify == nil {
		return
	}
	s.notify(draft.TenantID, "draft_status", map[string]string{
		"draft_id": draft.DraftID,
		"chat_id":  draft.ChatID,
		"status":   string(draft.Status),
	})
}

// sendRecipient sends a reply to the contact and returns the Telegram message_id of the sent message.
func (s *Service) sendRecipient(ctx context.Context, event contracts.NormalizedTelegramEvent, decisionID, text string) (string, error) {
	key := contracts.SendIdempotencyKey(event.TenantID, event.ChatID, event.SourceMessageID, decisionID)
	duplicate := s.repository.MarkSend(store.SendRecord{
		Key:             key,
		TenantID:        event.TenantID,
		ChatID:          event.ChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      decisionID,
		Text:            text,
		CreatedAt:       s.now(),
	})
	if duplicate {
		return "", nil
	}
	msg := telegram.OutboundMessage{
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      event.SourceMessageID,
		DecisionID:           decisionID,
		Text:                 text,
	}
	if idSender, ok := s.sender.(interface {
		SendMessageGetID(context.Context, telegram.OutboundMessage) (string, error)
	}); ok {
		return idSender.SendMessageGetID(ctx, msg)
	}
	return "", s.sender.SendMessage(ctx, msg)
}

func (s *Service) auditEvent(event contracts.NormalizedTelegramEvent, decision contracts.Decision, reason string) contracts.AuditEvent {
	return contracts.AuditEvent{
		AuditID:         fmt.Sprintf("audit:%s:%d", valueOrDefault(event.EventID, event.SourceMessageID), s.now().UnixNano()),
		TenantID:        valueOrDefault(event.TenantID, "unknown"),
		ChatID:          valueOrDefault(event.ChatID, "unknown"),
		SourceMessageID: valueOrDefault(event.SourceMessageID, "unknown"),
		Decision:        decision,
		Reason:          reason,
		CreatedAt:       s.now(),
	}
}

func localDraftRevision(text string, action contracts.ApprovalAction) string {
	switch action {
	case contracts.ApprovalMakeShorter:
		text = strings.TrimSpace(text)
		for _, sep := range []string{". ", "! ", "? ", "\n"} {
			if idx := strings.Index(text, sep); idx > 0 {
				return strings.TrimSpace(text[:idx+1])
			}
		}
		return text
	case contracts.ApprovalMakeMoreFormal:
		return "Thank you for your message. " + strings.TrimSpace(text)
	default:
		return text
	}
}

func onboardingEvent(tenantID, chatID, sourceMessageID string, updateID int64) contracts.NormalizedTelegramEvent {
	eventID := fmt.Sprintf("%d:onboarding:%s", updateID, sourceMessageID)
	return contracts.NormalizedTelegramEvent{
		EventID:              eventID,
		TenantID:             tenantID,
		ChatID:               chatID,
		TelegramUpdateID:     updateID,
		BusinessConnectionID: "",
		SourceMessageID:      sourceMessageID,
		SenderID:             chatID,
		MessageText:          "",
		MessageType:          "command",
		CreatedAt:            time.Now().UTC(),
	}
}

func tenantForUser(userID string) string {
	return "tenant:user:" + userID
}

// sendDraftToOwner delivers a draft card with approval keyboard to the owner's bot chat.
// Stores the Telegram message_id of the card so it can be deleted/edited later.
func (s *Service) sendDraftToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerID string) {
	connection, ok := s.repository.BusinessConnection(event.BusinessConnectionID)

	ownerChatID := ""
	if ok && connection.UserChatID != "" {
		ownerChatID = connection.UserChatID
	} else {
		// Fallback: business_connection record not in DB (e.g. service restarted before it was persisted).
		// Infer the owner's chat ID from claimed chats that reference this connection_id.
		ownerChatID = s.inferOwnerChatID(event.BusinessConnectionID)
		if ownerChatID != "" {
			s.logger.Info("draft delivery: using fallback owner chat (reconnect bot to persist properly)",
				"business_connection_id", event.BusinessConnectionID,
				"inferred_owner_chat_id", ownerChatID,
			)
			// Auto-save the connection so future lookups don't need the fallback.
			s.repository.SaveBusinessConnection(store.BusinessConnectionRecord{
				ConnectionID: event.BusinessConnectionID,
				TenantID:     "tenant:user:" + ownerChatID,
				UserID:       ownerChatID,
				UserChatID:   ownerChatID,
				CanReply:     true,
				IsEnabled:    true,
				UpdatedAt:    s.now(),
			})
		}
	}

	if ownerChatID == "" {
		s.logger.Warn("cannot deliver draft to owner: business connection not found and no fallback available",
			"business_connection_id", event.BusinessConnectionID,
			"draft_id", draft.DraftID,
			"hint", "reconnect the bot via Telegram Settings → Chat Automation",
		)
		return
	}
	cardText := bot.DraftCardText(event.MessageText, draft.ResponseText, event.ChatDisplayName)
	keyboard := bot.DraftApprovalKeyboard(draft.DraftID, s.draftNoMemory(draft))
	msg := telegram.OutboundMessage{
		TenantID:        event.TenantID,
		ChatID:          ownerChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      draft.DecisionID,
		Text:            cardText,
		ReplyMarkup:     keyboard,
	}
	// If the sender can return a message ID, store it so the card can be deleted later.
	if idSender, ok := s.sender.(interface {
		SendMessageGetID(context.Context, telegram.OutboundMessage) (string, error)
	}); ok {
		msgID, err := idSender.SendMessageGetID(ctx, msg)
		if err == nil && msgID != "" {
			draft.OwnerChatID = ownerChatID
			draft.OwnerMessageID = msgID
			s.repository.UpdateDraft(draft)
		}
	} else {
		_ = s.sender.SendMessage(ctx, msg)
	}
}

// inferOwnerChatID finds the owner's bot chat ID for a business connection
// by looking at chats that have been claimed by a tenant:user:* owner.
func (s *Service) inferOwnerChatID(businessConnectionID string) string {
	tenantID := s.repository.OwnerTenantForBusinessConnection(businessConnectionID)
	if tenantID == "" {
		return ""
	}
	userID := strings.TrimPrefix(tenantID, "tenant:user:")
	if userID == tenantID {
		return "" // not the expected "tenant:user:USERID" format
	}
	return userID
}

// handleCallbackQuery processes an inline button press from the owner.
func (s *Service) handleCallbackQuery(ctx context.Context, cbq telegram.CallbackQueryUpdate) InboundResult {
	tenantID := tenantForUser(cbq.SenderID)
	event := onboardingEvent(tenantID, cbq.ChatID, cbq.OriginalMsgID, cbq.UpdateID)

	if duplicate := s.repository.MarkReceive(cbq.ReceiveIdempotencyKey); duplicate {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_callback_query", Duplicate: true}
	}

	action, draftID, idempotencyKey, ok := contracts.CallbackDataDecode(cbq.Data)
	if !ok {
		s.logger.Warn("callback query: unrecognised callback data", "data", cbq.Data)
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "unknown_callback_data"}
	}

	draft, hasDraft := s.repository.Draft(draftID)
	chatID := cbq.ChatID
	if hasDraft {
		// Use the draft's chat/tenant as canonical source of truth.
		event.TenantID = draft.TenantID
		event.ChatID = draft.ChatID
		chatID = draft.ChatID
		tenantID = draft.TenantID
	}

	payload := contracts.ApprovalCallbackPayload{
		Action:         action,
		TenantID:       tenantID,
		ChatID:         chatID,
		DraftID:        draftID,
		IdempotencyKey: idempotencyKey,
	}
	if hasDraft {
		payload.SourceMessageID = draft.SourceMessageID
		payload.DecisionID = draft.DecisionID
	}

	// edit_request and ai_refine bypass idempotency — owner can always re-open these prompts.
	switch action {
	case contracts.ApprovalEditRequest:
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		s.repository.SetPendingInput(tenantID, cbq.ChatID, contracts.PendingInput{
			TenantID:  tenantID,
			ChatID:    cbq.ChatID,
			Type:      contracts.PendingInputEdit,
			DraftID:   draftID,
			ExpiresAt: s.now().Add(5 * 60 * 1e9),
		})
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   cbq.ChatID,
			Text:     bot.DraftEditPromptText(),
		})
		s.logger.Info("callback query handled", "action", action, "draft_id", draftID, "status", "edit_prompt_sent")
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "callback_edit_request", DraftID: draftID}

	case contracts.ApprovalAIRefine:
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		s.repository.SetPendingInput(tenantID, cbq.ChatID, contracts.PendingInput{
			TenantID:  tenantID,
			ChatID:    cbq.ChatID,
			Type:      contracts.PendingInputAIRefine,
			DraftID:   draftID,
			ExpiresAt: s.now().Add(5 * 60 * 1e9),
		})
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   cbq.ChatID,
			Text:     bot.AIRefinePromptText(),
		})
		s.logger.Info("callback query handled", "action", action, "draft_id", draftID, "status", "ai_refine_prompt_sent")
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "callback_ai_refine", DraftID: draftID}
	}

	result, err := s.HandleApprovalCallback(ctx, payload)
	_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
	if err != nil {
		s.logger.Error("approval callback failed", "error", err, "draft_id", draftID, "action", action)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "callback_approval_error:" + err.Error()}
	}

	// After thumb_down or reject: offer text feedback (only on first press, not duplicates).
	if !result.Duplicate {
		switch action {
		case contracts.ApprovalThumbDown, contracts.ApprovalReject:
			s.repository.SetPendingInput(tenantID, cbq.ChatID, contracts.PendingInput{
				TenantID:  tenantID,
				ChatID:    cbq.ChatID,
				Type:      contracts.PendingInputFeedback,
				DraftID:   draftID,
				ExpiresAt: s.now().Add(5 * 60 * 1e9),
			})
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   cbq.ChatID,
				Text:     bot.DraftFeedbackPromptText(),
			})
		case contracts.ApprovalNoMemory:
			noMemory := result.Status == "no_memory_enabled"
			// Update the draft card keyboard so the memory button reflects the new state.
			if draft, ok := s.repository.Draft(draftID); ok && draft.OwnerChatID != "" && draft.OwnerMessageID != "" {
				keyboard := bot.DraftApprovalKeyboard(draft.DraftID, noMemory)
				_ = s.sender.EditMessageReplyMarkup(ctx, draft.OwnerChatID, draft.OwnerMessageID, keyboard)
			}
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   cbq.ChatID,
				Text:     bot.NoMemoryStatusText(noMemory),
			})
		}
	}

	s.logger.Info("callback query handled", "action", result.Action, "draft_id", result.DraftID, "status", result.Status)
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "callback_" + string(result.Action), DraftID: result.DraftID}
}

// handleOwnerMessage handles plain (non-command) text from the owner in the bot chat.
// Used for pending-input flows: editing a draft or providing text feedback.
func (s *Service) handleOwnerMessage(ctx context.Context, msg telegram.OwnerMessageUpdate) InboundResult {
	tenantID := tenantForUser(msg.SenderID)
	event := onboardingEvent(tenantID, msg.ChatID, msg.MessageID, msg.UpdateID)

	pending, hasPending := s.repository.PendingInput(tenantID, msg.ChatID)
	if !hasPending {
		// No pending input state — treat as unknown, show help.
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   msg.ChatID,
			Text:     "Используй /help для списка команд.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_no_pending_input"}
	}

	s.repository.ClearPendingInput(tenantID, msg.ChatID)

	switch pending.Type {
	case contracts.PendingInputFeedback:
		if msg.Text != "/skip" {
			s.repository.SaveDraftFeedback(contracts.DraftFeedback{
				DraftID:      pending.DraftID,
				TenantID:     tenantID,
				ChatID:       msg.ChatID,
				Rating:       "down",
				FeedbackText: msg.Text,
				CreatedAt:    s.now(),
			})
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   msg.ChatID,
			Text:     "Спасибо, учту в следующий раз.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_feedback_saved"}

	case contracts.PendingInputEdit:
		// Direct text replacement — exactly what the owner typed becomes the new draft.
		draft, ok := s.repository.Draft(pending.DraftID)
		if !ok {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Черновик не найден.",
			})
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_edit_draft_not_found"}
		}
		_, err := s.HandleApprovalCallback(ctx, contracts.ApprovalCallbackPayload{
			Action:          contracts.ApprovalEdit,
			TenantID:        draft.TenantID,
			ChatID:          draft.ChatID,
			SourceMessageID: draft.SourceMessageID,
			DraftID:         draft.DraftID,
			DecisionID:      draft.DecisionID,
			IdempotencyKey:  draft.DraftID + ":edit:" + msg.MessageID,
			EditedText:      msg.Text,
		})
		if err != nil {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Не удалось обновить черновик: " + err.Error(),
			})
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "owner_edit_failed"}
		}
		updatedDraft, _ := s.repository.Draft(pending.DraftID)
		keyboard := bot.DraftApprovalKeyboard(updatedDraft.DraftID, s.draftNoMemory(updatedDraft))
		cardText := bot.DraftCardText(updatedDraft.SourceMessageText, updatedDraft.ResponseText, anonymousChatName(updatedDraft.ChatID))
		if updatedDraft.OwnerChatID != "" && updatedDraft.OwnerMessageID != "" {
			_ = s.sender.EditMessageText(ctx, updatedDraft.OwnerChatID, updatedDraft.OwnerMessageID, cardText, keyboard)
		} else {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: msg.ChatID, Text: cardText, ReplyMarkup: keyboard})
		}
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_edit_applied"}

	case contracts.PendingInputAIRefine:
		// AI refinement — the owner's text is an instruction for the AI to improve the current draft.
		draft, ok := s.repository.Draft(pending.DraftID)
		if !ok {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Черновик не найден.",
			})
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_ai_refine_draft_not_found"}
		}
		updatedDraft, err := s.regenerateDraftWithHint(ctx, draft, msg.Text)
		if err != nil {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "ИИ не смог применить правку: " + err.Error(),
			})
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "owner_ai_refine_failed"}
		}
		keyboard := bot.DraftApprovalKeyboard(updatedDraft.DraftID, s.draftNoMemory(updatedDraft))
		cardText := bot.DraftCardText(updatedDraft.SourceMessageText, updatedDraft.ResponseText, anonymousChatName(updatedDraft.ChatID))
		if updatedDraft.OwnerChatID != "" && updatedDraft.OwnerMessageID != "" {
			_ = s.sender.EditMessageText(ctx, updatedDraft.OwnerChatID, updatedDraft.OwnerMessageID, cardText, keyboard)
		} else {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: msg.ChatID, Text: cardText, ReplyMarkup: keyboard})
		}
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_ai_refine_applied"}
	}

	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_handled"}
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func truncateForLog(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "...[truncated]"
}
