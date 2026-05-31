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
	tracepkg "doubleu/backend-go/internal/trace"
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
	// scoreSender attaches owner feedback (approve / reject / thumb) to the
	// Langfuse trace of the draft.  nil means observability is off.
	scoreSender OwnerFeedbackSender
}

// OwnerFeedbackSender is implemented by observability.ScoreSender; the
// interface keeps the core package decoupled from the OTel/Langfuse SDK.
type OwnerFeedbackSender interface {
	SendOwnerFeedback(ctx context.Context, traceID string, rating string, comment string)
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

// WithOwnerFeedbackSender wires a Langfuse-backed owner feedback forwarder.
func WithOwnerFeedbackSender(sender OwnerFeedbackSender) Option {
	return func(service *Service) {
		service.scoreSender = sender
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
	// Stamp the request context with the deterministic trace-id derived from
	// event_id.  Idempotent webhook replays share a trace (Phase 0 envelope).
	ctx = tracepkg.WithTraceID(ctx, tracepkg.DeriveTraceID(event.EventID))
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
		// Auto-derived tenants must look like a real user tenant; otherwise drop
		// to avoid poisoning unclaimed_chats.owner_tenant_id with a fallback value
		// such as the business connection ID.  An explicit tenantHint from the
		// webhook URL is trusted as-is and bypasses this check.
		if !strings.HasPrefix(event.TenantID, "tenant:user:") {
			event.TenantID = ""
		}
	}
	connection, hasConnection := s.repository.BusinessConnection(event.BusinessConnectionID)
	ownerID := ""
	if hasConnection {
		ownerID = connection.UserID
	} else if strings.HasPrefix(event.TenantID, "tenant:user:") {
		ownerID = strings.TrimPrefix(event.TenantID, "tenant:user:")
	} else if ownerTenant := s.repository.OwnerTenantForBusinessConnection(event.BusinessConnectionID); strings.HasPrefix(ownerTenant, "tenant:user:") {
		ownerID = strings.TrimPrefix(ownerTenant, "tenant:user:")
	}
	if event.ChatDisplayName == "" {
		event.ChatDisplayName = anonymousChatName(event.ChatID)
	}

	// Gate: only process chats the owner has explicitly added.
	// New contacts are staged in unclaimed_chats for owner decision.
	// When the owner tenant is known, messages are persisted and facts are extracted in the
	// background so history and memory are available immediately after the owner claims the chat.
	if event.BusinessConnectionID != "" && !s.repository.IsChatClaimed(event.TenantID, event.ChatID) {
		s.repository.SaveUnclaimedChat(store.UnclaimedChatRecord{
			OwnerTenantID:        event.TenantID,
			BusinessConnectionID: event.BusinessConnectionID,
			ChatID:               event.ChatID,
			DisplayName:          event.ChatDisplayName,
			ChatType:             event.ChatType,
			LastMessageAt:        s.now(),
			Status:               "pending",
		})
		if event.TenantID != "" {
			// Owner is known: persist the chat row + message so history survives claim.
			s.repository.SaveChat(store.ChatRecord{
				TenantID:             event.TenantID,
				ChatID:               event.ChatID,
				DisplayName:          event.ChatDisplayName,
				ChatType:             event.ChatType,
				BusinessConnectionID: event.BusinessConnectionID,
				LastSourceMessageID:  event.SourceMessageID,
				LastSeenAt:           s.now(),
				Status:               "unclaimed",
			})
			s.repository.SaveMessage(event)
			s.processPassive(ctx, event)
		}
		s.logger.Info("unclaimed business chat — passive RAG processing",
			"chat_id", event.ChatID,
			"business_connection_id", event.BusinessConnectionID,
			"owner_tenant_id", event.TenantID,
		)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "passive_processing"}, nil
	}

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
	// Persist the message row BEFORE any state/audit write so the DB foreign
	// keys from message_states and audit_events back to messages always resolve.
	// no_memory chats skip persistence — for those, AppendState / AppendAudit
	// silently fall back to in-memory tracking inside the repository.
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		s.repository.SaveMessage(event)
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateReceived)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateNormalized)
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
		// Runtime State Layer: open or update the active episode for this chat (§6.2).
		now := s.now()
		ep := store.ActiveEpisodeRecord{
			TenantID:      event.TenantID,
			ChatID:        event.ChatID,
			StartedAt:     now,
			LastMessageAt: now,
			MessageCount:  1,
		}
		if existing, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
			ep.StartedAt = existing.StartedAt
			ep.MessageCount = existing.MessageCount + 1
		}
		s.repository.UpsertActiveEpisode(ep)
		// Message-limit trigger: close and summarise the episode when it reaches
		// episodeMessageLimit messages (design §6.2). The closure is synchronous so a
		// concurrent handler cannot create a duplicate; summarisation runs in a goroutine.
		if ep.MessageCount >= episodeMessageLimit {
			s.repository.CloseActiveEpisode(event.TenantID, event.ChatID)
			s.logger.Info("episode closed by message limit",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"message_count", ep.MessageCount,
			)
			closedAt := ep.LastMessageAt
			tenantID, chatID := event.TenantID, event.ChatID
			go s.summarizeChatDay(ctx, tenantID, chatID, closedAt, "message_limit")
		}
	}
	s.repository.IncrementChatMessageCount(event.TenantID, event.ChatID)
	if s.notify != nil {
		s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StatePolicyChecked)
	if isOwnerAuthoredMessage(event, ownerID) {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "owner_message_observed"))
		s.logger.Info("owner-authored business message stored as context without ai reply",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"source_message_id", event.SourceMessageID,
			"sender_id", event.SenderID,
		)
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

	// Burst debounce: mark this message as the latest in the chat, then wait replyDelay.
	// If a newer message arrives during the wait it will overwrite the map entry, so after
	// sleeping we only proceed to AI when we are still the latest message in the burst.
	// All burst messages are saved to context before this point, so the winning message
	// sees the full burst in RecentMessages.
	chatKey := event.TenantID + "/" + event.ChatID
	s.debounce.Store(chatKey, event.EventID)
	if s.replyDelay > 0 {
		select {
		case <-time.After(s.replyDelay):
		case <-ctx.Done():
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "context_cancelled"}, ctx.Err()
		}
		if latest, _ := s.debounce.Load(chatKey); latest != event.EventID {
			s.logger.Info("burst debounce skipped", "tenant_id", event.TenantID, "chat_id", event.ChatID, "event_id", event.EventID)
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "burst_debounce_skipped"}, nil
		}
	}

	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIRequested)
	s.logger.Info("ai analysis started", "tenant_id", event.TenantID, "chat_id", event.ChatID, "mode", settings.Mode)
	if s.notify != nil {
		s.notify(event.TenantID, "ai_thinking", map[string]any{"chat_id": event.ChatID})
	}
	request := contracts.AIProcessMessageRequest{
		RequestID:       event.EventID,
		TraceID:         tracepkg.DeriveTraceID(event.EventID),
		Event:           event,
		ChatSettings:    settings,
		MemoryContext:   s.conversationContext(ctx, event, ownerID, 20),
		FeedbackContext: s.feedbackContext(event.TenantID, event.ChatID, 5),
	}
	if profile, ok := s.repository.ContactProfile(event.TenantID, event.ChatID); ok {
		request.ContactProfile = &profile
	}
	// User Layer: owner identity and preferences (§3).
	if us, ok := s.repository.UserSettings(event.TenantID); ok {
		request.UserSettings = &us
	}
	// Runtime State Layer: current open episode data for AI awareness (§6.2).
	if ep, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
		ae := contracts.ActiveEpisode{
			TenantID:           ep.TenantID,
			ChatID:             ep.ChatID,
			StartedAt:          ep.StartedAt,
			LastMessageAt:      ep.LastMessageAt,
			MessageCount:       ep.MessageCount,
			CurrentTopic:       ep.CurrentTopic,
			OpenQuestionMsgID:  ep.OpenQuestionMsgID,
			PendingOwnerAction: ep.PendingOwnerAction,
		}
		request.ActiveEpisode = &ae
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
	if s.notify != nil {
		s.notify(event.TenantID, "ai_done", map[string]any{"chat_id": event.ChatID})
	}
	if err != nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "ai_process_error:"+err.Error()))
		s.logger.Error("ai analysis failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "ai_process_error"}, nil
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIGenerated)
	// Persist fast (regex) facts returned by process-message synchronously.
	// LLM-based extraction is fired asynchronously below, after the draft is dispatched,
	// so it never adds latency to the reply path (see backlog §7.1).
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
		sentMsgID, err := s.sendRecipient(ctx, event, event.EventID, proposal.ResponseText)
		if err != nil {
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
			s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "send_error:"+err.Error()))
			s.logger.Error("telegram send failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "send_error"}, nil
		}
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateSentToRecipient)
		s.logger.Info("telegram auto reply sent", "tenant_id", event.TenantID, "chat_id", event.ChatID, "source_message_id", event.SourceMessageID)
		s.saveOutgoingMessage(event, ownerID, sentMsgID, proposal.ResponseText, event.EventID)
		if s.notify != nil {
			s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
		}
	case contracts.DecisionDraft, contracts.DecisionAskOwner:
		// When the AI flags the incoming message as a choice question, replace the normal
		// draft flow with an interactive question card that lets the owner pick an option.
		if proposal.Intent == "choice_question" {
			options := parseOptions(proposal.ResponseText)
			if len(options) > 0 {
				draft := s.createDraft(event, event.EventID, "")
				draft.DraftType = "question"
				draft.SuggestedOptions = options
				s.repository.SaveDraft(draft)
				s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
				s.logger.Info("question card prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID, "options_count", len(options))
				result.DraftID = draft.DraftID
				if s.notify != nil {
					s.notify(draft.TenantID, "new_draft", map[string]any{
						"chat_id":     draft.ChatID,
						"draft_id":    draft.DraftID,
						"draft_type":  draft.DraftType,
						"options":     draft.SuggestedOptions,
						"source_text": draft.SourceMessageText,
						"created_at":  draft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
					})
				} else {
					s.sendQuestionToOwner(ctx, event, draft, ownerID)
				}
				break
			}
		}
		draft := s.createDraft(event, event.EventID, proposal.ResponseText)
		if proposal.Flagged {
			draft.ActionHistory = append(draft.ActionHistory, "flagged")
		}
		s.repository.SaveDraft(draft)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
		s.logger.Info("owner draft prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID, "decision", decision, "flagged", proposal.Flagged)
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

	// Async LLM fact extraction: fires after the draft is dispatched so it never blocks
	// the reply path.  Prefers TypedMemoryExtractor (/ai/extract-facts-typed) for proper
	// scope routing; falls back to MemoryExtractor (/ai/extract-memory) if unavailable.
	// Only runs when the AI client supports extraction and memory is not disabled.
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		privacyStrings := make([]string, len(settings.PrivacyFlags))
		for i, f := range settings.PrivacyFlags {
			privacyStrings[i] = string(f)
		}
		if typedExtractor, ok := s.ai.(aiclient.TypedMemoryExtractor); ok {
			go func() {
				extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				start := s.now()
				resp, err := typedExtractor.ExtractFactsTyped(extractCtx, aiclient.TypedExtractionRequest{
					MessageText:  event.MessageText,
					TenantID:     event.TenantID,
					ChatID:       event.ChatID,
					PrivacyFlags: privacyStrings,
				})
				durationMS := int(s.now().Sub(start).Milliseconds())
				if err != nil {
					s.logger.Warn("async typed extraction failed",
						"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
					s.repository.LogExtraction(store.ExtractionLogRecord{
						TenantID:        event.TenantID,
						ChatID:          event.ChatID,
						InputMessageIDs: []string{event.SourceMessageID},
						Error:           err.Error(),
						DurationMS:      durationMS,
					})
					return
				}
				if resp.BlockedByPrivacy {
					return
				}
				outcome := s.saveTypedFacts(event, resp.Facts)
				// Persist the attempt so a returned empty fact set is observable and
				// distinguishable from a silent failure (extraction observability).
				s.repository.LogExtraction(store.ExtractionLogRecord{
					TenantID:        event.TenantID,
					ChatID:          event.ChatID,
					InputMessageIDs: []string{event.SourceMessageID},
					ModelUsed:       resp.ModelUsed,
					RawOutput:       resp.RawOutput,
					FactsCreated:    outcome.CreatedIDs,
					FactsSkipped:    outcome.Skipped,
					FactsMerged:     resp.DeterministicFactsCount,
					DurationMS:      durationMS,
				})
			}()
		} else if extractor, ok := s.ai.(aiclient.MemoryExtractor); ok {
			go func() {
				extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				resp, err := extractor.ExtractMemory(extractCtx, aiclient.MemoryExtractionRequest{
					MessageText:  event.MessageText,
					TenantID:     event.TenantID,
					ChatID:       event.ChatID,
					PrivacyFlags: privacyStrings,
				})
				if err != nil {
					s.logger.Warn("async memory extraction failed",
						"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
					return
				}
				if !resp.BlockedByPrivacy {
					s.saveExtractedFacts(event, resp.Memories)
				}
			}()
		}
	}

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

// conversationContext assembles the full memory context slice for the AI.
// It embeds the incoming message text (best-effort, 2-second timeout) and uses
// vector similarity search for semantic/episodic retrieval when embeddings are
// available (design §8). Falls back to recency ordering when they are not.
func (s *Service) conversationContext(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, limit int) []string {
	records := s.repository.RecentMessages(event.TenantID, event.ChatID, limit)
	now := s.now().UTC()
	// Use "parts" to avoid shadowing the "context" package import.
	parts := make([]string, 0, len(records)+30)

	// Embed the incoming message for similarity-based retrieval (design §8).
	// Uses a bounded sub-context so a slow provider never blocks the reply path.
	// 5 s covers typical OpenRouter embedding latency (~2–3 s) with margin.
	var queryVector []float64
	if embedder, ok := s.ai.(aiclient.TextEmbedder); ok && event.MessageText != "" {
		embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := embedder.EmbedTexts(embedCtx, aiclient.EmbedTextsRequest{Texts: []string{event.MessageText}})
		cancel()
		if err == nil && len(resp.Embeddings) > 0 {
			queryVector = resp.Embeddings[0]
		}
	}

	// Current date so the AI can reason about time gaps between messages.
	parts = append(parts, fmt.Sprintf("[CURRENT DATE: %s]", now.Format("2006-01-02")))

	// Greeting state: injected from the authoritative DB function (view_chat_greeted_today)
	// so the Python prompt builder does not need to re-derive this by parsing timestamps.
	// Authoritative greeting state from the DB, always emitted (true AND false)
	// so the Python prompt builder can trust this flag and skip re-deriving
	// "greeted today" by scanning every transcript line.
	parts = append(parts, fmt.Sprintf("[RUNTIME: greeting_sent_today=%t]",
		s.repository.GreetedToday(event.TenantID, event.ChatID, ownerID)))

	// Open commitments: injected so the prompt can reference active obligations without
	// a full fact scan.  Uses view_open_commitments DB function for consistency.
	for _, c := range s.repository.OpenCommitments(event.TenantID, event.ChatID) {
		parts = append(parts, fmt.Sprintf("[ОБЯЗАТЕЛЬСТВО: %s]", c.FactText))
	}

	// Runtime state: what's currently happening in this chat (open episode data).
	if episode, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
		if episode.CurrentTopic != "" {
			parts = append(parts, fmt.Sprintf("[RUNTIME: тема разговора] %s", episode.CurrentTopic))
		}
		if episode.PendingOwnerAction != "" {
			parts = append(parts, fmt.Sprintf("[RUNTIME: ожидается действие владельца] %s", episode.PendingOwnerAction))
		}
	}

	// Semantic layer — owner-global facts (visible in all chats, §5.3).
	// Uses vector similarity when a query embedding is available; recency fallback otherwise.
	globalFacts := s.repository.GlobalSemanticFactsByVector(event.TenantID, queryVector, 5)
	for _, fact := range globalFacts {
		parts = append(parts, fmt.Sprintf("[ФАКТ О ВЛАДЕЛЬЦЕ: %s] %s", fact.FactType, fact.FactText))
	}

	// Semantic layer — chat-scoped facts about this contact (§5.3).
	chatFacts := s.repository.SemanticFactsByVector(event.TenantID, event.ChatID, queryVector, 8)
	for _, fact := range chatFacts {
		parts = append(parts, fmt.Sprintf("[ФАКТ: %s] %s", fact.FactType, fact.FactText))
	}

	// Episodic layer: compressed summaries of past closed episodes, oldest first (§7.5).
	episodeSummaries := s.repository.EpisodeSummariesByVector(event.TenantID, event.ChatID, queryVector, 3)
	if len(episodeSummaries) > 0 {
		parts = append(parts, "--- ЭПИЗОДИЧЕСКАЯ ПАМЯТЬ (сводки прошлых диалогов) ---")
		for i := len(episodeSummaries) - 1; i >= 0; i-- {
			sm := episodeSummaries[i]
			parts = append(parts, fmt.Sprintf("[СВОДКА %s] %s",
				sm.PeriodEnd.UTC().Format("2006-01-02"), sm.Content))
		}
	}

	// Working layer: recent raw messages from this chat.
	parts = append(parts, "--- ПАМЯТЬ ТЕКУЩЕГО ДНЯ ---")
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
		msgDate := record.Event.CreatedAt.UTC().Format("2006-01-02 15:04")
		parts = append(parts, fmt.Sprintf("[%s] %s: %s", msgDate, role, text))
	}

	// Pending drafts: responses prepared by the AI but not yet sent (awaiting owner approval).
	// Included so the AI understands there is an unanswered obligation and can deflect gracefully
	// in autonomous mode — using a tone appropriate for the contact's relationship type.
	pendingDrafts := s.repository.DraftsForChat(event.TenantID, event.ChatID)
	hasBlock := false
	for _, d := range pendingDrafts {
		if d.Status != store.DraftPending {
			continue
		}
		if !hasBlock {
			parts = append(parts, "--- НЕОТПРАВЛЕННЫЕ ЧЕРНОВИКИ (ожидают подтверждения владельца) ---")
			hasBlock = true
		}
		parts = append(parts, fmt.Sprintf(
			"[ЧЕРНОВИК НЕ ОТПРАВЛЕН] В ответ на: %q\nПредложено (не отправлено): %q",
			d.SourceMessageText, d.ResponseText,
		))
	}

	return parts
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

// contextMarkerPrefixes are strings that identify assembled prompt context lines injected
// by the Go backend (e.g. date headers, retrieved facts, conversation history).
// If any of these appear in memory_used they are context contamination — not extracted facts —
// and must be rejected to prevent the self-poisoning loop described in the Bug Backlog.
var contextMarkerPrefixes = []string{
	"[CURRENT DATE",
	"[ФАКТ",
	"[FACT",
	"[EXTRACTED",
	"[OWNER STYLE",
	"[OWNER INSTRUCTION",
	"[CURRENT DRAFT",
	"[ЧЕРНОВИК",
	"[RUNTIME",
	"[2026", "[2025", "[2024", "[2023", // timestamped conversation lines
}

// isContextContamination returns true when the string looks like an assembled prompt line
// rather than a newly extracted fact.  This is a defence-in-depth guard; the primary fix
// is in the Python service which must not return input context in memory_used.
func isContextContamination(s string) bool {
	for _, prefix := range contextMarkerPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// saveExtractedFacts persists facts returned by the AI service into the Semantic Layer.
// Only genuinely new facts extracted from the incoming message are stored; context marker
// strings that indicate assembled prompt lines are rejected as contamination.
// Facts are stored as chat-scoped preference facts — conservative defaults that work
// without typed JSON output from the AI (Phase 4 extractor upgrade will add full typing).
func (s *Service) saveExtractedFacts(event contracts.NormalizedTelegramEvent, facts []string) {
	if len(facts) == 0 || s.repository == nil {
		return
	}
	for _, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" || len(fact) < 5 || len(fact) > 200 {
			continue
		}
		if isContextContamination(fact) {
			s.logger.Warn("saveExtractedFacts: rejected context-contaminated string",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"fact_preview", truncateForLog(fact, 80),
			)
			continue
		}
		s.repository.UpsertSemanticFact(store.SemanticFactRecord{
			TenantID:        event.TenantID,
			ScopeKind:       "chat",
			ChatID:          event.ChatID,
			FactText:        fact,
			FactType:        "preference",
			Subject:         "contact",
			Confidence:      0.75,
			Source:          "llm_extracted",
			SourceMessageID: event.SourceMessageID,
		})
	}
}

// processPassive runs asynchronous fact extraction for unclaimed chats.
// It opens/updates the active episode and fires the extractor in a goroutine so it never
// blocks the webhook path. No response is generated and no draft is created.
func (s *Service) processPassive(ctx context.Context, event contracts.NormalizedTelegramEvent) {
	// Maintain an active episode so episode-based context works after claim.
	now := s.now()
	ep := store.ActiveEpisodeRecord{
		TenantID:      event.TenantID,
		ChatID:        event.ChatID,
		StartedAt:     now,
		LastMessageAt: now,
		MessageCount:  1,
	}
	if existing, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
		ep.StartedAt = existing.StartedAt
		ep.MessageCount = existing.MessageCount + 1
	}
	s.repository.UpsertActiveEpisode(ep)

	// Fire lightweight extraction in the background — no response generation.
	if typedExtractor, ok := s.ai.(aiclient.TypedMemoryExtractor); ok {
		go func() {
			extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := s.now()
			resp, err := typedExtractor.ExtractFactsTyped(extractCtx, aiclient.TypedExtractionRequest{
				MessageText: event.MessageText,
				TenantID:    event.TenantID,
				ChatID:      event.ChatID,
			})
			durationMS := int(s.now().Sub(start).Milliseconds())
			if err != nil {
				s.logger.Warn("passive typed extraction failed",
					"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
				s.repository.LogExtraction(store.ExtractionLogRecord{
					TenantID:        event.TenantID,
					ChatID:          event.ChatID,
					InputMessageIDs: []string{event.SourceMessageID},
					Error:           err.Error(),
					DurationMS:      durationMS,
				})
				return
			}
			if resp.BlockedByPrivacy {
				return
			}
			outcome := s.saveTypedFacts(event, resp.Facts)
			s.repository.LogExtraction(store.ExtractionLogRecord{
				TenantID:        event.TenantID,
				ChatID:          event.ChatID,
				InputMessageIDs: []string{event.SourceMessageID},
				ModelUsed:       resp.ModelUsed,
				RawOutput:       resp.RawOutput,
				FactsCreated:    outcome.CreatedIDs,
				FactsSkipped:    outcome.Skipped,
				FactsMerged:     resp.DeterministicFactsCount,
				DurationMS:      durationMS,
			})
		}()
	} else if extractor, ok := s.ai.(aiclient.MemoryExtractor); ok {
		go func() {
			extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := extractor.ExtractMemory(extractCtx, aiclient.MemoryExtractionRequest{
				MessageText: event.MessageText,
				TenantID:    event.TenantID,
				ChatID:      event.ChatID,
			})
			if err != nil {
				s.logger.Warn("passive memory extraction failed",
					"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
				return
			}
			if !resp.BlockedByPrivacy {
				s.saveExtractedFacts(event, resp.Memories)
			}
		}()
	}
}

// ownerGlobalMinConfidence is the floor a subject=owner biographical/health fact
// must clear before it is promoted to owner_global (visible in every chat).  A
// low-confidence guess like "sitting in class" (0.7) otherwise leaked into all
// chats and over-anchored the draft model; below the floor the fact is kept
// chat-scoped so it informs only the chat it was observed in.
const ownerGlobalMinConfidence = 0.8

// saveTypedFacts persists typed facts returned by the Python AI typed-extraction endpoint.
// Applies deterministic scope routing per design §5.4:
//   - subject=owner AND fact_type∈(health,biographical) AND confidence≥ownerGlobalMinConfidence → scope_kind=owner_global
//   - everything else → scope_kind=chat (overrides Python hint for safety)
//
// Health facts with scope_kind=owner_global are routed to the pending_global_health_facts
// confirmation queue rather than written directly to semantic_facts.
// extractionOutcome summarises what saveTypedFacts did with a batch of facts, so
// the caller can persist an extraction_log row for observability.
type extractionOutcome struct {
	CreatedIDs []int64
	Skipped    int
}

func (s *Service) saveTypedFacts(event contracts.NormalizedTelegramEvent, facts []aiclient.TypedFact) extractionOutcome {
	outcome := extractionOutcome{}
	if len(facts) == 0 || s.repository == nil {
		return outcome
	}
	for _, f := range facts {
		text := strings.TrimSpace(f.FactText)
		if text == "" || len(text) < 5 || len(text) > 200 {
			outcome.Skipped++
			continue
		}
		if isContextContamination(text) {
			s.logger.Warn("saveTypedFacts: rejected context-contaminated string",
				"tenant_id", event.TenantID, "fact_preview", truncateForLog(text, 80))
			outcome.Skipped++
			continue
		}

		// Deterministic scope routing: backend always decides — Python hint is ignored.
		// Start from "chat"; promote to owner_global only when the conditions are
		// unambiguously met. This prevents a low-confidence Python guess like
		// "sitting in class" from leaking into every other chat.
		scopeKind := "chat"
		if f.Subject == "owner" && (f.FactType == "health" || f.FactType == "biographical") &&
			f.Confidence >= ownerGlobalMinConfidence {
			scopeKind = "owner_global"
		}

		// Health facts from the owner require confirmation before entering semantic_facts.
		if f.FactType == "health" && scopeKind == "owner_global" {
			s.repository.SavePendingHealthFact(store.PendingHealthFactRecord{
				TenantID:        event.TenantID,
				ChatID:          event.ChatID,
				SourceMessageID: event.SourceMessageID,
				FactText:        text,
				Confidence:      f.Confidence,
			})
			// Routed to the confirmation queue rather than written — not a skip.
			continue
		}

		chatID := event.ChatID
		if scopeKind == "owner_global" {
			chatID = ""
		}
		result := s.repository.UpsertSemanticFact(store.SemanticFactRecord{
			TenantID:        event.TenantID,
			ScopeKind:       scopeKind,
			ChatID:          chatID,
			FactText:        text,
			FactType:        f.FactType,
			Subject:         f.Subject,
			Confidence:      f.Confidence,
			Source:          "llm_extracted",
			SourceMessageID: event.SourceMessageID,
		})
		if result.FactID != 0 {
			outcome.CreatedIDs = append(outcome.CreatedIDs, result.FactID)
		}
	}
	return outcome
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
		s.saveOutgoingMessageFromDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateSentToRecipient)
		if s.notify != nil {
			s.notify(draft.TenantID, "new_message", map[string]any{"chat_id": draft.ChatID})
		}
		// Sending = implicit positive feedback: the owner approved this exact text.
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "up",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "up", "")
		}
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalReject:
		draft.Status = store.DraftRejected
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateIgnored)
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
		}
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalEdit:
		if strings.TrimSpace(payload.EditedText) == "" {
			return ApprovalResult{}, fmt.Errorf("edited_text is required for edit action")
		}
		// Edit-delta capture: the original AI text the owner chose to rewrite is the
		// single richest negative signal. Record it once — on the FIRST edit, before
		// ResponseText is overwritten — as a "down". The eventual approve records the
		// sent text as "up", yielding a clean original→final pair under the
		// UNIQUE(draft_id, rating) constraint that future drafts learn from.
		alreadyEdited := false
		for _, a := range draft.ActionHistory {
			if a == "edit" {
				alreadyEdited = true
				break
			}
		}
		if !alreadyEdited {
			s.repository.SaveDraftFeedback(contracts.DraftFeedback{
				DraftID:      draft.DraftID,
				TenantID:     draft.TenantID,
				ChatID:       draft.ChatID,
				Rating:       "down",
				ResponseText: draft.ResponseText,
				CreatedAt:    now,
			})
			if s.scoreSender != nil {
				s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
			}
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
	case contracts.ApprovalAIRefine:
		if strings.TrimSpace(payload.EditedText) != "" {
			// Mini App flow: owner supplied a refinement hint inline.
			updated, err := s.regenerateDraftWithHint(ctx, draft, payload.EditedText)
			if err != nil {
				return ApprovalResult{}, err
			}
			s.notifyDraftStatus(updated)
			return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
		}
		// Bot flow: no-op, pending input handled by handleCallbackQuery.
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalEditRequest:
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
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "up", "")
		}
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
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
		}
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
		TraceID:       tracepkg.DeriveTraceID(event.EventID),
		Event:         event,
		ChatSettings:  settings,
		MemoryContext: s.conversationContext(ctx, event, ownerID, 20),
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
	baseContext := s.conversationContext(ctx, event, ownerID, 20)
	// Inject the owner's hint and the current draft as context so the AI knows what to change.
	hintContext := append(baseContext,
		"[CURRENT DRAFT] "+draft.ResponseText,
		"[OWNER INSTRUCTION] "+hint,
	)
	hintRequest := contracts.AIProcessMessageRequest{
		RequestID:       draft.DecisionID + ":hint",
		TraceID:         tracepkg.DeriveTraceID(event.EventID),
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
		// Persisting trace_id lets owner feedback (approve / thumb) attach a
		// Langfuse score back to the Phase 4 generation span.
		TraceID:   tracepkg.DeriveTraceID(event.EventID),
		CreatedAt: now,
		UpdatedAt: now,
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
		ReplyToMessageID:     event.SourceMessageID,
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
		// trace_id is derived from event_id so all audit/state rows produced for
		// the same webhook share a trace (idempotent on retries).
		TraceID:   tracepkg.DeriveTraceID(event.EventID),
		CreatedAt: s.now(),
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

	// Handle question-card option selection (format "co:N:bareID") before the regular decode.
	if optionIdx, draftID, idempotencyKey, choiceOK := contracts.CallbackDataDecodeChoice(cbq.Data); choiceOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return s.handleChoiceOption(ctx, event, tenantID, cbq, optionIdx, draftID, idempotencyKey)
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

// outgoingUpdateID derives a unique negative int64 from a decisionID for synthetic outgoing
// messages. Negative values never collide with real Telegram update IDs (always positive).
func outgoingUpdateID(decisionID string) int64 {
	h := sha256.Sum256([]byte(decisionID))
	// Take first 8 bytes as uint64, shift to stay in int64 range, then negate.
	v := int64(uint64(h[0])<<56 | uint64(h[1])<<48 | uint64(h[2])<<40 | uint64(h[3])<<32 |
		uint64(h[4])<<24 | uint64(h[5])<<16 | uint64(h[6])<<8 | uint64(h[7]))
	if v == 0 {
		v = -1
	}
	if v > 0 {
		v = -v
	}
	return v
}

// saveOutgoingMessage persists a synthetic outgoing message and a virtual sent draft so the
// Mini App can display the AI auto-reply in chat history and label it as "DoubleU".
func (s *Service) saveOutgoingMessage(event contracts.NormalizedTelegramEvent, ownerID, sentMsgID, text, decisionID string) {
	if ownerID == "" || sentMsgID == "" {
		return
	}
	s.repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:              "out:" + decisionID,
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      sentMsgID,
		SenderID:             ownerID,
		MessageText:          text,
		MessageType:          "text",
		TelegramUpdateID:     outgoingUpdateID(decisionID),
		CreatedAt:            s.now(),
	})
	// Virtual draft lets DraftBySentMessageID recognise this as a DoubleU message.
	s.repository.SaveDraft(store.DraftRecord{
		DraftID:              "draft:" + decisionID,
		DecisionID:           decisionID,
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      event.SourceMessageID,
		ResponseText:         text,
		Status:               store.DraftSent,
		SentMessageID:        sentMsgID,
		CreatedAt:            s.now(),
		UpdatedAt:            s.now(),
	})
}

