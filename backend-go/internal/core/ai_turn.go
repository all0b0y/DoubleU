package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	tracepkg "doubleu/backend-go/internal/trace"
)

// aiProcessMaxAttempts bounds process-message tries. A second attempt only runs
// for a transient failure — see processMessageWithRetry.
const aiProcessMaxAttempts = 2

// processMessageWithRetry calls the AI process-message endpoint, retrying once on
// a transient failure so a momentary blip (ai-service restart, connection reset,
// a fast 5xx) does not cost the contact their reply. A deadline-exceeded is NOT
// retried: it means the model is genuinely slow, so a fresh attempt would only
// run out the clock again and double the wait — the caller's graceful notice
// covers that case instead.
func (s *Service) processMessageWithRetry(ctx context.Context, event contracts.NormalizedTelegramEvent, request contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	var proposal contracts.AIProcessMessageResponse
	var err error
	for attempt := 1; attempt <= aiProcessMaxAttempts; attempt++ {
		proposal, err = s.ai.ProcessMessage(ctx, request)
		if err == nil {
			return proposal, nil
		}
		if attempt == aiProcessMaxAttempts || isTimeoutError(err) || ctx.Err() != nil {
			break
		}
		s.logger.Warn("ai analysis transient failure; retrying",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"attempt", attempt, "error", err)
	}
	return proposal, err
}

// isTimeoutError reports whether err is a client/context timeout (the model was
// simply too slow) rather than a transient, fast-failing error worth retrying.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) {
		return timeoutErr.Timeout()
	}
	return false
}

