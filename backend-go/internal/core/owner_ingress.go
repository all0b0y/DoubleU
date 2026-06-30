package core

import (
	"context"
	"fmt"
	"strings"

	"doubleu/backend-go/internal/bot"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func isOwnerAuthoredMessage(event contracts.NormalizedTelegramEvent, ownerID string) bool {
	return ownerID != "" && event.SenderID == ownerID
}

// isOwnerSideMessage reports whether a message belongs to the owner's side of
// the conversation — i.e. it should be treated as written on the owner's behalf
// (suppress an AI reply, label OWNER in the AI context, render "outgoing" in the
// Mini App). The rule depends on transport:
//
//   - business: the bot acts AS the owner's own Telegram account, so anything
//     the owner typed (SenderID == ownerID) is owner-side.
//   - bot: the bot is a SEPARATE identity. A human DMing the bot — including the
//     owner testing their own bot — is an incoming contact the AI must answer.
//     Only system-emitted messages (synthetic "out:" events: auto-replies,
//     approved drafts, app/manual sends) are owner-side.
func isOwnerSideMessage(event contracts.NormalizedTelegramEvent, ownerID, transport string) bool {
	if transport == contracts.TransportBot {
		return strings.HasPrefix(event.EventID, "out:")
	}
	return isOwnerAuthoredMessage(event, ownerID)
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
	case contracts.PendingInputForgetAll:
		// §10.2: full erasure runs only on the exact confirmation phrase.
		if strings.TrimSpace(msg.Text) != contracts.ForgetAllConfirmationPhrase {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Фраза не совпала — удаление отменено.",
			})
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_all_cancelled"}
		}
		if err := s.repository.ForgetAll(tenantID); err != nil {
			s.logger.Error("forget all failed", "tenant_id", tenantID, "error", err)
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Не удалось удалить данные: " + err.Error(),
			})
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "forget_all_failed"}
		}
		s.logger.Info("tenant memory forgotten", "tenant_id", tenantID)
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   msg.ChatID,
			Text:     "🗑 Все данные удалены безвозвратно. Подключи бота заново, чтобы начать с чистого листа.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_all_confirmed"}

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

// ManualSend delivers an owner-authored reply through the chat's own channel
// (the tenant's bot for bot chats) exactly as if the owner had typed it in
// Telegram. The reply is stored as an outgoing owner message so it becomes
// context the AI sees on later replies, and any pending AI drafts in the chat
// are discarded — the owner has answered, so the suggestions are now obsolete.
func (s *Service) ManualSend(ctx context.Context, tenantID, chatID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("message text is required")
	}
	ownerID := strings.TrimPrefix(tenantID, "tenant:user:")
	decisionID := fmt.Sprintf("manual:%s:%d", chatID, s.now().UnixNano())
	manualEvent := contracts.NormalizedTelegramEvent{TenantID: tenantID, ChatID: chatID}
	sentMsgID, err := s.sendRecipient(ctx, manualEvent, decisionID, text, contracts.TextFormatPlain, false, s.deliverySettingsFor(manualEvent))
	if err != nil {
		return err
	}
	if sentMsgID == "" {
		// Senders without a message-id capability (or idempotent replays) return
		// an empty id; fall back to the decisionID so the stored row stays unique.
		sentMsgID = decisionID
	}
	// Owner-authored outgoing message with no virtual DoubleU draft, so the Mini
	// App labels it "Ты"; lands in RecentMessages → the AI accounts for it next.
	s.repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:          "out:" + decisionID,
		TenantID:         tenantID,
		ChatID:           chatID,
		SourceMessageID:  sentMsgID,
		SenderID:         ownerID,
		MessageText:      text,
		MessageType:      "text",
		TelegramUpdateID: outgoingUpdateID(decisionID),
		CreatedAt:        s.now(),
	})
	s.discardPendingDrafts(ctx, tenantID, chatID)
	if s.notify != nil {
		s.notify(tenantID, "new_message", map[string]any{"chat_id": chatID})
	}
	return nil
}

// discardPendingDrafts rejects every pending draft in a chat. Used when the
// owner answers manually so the AI's now-obsolete suggestions disappear from
// the chat and the owner's bot card.
func (s *Service) discardPendingDrafts(ctx context.Context, tenantID, chatID string) {
	for _, draft := range s.repository.DraftsForChat(tenantID, chatID) {
		if draft.Status != store.DraftPending {
			continue
		}
		draft.Status = store.DraftRejected
		draft.UpdatedAt = s.now()
		s.repository.UpdateDraft(draft)
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
	}
}