// saveOutgoingMessageFromDraft persists a synthetic outgoing message for an approved draft so
// the Mini App can display it in chat history.
func (s *Service) saveOutgoingMessageFromDraft(draft store.DraftRecord) {
	if draft.SentMessageID == "" {
		return
	}
	ownerID := ""
	if conn, ok := s.repository.BusinessConnection(draft.BusinessConnectionID); ok {
		ownerID = conn.UserID
	}
	if ownerID == "" {
		if strings.HasPrefix(draft.TenantID, "tenant:user:") {
			ownerID = strings.TrimPrefix(draft.TenantID, "tenant:user:")
		} else if ownerTenant := s.repository.OwnerTenantForBusinessConnection(draft.BusinessConnectionID); strings.HasPrefix(ownerTenant, "tenant:user:") {
			ownerID = strings.TrimPrefix(ownerTenant, "tenant:user:")
		}
	}
	if ownerID == "" {
		return
	}
	s.repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:              "out:" + draft.DecisionID,
		TenantID:             draft.TenantID,
		ChatID:               draft.ChatID,
		BusinessConnectionID: draft.BusinessConnectionID,
		SourceMessageID:      draft.SentMessageID,
		SenderID:             ownerID,
		MessageText:          draft.ResponseText,
		MessageType:          "text",
		TelegramUpdateID:     outgoingUpdateID(draft.DecisionID),
		CreatedAt:            s.now(),
	})
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

