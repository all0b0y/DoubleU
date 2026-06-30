package core

import (
	"context"
	"strings"

	"doubleu/backend-go/internal/bot"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

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

func (s *Service) richOwnerDraftCardEnabled(event contracts.NormalizedTelegramEvent, draft store.DraftRecord) bool {
	return draft.TextFormat.Normalize() == contracts.TextFormatRich && s.deliverySettingsFor(event).RichFormattingEnabled
}

func (s *Service) sendOwnerCard(ctx context.Context, msg telegram.OutboundMessage, rich bool) (string, error) {
	if rich {
		if richSender, ok := s.sender.(interface {
			SendRichMessage(context.Context, telegram.OutboundMessage) (string, error)
		}); ok {
			msgID, err := richSender.SendRichMessage(ctx, msg)
			if err == nil {
				return msgID, nil
			}
			s.logger.Warn("telegram owner rich card send failed; falling back to plain",
				"tenant_id", msg.TenantID,
				"chat_id", msg.ChatID,
				"decision_id", msg.DecisionID,
				"error", err,
			)
		}
	}
	if idSender, ok := s.sender.(interface {
		SendMessageGetID(context.Context, telegram.OutboundMessage) (string, error)
	}); ok {
		return idSender.SendMessageGetID(ctx, msg)
	}
	return "", s.sender.SendMessage(ctx, msg)
}

// sendWarningToOwner delivers a critical-risk warning card to the owner's bot chat.
// The card has no Approve/Edit controls — the owner can only dismiss it and reply
// manually from their own Telegram (ARCH-1).
func (s *Service) sendWarningToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerNote string) {
	ownerChatID := s.ownerChatIDForEvent(event)
	if ownerChatID == "" {
		s.logger.Warn("cannot deliver warning card to owner: chat ID not found",
			"business_connection_id", event.BusinessConnectionID, "draft_id", draft.DraftID)
		return
	}
	msg := telegram.OutboundMessage{
		TenantID:        event.TenantID,
		ChatID:          ownerChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      draft.DecisionID,
		Text:            bot.WarningCardText(event.MessageText, ownerNote, event.ChatDisplayName),
		ReplyMarkup:     bot.WarningKeyboard(draft.DraftID),
	}
	msgID, err := s.sendOwnerCard(ctx, msg, false)
	if err != nil {
		s.logger.Warn("failed to deliver warning card to owner", "tenant_id", event.TenantID, "draft_id", draft.DraftID, "error", err)
		return
	}
	if msgID != "" {
		draft.OwnerChatID = ownerChatID
		draft.OwnerMessageID = msgID
		s.repository.UpdateDraft(draft)
	}
}

// ownerChatIDForEvent resolves the owner's control-bot chat for any transport.
// Bot-transport tenants are keyed "tenant:user:<id>" where <id> is the owner's
// Telegram user id (which equals their DM chat with the control bot); business
// transport keeps the connection-based lookup with its fallbacks.
func (s *Service) ownerChatIDForEvent(event contracts.NormalizedTelegramEvent) string {
	if event.Transport == contracts.TransportBot || event.BusinessConnectionID == "" {
		if owner := strings.TrimPrefix(event.TenantID, "tenant:user:"); owner != event.TenantID {
			return owner
		}
		if event.BusinessConnectionID == "" {
			return ""
		}
	}
	return s.ownerChatIDFor(event.BusinessConnectionID)
}

// ownerChatIDFor resolves the owner's bot chat for a business connection,
// falling back to inference from claimed chats (shared by draft/question/warning cards).
func (s *Service) ownerChatIDFor(businessConnectionID string) string {
	if connection, ok := s.repository.BusinessConnection(businessConnectionID); ok && connection.UserChatID != "" {
		return connection.UserChatID
	}
	ownerChatID := s.inferOwnerChatID(businessConnectionID)
	if ownerChatID != "" {
		s.repository.SaveBusinessConnection(store.BusinessConnectionRecord{
			ConnectionID: businessConnectionID,
			TenantID:     "tenant:user:" + ownerChatID,
			UserID:       ownerChatID,
			UserChatID:   ownerChatID,
			CanReply:     true,
			IsEnabled:    true,
			UpdatedAt:    s.now(),
		})
	}
	return ownerChatID
}

// sendDraftToOwner delivers a draft card with approval keyboard to the owner's bot chat.
// Stores the Telegram message_id of the card so it can be deleted/edited later.
func (s *Service) sendDraftToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerID string) {
	ownerChatID := s.ownerChatIDForEvent(event)
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
	msgID, err := s.sendOwnerCard(ctx, msg, s.richOwnerDraftCardEnabled(event, draft))
	if err != nil {
		s.logger.Warn("failed to deliver draft card to owner", "tenant_id", event.TenantID, "draft_id", draft.DraftID, "error", err)
		return
	}
	if msgID != "" {
		draft.OwnerChatID = ownerChatID
		draft.OwnerMessageID = msgID
		s.repository.UpdateDraft(draft)
	}
}

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

// sendQuestionToOwner delivers a question card with AI-suggested option buttons to the owner's bot chat.
func (s *Service) sendQuestionToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerID string) {
	ownerChatID := s.ownerChatIDForEvent(event)
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
	msgID, err := s.sendOwnerCard(ctx, msg, false)
	if err != nil {
		s.logger.Warn("failed to deliver question card to owner", "tenant_id", event.TenantID, "draft_id", draft.DraftID, "error", err)
		return
	}
	if msgID != "" {
		draft.OwnerChatID = ownerChatID
		draft.OwnerMessageID = msgID
		s.repository.UpdateDraft(draft)
	}
}
