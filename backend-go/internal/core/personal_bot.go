package core

import (
	"context"
	"strconv"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/telegram"
)

// defaultPersonalBotStubText is the branded reply a personal-mode bot sends to a
// non-allowed sender when the owner has not configured PERSONAL_BOT_STUB_TEXT.
const defaultPersonalBotStubText = "Это личный бот — он общается только со своим владельцем.\n\n" +
	"Хотите такого же персонального ИИ-ассистента в Telegram? Создайте своего с DoubleU."

// personalBotIntercept enforces personal-bot mode on the bot transport. When the
// bot has the toggle on and the sender is neither the owner nor on the bot's
// allowlist, the message is suppressed: a branded stub is sent once per contact,
// then the bot stays silent. Nothing reaches the AI or memory (audit only).
//
// It returns (result, true) when it handled the message — the caller must return
// result and NOT run the normal pipeline — or (_, false) to proceed as usual
// (owner/allowlisted sender, toggle off, or a non-bot transport).
func (s *Service) personalBotIntercept(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string) (InboundResult, bool) {
	if event.Transport != contracts.TransportBot || event.BotID == "" {
		return InboundResult{}, false
	}
	botID, err := strconv.ParseInt(event.BotID, 10, 64)
	if err != nil {
		return InboundResult{}, false
	}
	rec, ok := s.repository.UserBotByID(event.TenantID, botID)
	if !ok || !rec.PersonalMode {
		return InboundResult{}, false
	}
	// The owner (implicitly) and any allowlisted Telegram user are served normally.
	if event.SenderID == ownerID || s.repository.IsBotAllowedUser(event.TenantID, botID, event.SenderID) {
		return InboundResult{}, false
	}
	// Non-allowed sender: stub once, then silence. MarkPersonalBotStubShown is the
	// atomic once-per-contact gate (first call false → send; later calls true).
	reason := "personal_bot_silent"
	if alreadyShown := s.repository.MarkPersonalBotStubShown(event.TenantID, event.ChatID); !alreadyShown {
		s.sendPersonalBotStub(ctx, event)
		reason = "personal_bot_stub"
	}
	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, reason))
	s.logger.Info("personal bot suppressed non-allowed sender",
		"tenant_id", event.TenantID, "chat_id", event.ChatID, "bot_id", event.BotID,
		"sender_id", event.SenderID, "reason", reason)
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: reason}, true
}

// sendPersonalBotStub delivers the branded stub through the bot the message
// arrived on. Best-effort: a send failure is logged, never fatal.
func (s *Service) sendPersonalBotStub(ctx context.Context, event contracts.NormalizedTelegramEvent) {
	sender, err := s.recipientSender(event)
	if err != nil {
		s.logger.Warn("personal bot stub not sent: no sender",
			"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
		return
	}
	if err := sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID: event.TenantID,
		ChatID:   contracts.RawChatID(event.ChatID),
		Text:     s.personalBotStub(),
	}); err != nil {
		s.logger.Warn("personal bot stub send failed",
			"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
	}
}

// personalBotStub returns the configured stub text or the built-in default.
func (s *Service) personalBotStub() string {
	if strings.TrimSpace(s.personalBotStubText) != "" {
		return s.personalBotStubText
	}
	return defaultPersonalBotStubText
}