// episodeMessageLimit is the maximum number of messages allowed in a single episode.
// When a chat accumulates this many messages the episode is closed immediately
// (design §6.2 — episode lifecycle trigger: message_limit).
const episodeMessageLimit = 40

// summarizeChatDay compresses one day's conversation for a chat into an episode summary.
// closureReason records what triggered the episode close: "silence_gap", "message_limit",
// or "daily_summary". Skips if a summary for that date already exists. Safe to call concurrently.
func (s *Service) summarizeChatDay(ctx context.Context, tenantID, chatID string, date time.Time, closureReason string) {
	settings := s.repository.ChatSettings(tenantID, chatID)
	if settings.PrivacyFlags.Contains(contracts.PrivacyNoSummary) {
		return
	}
	if s.repository.EpisodeSummaryExistsForDate(tenantID, chatID, date) {
		return
	}
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := startOfDay.Add(24 * time.Hour)
	messages := s.repository.MessagesByDateRange(tenantID, chatID, startOfDay, endOfDay)
	if len(messages) < 3 {
		return
	}
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		if t := strings.TrimSpace(m.Event.MessageText); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return
	}
	summarizer, ok := s.ai.(aiclient.ChatSummarizer)
	if !ok {
		return
	}
	summ, err := summarizer.SummarizeChat(ctx, contracts.ChatSummaryRequest{
		Messages:     texts,
		PrivacyFlags: settings.PrivacyFlags,
	})
	if err != nil || summ.BlockedByPrivacy || strings.TrimSpace(summ.Summary) == "" {
		return
	}
	s.repository.SaveEpisodeSummary(store.EpisodeSummaryRecord{
		TenantID:      tenantID,
		ChatID:        chatID,
		PeriodStart:   startOfDay,
		PeriodEnd:     endOfDay,
		Content:       summ.Summary,
		MessageCount:  len(messages),
		ClosureReason: closureReason,
	})
	s.logger.Info("episode summary saved",
		"tenant_id", tenantID, "chat_id", chatID,
		"date", date.UTC().Format("2006-01-02"),
		"closure_reason", closureReason,
	)
	if len(summ.Recommendations) > 0 {
		s.sendRecommendationsToOwner(ctx, tenantID, chatID, summ.Recommendations)
	}
}

