package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
	tracepkg "doubleu/backend-go/internal/trace"
)

// HandleBotMessage processes a normalized message that arrived through the
// tenant's user-supplied bot (botservice poller). Unlike the Business webhook
// path the tenant is always known from the bot registration, so there is no
// connection resolution and no unclaimed staging: the owner connected the bot
// deliberately, so its chats are managed immediately.
func (s *Service) HandleBotMessage(ctx context.Context, event contracts.NormalizedTelegramEvent) (InboundResult, error) {
	ctx = tracepkg.WithTraceID(ctx, tracepkg.DeriveTraceID(event.EventID))
	if !s.ingestionModeFor(event.TenantID).AllowsBot() {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "ingestion_mode_personal"))
		s.logger.Info("bot message skipped by ingestion mode",
			"tenant_id", event.TenantID, "chat_id", event.ChatID, "bot_id", event.BotID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "ingestion_mode_personal"}, nil
	}
	if event.ChatDisplayName == "" {
		event.ChatDisplayName = anonymousChatName(event.ChatID)
	}
	// Bot tenants are keyed "tenant:user:<telegram_user_id>" at registration,
	// so the owner id falls out of the tenant id without a connection lookup.
	ownerID := strings.TrimPrefix(event.TenantID, "tenant:user:")
	if ownerID == event.TenantID {
		ownerID = ""
	}
	s.logger.Info("bot message received",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"bot_id", event.BotID,
		"source_message_id", event.SourceMessageID,
	)
	// Personal-bot mode: a bot the owner marked "personal" answers only the owner
	// and its allowlist; anyone else gets a one-time stub and is never processed.
	if result, intercepted := s.personalBotIntercept(ctx, event, ownerID); intercepted {
		return result, nil
	}
	return s.processContactMessage(ctx, event, ownerID, "")
}

// HandleBotCallbackQuery processes a callback received by a tenant's own bot.
// Tenant/bot scope comes from the poller registration, never from callback data.
func (s *Service) HandleBotCallbackQuery(ctx context.Context, callback telegram.CallbackQueryUpdate, tenantID, botID string) (InboundResult, error) {
	callback.TenantID = tenantID
	callback.BotID = botID
	callback.Transport = contracts.TransportBot
	return s.handleCallbackQuery(ctx, callback), nil
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

	summ, msgCount, err := s.SummarizeChat(ctx, tenantID, chat.ChatID)
	if errors.Is(err, ErrSummaryBlockedByPrivacy) {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Для этого чата отключены сводки (флаг no_summary).",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_blocked_by_privacy"}
	}
	if errors.Is(err, ErrSummaryNoMessages) {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Нет сообщений для суммаризации в этом чате.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_no_messages"}
	}

	replyText := ""
	if err != nil || summ.Summary == "" {
		replyText = fmt.Sprintf("📊 Чат «%s»: %d сообщений. Сводка недоступна.", chatDisplayName(chat), msgCount)
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