// processContactMessage is the transport-agnostic inbound pipeline shared by
// the Business webhook path and the bot polling path: persist → dedupe →
// episode upkeep → debounce → AI call → policy decision → dispatch.
// replyBlockNote, when non-empty, short-circuits AI processing with an
// owner-facing draft explaining why replies are blocked (Business-specific
// can_reply permission).
func (s *Service) processContactMessage(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, replyBlockNote string) (InboundResult, error) {
	// Redact credentials/PII before the message is stored, sent to the AI,
	// extracted into memory, or embedded — the single choke point both transports
	// funnel through (owner policy, 2026-07-04). Doing it here means the model
	// never sees the raw secret and so cannot echo it.
	if redacted, ok := redactSecrets(event.MessageText); ok {
		event.MessageText = redacted
		s.logger.Info("secrets redacted from inbound message",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"source_message_id", event.SourceMessageID)
	}
	settings := s.repository.ChatSettings(event.TenantID, event.ChatID)
	roleContextID := s.activeRoleContextIDForEvent(event)
	s.repository.SaveChat(store.ChatRecord{
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		DisplayName:          event.ChatDisplayName,
		ChatType:             event.ChatType,
		BusinessConnectionID: event.BusinessConnectionID,
		LastSourceMessageID:  event.SourceMessageID,
		LastSeenAt:           s.now(),
		Transport:            event.Transport,
	})
	// Persist the message row BEFORE any state/audit write so the DB foreign
	// keys from message_states and audit_events back to messages always resolve.
	// no_memory chats skip persistence — for those, AppendState / AppendAudit
	// silently fall back to in-memory tracking inside the repository.
	// quarantined: exact duplicate of a recent contact message → stored for audit
	// but excluded from every memory surface (window-20, episode summaries, extraction).
	quarantined := false
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		recent := s.recentMessagesForMemoryEvent(event, 5, roleContextID)
		quarantined = classifyQuarantine(event.MessageText, recent)
		s.repository.SaveMessageQuarantinedWithRoleContext(event, quarantined, roleContextID)
		if quarantined {
			s.logger.Info("message quarantined as duplicate",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"source_message_id", event.SourceMessageID,
			)
		}
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
			RoleContextID: roleContextID,
		}
		if existing, ok := s.activeEpisodeForMemoryEvent(event, roleContextID); ok {
			ep.StartedAt = existing.StartedAt
			ep.MessageCount = existing.MessageCount + 1
			if ep.RoleContextID == 0 {
				ep.RoleContextID = existing.RoleContextID
			}
		}
		s.repository.UpsertActiveEpisode(ep)

		// Topic tagging no longer fires a separate LLM call per message: the
		// structured draft response carries the topic (ARCH-4), persisted after
		// the process-message call below. /ai/tag-topic remains for episode workers.

		// Message-limit trigger: close and summarise the episode when it reaches
		// episodeMessageLimit messages (design §6.2). The closure is synchronous so a
		// concurrent handler cannot create a duplicate; summarisation runs in a goroutine.
		if ep.MessageCount >= episodeMessageLimit {
			s.repository.CloseActiveEpisode(event.TenantID, event.ChatID)
			startedAt, closedAt := ep.StartedAt, ep.LastMessageAt
			span := closedAt.Sub(startedAt)
			if span < minEpisodeSpan {
				// A sub-minEpisodeSpan burst of 40 is a flood, not a conversation.
				// Closing the episode resets the runtime counter; real content (if any)
				// is captured later by the time-based daily fallback (summarizeChatDay).
				s.logger.Info("episode closed by flood, summary skipped",
					"tenant_id", event.TenantID,
					"chat_id", event.ChatID,
					"message_count", ep.MessageCount,
					"span_seconds", span.Seconds(),
				)
			} else {
				s.logger.Info("episode closed by message limit",
					"tenant_id", event.TenantID,
					"chat_id", event.ChatID,
					"message_count", ep.MessageCount,
				)
				tenantID, chatID := event.TenantID, event.ChatID
				capturedRoleContextID := roleContextID
				s.runAsyncLLMTask(event.TenantID, func() {
					s.summarizeEpisode(context.Background(), tenantID, chatID, startedAt, closedAt, "message_count", capturedRoleContextID)
				})
			}
		}
	}
	s.repository.IncrementChatMessageCount(event.TenantID, event.ChatID)
	if s.notify != nil {
		s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StatePolicyChecked)
	if isOwnerSideMessage(event, ownerID, event.Transport) {
		// The owner replied themselves: the open question is answered and no
		// owner action is pending any more (§6.2 lifecycle, MEM-5). On bot
		// transport this only fires for synthetic system messages — a human
		// (incl. the owner testing the bot) is a contact the AI answers.
		s.repository.ClearEpisodeRuntimeState(event.TenantID, event.ChatID)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "owner_message_observed"))
		s.logger.Info("owner-authored business message stored as context without ai reply",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"source_message_id", event.SourceMessageID,
			"sender_id", event.SenderID,
		)
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_observed"}, nil
	}

	if form, ok := s.repository.ActiveInteractionFormAt(event.TenantID, event.ChatID, s.now()); ok {
		nextEvent, formResult, err := s.handleInteractionFormTextAnswer(ctx, event, ownerID, form)
		if err != nil {
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
			s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "form_answer_error:"+err.Error()))
			s.logger.Error("interaction form answer failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "form_id", form.ID, "error", err)
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "form_answer_error"}, nil
		}
		if formResult != nil {
			s.repository.AppendAudit(s.auditEvent(event, formResult.Decision, formResult.Reason))
			return *formResult, nil
		}
		event = nextEvent
	}

	// Per chat+sender rate limit: owner messages are already handled above (returned early).
	// Bot chats key by tenant|chat|sender (each user gets an independent bucket).
	// Business chats key by tenant|chat. Excess messages are quarantined and skipped —
	// backpressure, not a block; the chat remains open for the next in-window message.
	if !s.chatLimiterFor(event).Allow() {
		if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
			s.repository.SaveMessageQuarantinedWithRoleContext(event, true, roleContextID)
		}
		s.logger.Info("message rate_limited",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"sender_id", event.SenderID,
		)
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "rate_limited"}, nil
	}

	if replyBlockNote != "" {
		draft := s.createDraftWithRoleContext(event, event.EventID, replyBlockNote, contracts.TextFormatPlain, roleContextID)
		s.repository.SaveDraft(draft)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionAskOwner, "business_connection_cannot_reply"))
		s.logger.Info("ai analysis blocked by telegram reply permission", "tenant_id", event.TenantID, "chat_id", event.ChatID)
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

	aiRole := s.aiRoleFor(event.TenantID, event.ChatID)
	effectiveSettings, botPersonaAuto := botPersonaEffectiveSettings(event, settings, aiRole)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIRequested)
	s.logger.Info("ai analysis started",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"mode", settings.Mode,
		"effective_mode", effectiveSettings.Mode,
	)
	if s.notify != nil {
		s.notify(event.TenantID, "ai_thinking", map[string]any{"chat_id": event.ChatID})
	}
	memoryContext, recentRecords := s.conversationContextAndMessages(ctx, event, ownerID, 20)
	request := contracts.AIProcessMessageRequest{
		RequestID:     event.EventID,
		TraceID:       tracepkg.DeriveTraceID(event.EventID),
		Event:         event,
		ChatSettings:  effectiveSettings,
		MemoryContext: memoryContext,
		CurrentTime:   s.now().UTC().Format(time.RFC3339),
	}
	if personaSpeaksAsOwner(aiRole) {
		request.FeedbackContext = s.feedbackContext(event.TenantID, event.ChatID, 5)
	}
	if profile, ok := s.repository.ContactProfile(event.TenantID, event.ChatID); ok {
		request.ContactProfile = &profile
	}
	// AI role: the persona assigned to this chat (nil → default assistant persona).
	request.AIRole = aiRole
	if shouldSurfaceScheduledJobs(aiRole) {
		request.ExistingScheduledJobs = existingScheduledJobs(s.repository.ListJobs(event.TenantID, event.ChatID))
	}
	// User Layer: owner identity and preferences (§3).
	// Also pre-compute delivery settings from the same read so sendRecipient below
	// does not need a second UserSettings lookup on this hot path.
	var hotDelivery deliverySettings
	if us, ok := s.repository.UserSettings(event.TenantID); ok {
		hotDelivery = s.deliverySettingsFromUser(event, &us)
		if personaSpeaksAsOwner(aiRole) {
			request.UserSettings = &us
		} else {
			// Strip owner identity for bot-persona chats; keep operational defaults.
			stripped := us
			stripped.DisplayName = ""
			stripped.Bio = ""
			request.UserSettings = &stripped
		}
	} else {
		hotDelivery = s.deliverySettingsFromUser(event, nil)
	}
	// Runtime State Layer: current open episode data for AI awareness (§6.2).
	if ep, ok := s.activeEpisodeForMemoryEvent(event, roleContextID); ok {
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
		"effective_mode", effectiveSettings.Mode,
		"privacy_flags", settings.PrivacyFlags,
		"memory_context_count", len(request.MemoryContext),
	)
	proposal, err := s.processMessageWithRetry(ctx, event, request)
	if s.notify != nil {
		s.notify(event.TenantID, "ai_done", map[string]any{"chat_id": event.ChatID})
	}
	if err != nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "ai_process_error:"+err.Error()))
		s.logger.Error("ai analysis failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
		s.notifyAIError(ctx, event)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "ai_process_error"}, nil
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIGenerated)
	s.cancelAIScheduledJobs(event, proposal.CancelledJobIDs)
	if err := s.saveAIScheduledJobs(event, settings, proposal.ScheduledJobs); err != nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "scheduled_job_persist_error:"+err.Error()))
		s.logger.Error("scheduled job persistence failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
		s.notifyAIError(ctx, event)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "scheduled_job_persist_error"}, nil
	}
	// Persist fast (regex) facts returned by process-message synchronously.
	// LLM-based extraction is fired asynchronously below, after the draft is dispatched,
	// so it never adds latency to the reply path (see backlog §7.1).
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) && !s.pythonOwnsMemory() {
		s.saveExtractedFactsWithRoleContext(event, proposal.MemoryUsed, roleContextID)
	}
	// Auto-apply AI-proposed profile-field updates when the role opts in (the
	// default) and memory is allowed. The suggest_profile_update tool has already
	// committed any suggestions to the DB during process-message, so they are
	// visible here. Best-effort: a failure never blocks the reply. When the role
	// has auto-learn off, suggestions stay pending for manual owner review.
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		if role, ok := s.resolveRole(event.TenantID, event.ChatID); !ok || role.AutoLearnProfile {
			if n, err := s.repository.AutoApplyProfileSuggestions(event.TenantID, event.ChatID); err != nil {
				s.logger.Warn("auto-apply profile suggestions failed",
					"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
			} else if n > 0 {
				s.logger.Info("auto-applied profile suggestions",
					"tenant_id", event.TenantID, "chat_id", event.ChatID, "count", n)
			}
		}
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

	decision, reason := s.engine.Decide(effectiveSettings, proposal)
	if botPersonaAuto && decision == contracts.DecisionAutoReply {
		reason = "bot_persona_low_risk_send"
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDecisionMade)
	s.logger.Info("backend final decision made", "tenant_id", event.TenantID, "chat_id", event.ChatID, "decision", decision, "reason", reason)
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		s.updateEpisodeRuntime(event, proposal, decision)
	}
	result := InboundResult{Event: event, Decision: decision, Reason: reason}
	interactionQuestions, contactInteraction := s.contactInteractionQuestions(event, proposal)
	if contactInteraction {
		if _, err := s.startInteractionForm(ctx, event, ownerID, proposal, interactionQuestions, hotDelivery); err != nil {
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
			s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "form_start_error:"+err.Error()))
			s.logger.Error("interaction form start failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "form_start_error"}, nil
		}
		result.Decision = contracts.DecisionAutoReply
		result.Reason = "form_started"
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateSentToRecipient)
		s.repository.ClearEpisodeRuntimeState(event.TenantID, event.ChatID)
		if s.notify != nil {
			s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
		}
	} else {
		switch decision {
		case contracts.DecisionAutoReply:
			decisionID := event.EventID
			allowStreaming := proposal.Intent != "choice_question"
			sentMsgID, err := s.sendRecipientWithMarkup(ctx, event, decisionID, proposal.ResponseText, proposal.TextFormat, allowStreaming, hotDelivery, nil)
			if err != nil {
				s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
				s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "send_error:"+err.Error()))
				s.logger.Error("telegram send failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
				return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "send_error"}, nil
			}
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateSentToRecipient)
			s.logger.Info("telegram auto reply sent", "tenant_id", event.TenantID, "chat_id", event.ChatID, "source_message_id", event.SourceMessageID)
			s.saveOutgoingMessageWithRoleContext(event, ownerID, sentMsgID, proposal.ResponseText, event.EventID, proposal.TextFormat, roleContextID)
			// A reply went out on the owner's behalf: nothing is awaited from the owner.
			s.repository.ClearEpisodeRuntimeState(event.TenantID, event.ChatID)
			if s.notify != nil {
				s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
			}
		case contracts.DecisionDraft, contracts.DecisionAskOwner:
			// Critical-risk warning (ARCH-1): the note is owner-facing only. The draft
			// row carries an empty ResponseText and DraftType="warning" so no approval
			// path can ever deliver the service text to the contact.
			if ownerNote := s.ownerNoteFor(proposal); decision == contracts.DecisionAskOwner && ownerNote != "" {
				draft := s.createDraftWithRoleContext(event, event.EventID, "", contracts.TextFormatPlain, roleContextID)
				draft.DraftType = "warning"
				s.repository.SaveDraft(draft)
				s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
				s.logger.Info("warning card prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID)
				result.DraftID = draft.DraftID
				if s.notify != nil {
					s.notify(draft.TenantID, "new_draft", map[string]any{
						"chat_id":     draft.ChatID,
						"draft_id":    draft.DraftID,
						"draft_type":  draft.DraftType,
						"owner_note":  ownerNote,
						"source_text": draft.SourceMessageText,
						"created_at":  draft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
					})
				} else {
					s.sendWarningToOwner(ctx, event, draft, ownerNote)
				}
				break
			}
			// When the AI flags the incoming message as a choice question, replace the normal
			// draft flow with an interactive question card that lets the owner pick an option.
			if proposal.Intent == "choice_question" && proposal.ResponsePlan == nil {
				// Structured output carries options directly (ARCH-4); the line-split
				// of ResponseText remains as a legacy fallback.
				options := proposal.Options
				if len(options) == 0 {
					options = parseOptions(proposal.ResponseText)
				}
				if len(options) > 0 {
					draft := s.createDraftWithRoleContext(event, event.EventID, "", contracts.TextFormatPlain, roleContextID)
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
			draft := s.createDraftWithRoleContext(event, event.EventID, proposal.ResponseText, proposal.TextFormat, roleContextID)
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
			// The AI produced no usable reply (transient model failure). Tell the
			// owner so the thinking indicator resolves into a retry notice instead of
			// silently vanishing, which reads as the bot being dead.
			s.notifyAIError(ctx, event)
		}
	}
	s.repository.AppendAudit(s.auditEvent(event, decision, reason))

	// Async LLM fact extraction: fires after the draft is dispatched so it never blocks
	// the reply path.  Prefers TypedMemoryExtractor (/ai/extract-facts-typed) for proper
	// scope routing; falls back to MemoryExtractor (/ai/extract-memory) if unavailable.
	// Only runs when the AI client supports extraction, memory is not disabled, the message
	// is not quarantined (spam duplicate), and it passes the cheap fact-bearing pre-filter (MEM-13).
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) && !quarantined && shouldExtractFacts(event.MessageText) {
		privacyStrings := make([]string, len(settings.PrivacyFlags))
		for i, f := range settings.PrivacyFlags {
			privacyStrings[i] = string(f)
		}
		if typedExtractor, ok := s.ai.(aiclient.TypedMemoryExtractor); ok {
			// Context window for multi-turn fact detection (e.g. "учусь в МГУ"
			// followed by "на медицинском факультете"): reuse the messages already
			// fetched for the prompt (ARCH-8) and keep the last 5 of them.
			recentTexts := make([]string, 0, 5)
			for _, m := range recentRecords {
				if t := strings.TrimSpace(m.Event.MessageText); t != "" && m.Event.SourceMessageID != event.SourceMessageID {
					recentTexts = append(recentTexts, t)
				}
			}
			if len(recentTexts) > 5 {
				recentTexts = recentTexts[len(recentTexts)-5:]
			}
			s.runAsyncLLMTask(event.TenantID, func() {
				extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				start := s.now()
				resp, err := typedExtractor.ExtractFactsTyped(extractCtx, aiclient.TypedExtractionRequest{
					MessageText:    event.MessageText,
					TenantID:       event.TenantID,
					ChatID:         event.ChatID,
					PrivacyFlags:   privacyStrings,
					RecentMessages: recentTexts,
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
						RoleContextID:   roleContextID,
					})
					return
				}
				if resp.BlockedByPrivacy {
					return
				}
				// AI_MEMORY_SOURCE=python: the agent runtime already wrote the
				// facts/suggestions through upsert_semantic_fact; Go only logs.
				outcome := extractionOutcome{}
				if !s.pythonOwnsMemory() {
					outcome = s.saveTypedFacts(event, resp.Facts, roleContextID)
					s.saveProfileSuggestions(extractCtx, event, resp.ProfileSuggestions)
				}
				// Persist the attempt so a returned empty fact set is observable and
				// distinguishable from a silent failure (extraction observability).
				// Health-fact texts are redacted to ID markers first (§10.3, MEM-8).
				s.repository.LogExtraction(store.ExtractionLogRecord{
					TenantID:        event.TenantID,
					ChatID:          event.ChatID,
					InputMessageIDs: []string{event.SourceMessageID},
					ModelUsed:       resp.ModelUsed,
					RawOutput:       redactHealthFacts(resp.RawOutput, resp.Facts, outcome.HealthFactIDs),
					FactsCreated:    outcome.CreatedIDs,
					FactsSkipped:    outcome.Skipped,
					FactsMerged:     resp.DeterministicFactsCount,
					DurationMS:      durationMS,
					RoleContextID:   roleContextID,
				})
			})
		} else if extractor, ok := s.ai.(aiclient.MemoryExtractor); ok {
			s.runAsyncLLMTask(event.TenantID, func() {
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
				if !resp.BlockedByPrivacy && !s.pythonOwnsMemory() {
					s.saveExtractedFactsWithRoleContext(event, resp.Memories, roleContextID)
				}
			})
		}
	}

	return result, nil
}