// maxSummaryConcurrency caps simultaneous LLM summarisation calls across all tenants.
// Each call blocks on a remote LLM endpoint; uncapped fan-out at scale causes a request storm.
const maxSummaryConcurrency = 8

// runDailySummariesForDate triggers summarizeChatDay for every active chat on the given date.
// Concurrency is bounded by maxSummaryConcurrency to prevent an LLM request storm.
func (s *Service) runDailySummariesForDate(ctx context.Context, date time.Time) {
	tenantIDs := s.repository.AllActiveTenantIDs()
	s.logger.Info("daily summary run started", "date", date.UTC().Format("2006-01-02"), "tenant_count", len(tenantIDs))
	sem := make(chan struct{}, maxSummaryConcurrency)
	for _, tenantID := range tenantIDs {
		for _, chat := range s.repository.ChatsForTenant(tenantID) {
			tenantID, chatID := tenantID, chat.ChatID // capture loop vars
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				s.summarizeChatDay(ctx, tenantID, chatID, date, "daily_summary")
			}()
		}
	}
	// Drain the semaphore to wait for all goroutines before returning.
	for i := 0; i < maxSummaryConcurrency; i++ {
		sem <- struct{}{}
	}
}

// closeStaleEpisodes materialises episode summaries for any chat whose active episode
// has had no messages for > 6 hours, then removes the active_episodes row.
// Concurrency is bounded by maxSummaryConcurrency to prevent an LLM request storm.
// Safe to call concurrently; episode closure is idempotent via EpisodeSummaryExistsForDate.
func (s *Service) closeStaleEpisodes(ctx context.Context) {
	tenantIDs := s.repository.AllActiveTenantIDs()
	sem := make(chan struct{}, maxSummaryConcurrency)
	for _, tenantID := range tenantIDs {
		for _, chat := range s.repository.ChatsForTenant(tenantID) {
			ep, ok := s.repository.ActiveEpisodeByChat(tenantID, chat.ChatID)
			if !ok {
				continue
			}
			if s.now().Sub(ep.LastMessageAt) < 6*time.Hour {
				continue
			}
			tenantID, chatID, lastMsg := tenantID, chat.ChatID, ep.LastMessageAt
			// Close the active episode record before launching the goroutine so a
			// second closeStaleEpisodes call for the same chat cannot race.
			s.repository.CloseActiveEpisode(tenantID, chatID)
			s.logger.Info("stale episode closed",
				"tenant_id", tenantID,
				"chat_id", chatID,
				"idle_hours", s.now().Sub(ep.LastMessageAt).Hours(),
			)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				s.summarizeChatDay(ctx, tenantID, chatID, lastMsg, "silence_gap")
			}()
		}
	}
	// Drain the semaphore so callers can observe all goroutines completing.
	for i := 0; i < maxSummaryConcurrency; i++ {
		sem <- struct{}{}
	}
}

