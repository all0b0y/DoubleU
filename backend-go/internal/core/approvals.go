package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/bot"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
	tracepkg "doubleu/backend-go/internal/trace"
)

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
	// Warning cards carry an owner-facing service note, not a reply. Every
	// mutating/sending action is blocked so no callback path can deliver the
	// note (or an edit of it) to the contact — only dismissal works (ARCH-1).
	if draft.DraftType == "warning" && payload.Action != contracts.ApprovalReject {
		return ApprovalResult{}, fmt.Errorf("warning card supports only reject")
	}
	now := s.now()
	switch payload.Action {
	case contracts.ApprovalApprove:
		if strings.TrimSpace(draft.ResponseText) == "" {
			return ApprovalResult{}, fmt.Errorf("draft has no sendable text")
		}
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateApprovedByOwner)
		approvalEvent := s.resolveSourceEvent(draft)
		sentMsgID, err := s.sendRecipientWithMarkup(ctx, approvalEvent, draft.DecisionID, draft.ResponseText, draft.TextFormat, false, s.deliverySettingsFor(approvalEvent), nil)
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
		// The approved reply answers the open question; owner action completed (MEM-5).
		s.repository.ClearEpisodeRuntimeState(draft.TenantID, draft.ChatID)
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
		s.repository.RecordSkillFeedbackForTurn(draft.TenantID, draft.ChatID, draft.SourceMessageID, "up")
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
			s.repository.RecordSkillFeedbackForTurn(draft.TenantID, draft.ChatID, draft.SourceMessageID, "down")
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
		s.repository.RecordSkillFeedbackForTurn(draft.TenantID, draft.ChatID, draft.SourceMessageID, "up")
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
		s.repository.RecordSkillFeedbackForTurn(draft.TenantID, draft.ChatID, draft.SourceMessageID, "down")
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
		}
		s.stripRatingButtons(ctx, draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: "feedback_saved"}, nil
	default:
		return ApprovalResult{}, fmt.Errorf("unsupported action %q", payload.Action)
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
		CurrentTime:   s.now().UTC().Format(time.RFC3339),
	}
	if profile, ok := s.repository.ContactProfile(draft.TenantID, draft.ChatID); ok {
		regenRequest.ContactProfile = &profile
	}
	regenRequest.AIRole = s.aiRoleFor(draft.TenantID, draft.ChatID)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIRequested)
	proposal, err := s.ai.ProcessMessage(ctx, regenRequest)
	if err != nil {
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
		return store.DraftRecord{}, err
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIGenerated)
	draft.ResponseText = proposal.ResponseText
	draft.TextFormat = proposal.TextFormat.Normalize()
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
		RequestID:     draft.DecisionID + ":hint",
		TraceID:       tracepkg.DeriveTraceID(event.EventID),
		Event:         event,
		ChatSettings:  settings,
		MemoryContext: hintContext,
		CurrentTime:   s.now().UTC().Format(time.RFC3339),
	}
	if profile, ok := s.repository.ContactProfile(draft.TenantID, draft.ChatID); ok {
		hintRequest.ContactProfile = &profile
	}
	hintRequest.AIRole = s.aiRoleFor(draft.TenantID, draft.ChatID)
	if personaSpeaksAsOwner(hintRequest.AIRole) {
		hintRequest.FeedbackContext = s.feedbackContext(draft.TenantID, draft.ChatID, 5)
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
	draft.TextFormat = proposal.TextFormat.Normalize()
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

// handleCallbackQuery processes an inline button press from the owner.
func (s *Service) handleCallbackQuery(ctx context.Context, cbq telegram.CallbackQueryUpdate) InboundResult {
	tenantID := tenantForUser(cbq.SenderID)
	event := onboardingEvent(tenantID, cbq.ChatID, cbq.OriginalMsgID, cbq.UpdateID)
	callbackSender := s.sender
	if cbq.Transport == contracts.TransportBot && s.botSenders != nil {
		if sender, ok := s.botSenders.SenderFor(cbq.TenantID, cbq.BotID); ok {
			callbackSender = sender
		}
	}

	if duplicate := s.repository.MarkReceive(cbq.ReceiveIdempotencyKey); duplicate {
		_ = callbackSender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_callback_query", Duplicate: true}
	}

	if formID, questionIdx, optionIdx, freeText, idempotencyKey, qformOK := contracts.CallbackDataDecodeQForm(cbq.Data); qformOK {
		_ = callbackSender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return s.handleQFormCallback(ctx, cbq, callbackSender, formID, questionIdx, optionIdx, freeText, idempotencyKey)
	}

	// Handle question-card option selection (format "co:N:bareID") before the regular decode.
	if optionIdx, draftID, idempotencyKey, choiceOK := contracts.CallbackDataDecodeChoice(cbq.Data); choiceOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return s.handleChoiceOption(ctx, event, tenantID, cbq, optionIdx, draftID, idempotencyKey)
	}

	// Forget-chat confirmation flow (§10.2, MEM-2).
	if forgetChatID, forgetOK := contracts.CallbackDataDecodeForget(cbq.Data); forgetOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		text := "🗑 Память чата удалена безвозвратно."
		if err := s.repository.ForgetChat(tenantID, forgetChatID); err != nil {
			s.logger.Error("forget chat failed", "tenant_id", tenantID, "chat_id", forgetChatID, "error", err)
			text = "Не удалось удалить память чата: " + err.Error()
		} else {
			s.logger.Info("chat memory forgotten", "tenant_id", tenantID, "chat_id", forgetChatID)
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: cbq.ChatID, Text: text})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_chat_confirmed"}
	}
	if cbq.Data == contracts.ForgetCancelCallbackData {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: cbq.ChatID, Text: "Удаление отменено."})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_chat_cancelled"}
	}

	// Profile-suggestion review (§4.5, MEM-4).
	if suggestionID, accept, suggestionOK := contracts.CallbackDataDecodeSuggestion(cbq.Data); suggestionOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		text := "✅ Предложение принято — профиль обновлён."
		if !accept {
			text = "Предложение отклонено."
		}
		if err := s.repository.ResolveSuggestion(tenantID, suggestionID, accept); err != nil {
			s.logger.Warn("suggestion resolution failed", "tenant_id", tenantID, "suggestion_id", suggestionID, "error", err)
			text = "Предложение уже обработано или не найдено."
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: cbq.ChatID, Text: text})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "suggestion_resolved"}
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