// StartEpisodeCloser runs every 15 minutes and closes active episodes that have been
// idle for more than 6 hours (§6.2 — episode lifecycle trigger: silence_gap).
func (s *Service) StartEpisodeCloser(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.closeStaleEpisodes(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// StartDailySummaryScheduler blocks until ctx is cancelled, waking at midnight UTC each day
// to compress the previous day's conversations into long-term memory.
func (s *Service) StartDailySummaryScheduler(ctx context.Context) {
	for {
		now := s.now().UTC()
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		select {
		case <-time.After(time.Until(nextMidnight)):
			yesterday := nextMidnight.Add(-24 * time.Hour)
			s.runDailySummariesForDate(ctx, yesterday)
		case <-ctx.Done():
			return
		}
	}
}

// StartEmbeddingWorker runs every 5 minutes and generates embeddings for any semantic facts
// that were inserted without one.  Runs in a separate goroutine; safe to call concurrently.
// Activates only when the AI client implements TextEmbedder; otherwise it is a no-op.
func (s *Service) StartEmbeddingWorker(ctx context.Context) {
	embedder, ok := s.ai.(aiclient.TextEmbedder)
	if !ok {
		return // AI client does not support embedding generation.
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runEmbeddingBatch(ctx, embedder)
		case <-ctx.Done():
			return
		}
	}
}

// runEmbeddingBatch fetches facts without embeddings (up to 50 at a time), calls the
// Python embedding endpoint, and persists vectors into fact_embeddings.
func (s *Service) runEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder) {
	const batchSize = 50
	facts := s.repository.FactsWithoutEmbeddings(batchSize)
	if len(facts) == 0 {
		return
	}
	texts := make([]string, len(facts))
	for i, f := range facts {
		texts[i] = f.FactText
	}
	resp, err := embedder.EmbedTexts(ctx, aiclient.EmbedTextsRequest{
		Texts: texts,
		Model: "qwen/qwen3-embedding-8b",
	})
	if err != nil || resp.ModelUsed == "none" || resp.ModelUsed == "error" {
		s.logger.Warn("embedding batch failed", "error", err, "model", resp.ModelUsed)
		return
	}
	for i, f := range facts {
		if i >= len(resp.Embeddings) {
			break
		}
		emb := resp.Embeddings[i]
		if len(emb) == 0 {
			continue
		}
		s.repository.SaveFactEmbedding(f.ID, f.TenantID, f.ScopeKind, f.ChatID, emb, resp.ModelUsed)
	}
	s.logger.Info("embedding batch completed",
		"facts_embedded", len(facts),
		"model", resp.ModelUsed,
	)
}

// sendRecommendationsToOwner delivers proactive recommendations to the owner's bot chat.
// Recommendations are informational and are not persisted as memory facts — they are
// derived from conversation history and will surface again at the next summarization run.
func (s *Service) sendRecommendationsToOwner(ctx context.Context, tenantID, chatID string, recommendations []string) {
	ownerChatID := strings.TrimPrefix(tenantID, "tenant:user:")
	if ownerChatID == tenantID {
		return // not a "tenant:user:USERID" tenant
	}
	chat, ok := s.repository.Chat(tenantID, chatID)
	if !ok {
		return
	}
	lines := make([]string, 0, len(recommendations)+2)
	lines = append(lines, fmt.Sprintf("💡 Рекомендации для чата с %s:", chatDisplayName(chat)))
	lines = append(lines, "")
	for _, rec := range recommendations {
		lines = append(lines, "• "+rec)
	}
	lines = append(lines, "")
	lines = append(lines, "(на основе истории общения)")
	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID: tenantID,
		ChatID:   ownerChatID,
		Text:     strings.Join(lines, "\n"),
	})
}

// sendQuestionToOwner delivers a question card with AI-suggested option buttons to the owner's bot chat.
func (s *Service) sendQuestionToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerID string) {
	connection, ok := s.repository.BusinessConnection(event.BusinessConnectionID)
	ownerChatID := ""
	if ok && connection.UserChatID != "" {
		ownerChatID = connection.UserChatID
	} else {
		ownerChatID = s.inferOwnerChatID(event.BusinessConnectionID)
		if ownerChatID != "" {
			s.repository.SaveBusinessConnection(store.BusinessConnectionRecord{
				ConnectionID: event.BusinessConnectionID,
				TenantID:     "tenant:user:" + ownerChatID,
				UserID:       ownerChatID,
				UserChatID:   ownerChatID,
				CanReply:     true,
				IsEnabled:    true,
			})
		}
	}
	if ownerChatID == "" {
		s.logger.Warn("cannot deliver question card to owner: chat ID not found", "business_connection_id", event.BusinessConnectionID, "draft_id", draft.DraftID)
		return
	}
	cardText := bot.QuestionCardText(event.MessageText, event.ChatDisplayName)
	keyboard := bot.QuestionOptionsKeyboard(draft.DraftID, draft.SuggestedOptions)
	msg := telegram.OutboundMessage{
		TenantID:        event.TenantID,
		ChatID:          ownerChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      draft.DecisionID,
		Text:            cardText,
		ReplyMarkup:     keyboard,
	}
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

// handleChoiceOption processes an ApprovalChooseOption callback: the owner has selected one of the
// AI-suggested options on a question card. The selection is fed back to the AI as a hint to
// generate a full reply, then that reply is auto-sent or presented as a regular draft.
func (s *Service) handleChoiceOption(ctx context.Context, event contracts.NormalizedTelegramEvent, tenantID string, cbq telegram.CallbackQueryUpdate, optionIdx int, draftID, idempotencyKey string) InboundResult {
	if s.repository.MarkCallback(idempotencyKey) {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_choice_callback", Duplicate: true}
	}
	draft, ok := s.repository.Draft(draftID)
	if !ok || draft.DraftType != "question" {
		s.logger.Warn("choice option: draft not found or not a question card", "draft_id", draftID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "choice_draft_not_found"}
	}
	if optionIdx >= len(draft.SuggestedOptions) {
		s.logger.Warn("choice option: index out of range", "draft_id", draftID, "option_idx", optionIdx, "options_count", len(draft.SuggestedOptions))
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "choice_option_out_of_range"}
	}
	optionText := draft.SuggestedOptions[optionIdx]
	s.logger.Info("choice option selected", "tenant_id", draft.TenantID, "chat_id", draft.ChatID, "draft_id", draftID, "option", optionText)

	// Remove the question card from the owner's bot chat.
	s.removeDraftCard(ctx, draft)

	// Convert to a regular reply draft and ask the AI to elaborate on the chosen option.
	draft.DraftType = "reply"
	draft.SuggestedOptions = nil
	hint := "Пользователь выбрал: " + optionText + ". Составь полноценный ответ на вопрос собеседника с учётом этого выбора."
	newDraft, err := s.regenerateDraftWithHint(ctx, draft, hint)
	if err != nil {
		s.logger.Error("choice option: ai hint failed, sending option text directly", "error", err, "draft_id", draftID)
		srcEvent := s.resolveSourceEvent(draft)
		_, _ = s.sendRecipient(ctx, srcEvent, draft.DecisionID+":choice_fallback", optionText)
		return InboundResult{Event: event, Decision: contracts.DecisionAutoReply, Reason: "choice_fallback_sent", DraftID: draftID}
	}

	settings := s.repository.ChatSettings(newDraft.TenantID, newDraft.ChatID)
	ownerID := ""
	if conn, connOK := s.repository.BusinessConnection(draft.BusinessConnectionID); connOK {
		ownerID = conn.UserID
	}
	srcEvent := s.resolveSourceEvent(newDraft)
	if settings.Mode == contracts.ChatModeAuto {
		_, _ = s.sendRecipient(ctx, srcEvent, newDraft.DecisionID, newDraft.ResponseText)
		s.saveOutgoingMessage(srcEvent, ownerID, "", newDraft.ResponseText, newDraft.DecisionID)
		if s.notify != nil {
			s.notify(newDraft.TenantID, "new_message", map[string]any{"chat_id": newDraft.ChatID})
		}
		return InboundResult{Event: event, Decision: contracts.DecisionAutoReply, Reason: "choice_auto_sent", DraftID: newDraft.DraftID}
	}
	s.sendDraftToOwner(ctx, srcEvent, newDraft, ownerID)
	if s.notify != nil {
		s.notify(newDraft.TenantID, "new_draft", map[string]any{
			"chat_id":       newDraft.ChatID,
			"draft_id":      newDraft.DraftID,
			"response_text": newDraft.ResponseText,
			"source_text":   newDraft.SourceMessageText,
			"created_at":    newDraft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return InboundResult{Event: event, Decision: contracts.DecisionDraft, Reason: "choice_draft_sent", DraftID: newDraft.DraftID}
}

// parseOptions splits a multi-line string into trimmed, non-empty option lines.
func parseOptions(text string) []string {
	lines := strings.Split(text, "\n")
	options := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			options = append(options, line)
		}
	}
	return options
}
