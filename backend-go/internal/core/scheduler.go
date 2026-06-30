package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// StartScheduler dispatches due scheduled_jobs every 45 seconds. It is safe to
// wire from multiple lifecycle surfaces; only the first caller starts a loop.
func (s *Service) StartScheduler(ctx context.Context) {
	if !s.schedulerStarted.CompareAndSwap(false, true) {
		return
	}
	s.runSchedulerOnce(ctx)
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runSchedulerOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) runSchedulerOnce(ctx context.Context) {
	jobs, err := s.repository.DueJobs(s.now(), 50)
	if err != nil {
		s.logger.Error("scheduled jobs query failed", "error", err)
		return
	}
	for _, job := range jobs {
		if !s.repository.ClaimScheduledJob(job.ID, s.now()) {
			continue
		}
		if err := s.dispatchScheduledJob(ctx, job); err != nil {
			s.repository.MarkJobFailed(job.ID)
			s.logger.Error("scheduled job failed",
				"job_id", job.ID, "tenant_id", job.TenantID,
				"chat_id", job.ChatID, "kind", job.Kind, "target", job.Target,
				"error", err,
			)
			continue
		}
		s.repository.MarkJobSent(job.ID, s.now())
		s.logger.Info("scheduled job sent",
			"job_id", job.ID, "tenant_id", job.TenantID,
			"chat_id", job.ChatID, "kind", job.Kind, "target", job.Target,
		)
		if s.notify != nil {
			s.notify(job.TenantID, "scheduled_job", map[string]any{
				"job_id": job.ID, "chat_id": job.ChatID, "status": "sent",
			})
		}
	}
}

func (s *Service) dispatchScheduledJob(ctx context.Context, job store.ScheduledJobRecord) error {
	settings := s.repository.ChatSettings(job.TenantID, job.ChatID).WithDefaults()
	if job.CreatedBy == "ai" && (settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) ||
		settings.PrivacyFlags.Contains(contracts.PrivacyPrivate)) {
		return fmt.Errorf("chat privacy blocks AI-created scheduled job")
	}
	text, _ := job.Payload["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("scheduled job payload text is empty")
	}
	event, err := s.scheduledJobEvent(job, text)
	if err != nil {
		return err
	}
	if job.Kind == "ai_followup" {
		ownerID := strings.TrimPrefix(job.TenantID, "tenant:user:")
		if ownerID == job.TenantID {
			ownerID = ""
		}
		_, err := s.processContactMessage(ctx, event, ownerID, "")
		return err
	}
	if job.Target == "owner" {
		return s.sendScheduledOwnerNotification(ctx, event, job, text)
	}
	if job.Target != "contact" {
		return fmt.Errorf("unsupported scheduled target %q", job.Target)
	}
	switch settings.Mode {
	case contracts.ChatModeAuto:
		decisionID := fmt.Sprintf("scheduled-job:%d", job.ID)
		sentMsgID, sendErr := s.sendRecipient(
			ctx, event, decisionID, text, contracts.TextFormatPlain, false,
			s.deliverySettingsFor(event),
		)
		if sendErr != nil {
			return sendErr
		}
		ownerID := strings.TrimPrefix(job.TenantID, "tenant:user:")
		if ownerID == job.TenantID {
			ownerID = ""
		}
		s.saveOutgoingMessage(event, ownerID, sentMsgID, text, decisionID, contracts.TextFormatPlain)
		if s.notify != nil {
			s.notify(job.TenantID, "new_message", map[string]any{"chat_id": job.ChatID})
		}
		return nil
	case contracts.ChatModeManual, contracts.ChatModeSemiAuto:
		return s.createScheduledDraft(ctx, event, job, text)
	default:
		return fmt.Errorf("chat mode %q blocks scheduled contact delivery", settings.Mode)
	}
}

func (s *Service) scheduledJobEvent(job store.ScheduledJobRecord, text string) (contracts.NormalizedTelegramEvent, error) {
	chat, ok := s.repository.Chat(job.TenantID, job.ChatID)
	if !ok {
		return contracts.NormalizedTelegramEvent{}, fmt.Errorf("scheduled chat not found")
	}
	botID := job.BotID
	if botID == "" {
		if parsed, _, botChat := contracts.ParseBotChatID(job.ChatID); botChat {
			botID = parsed
		}
	}
	transport := chat.Transport
	if transport == "" && botID != "" {
		transport = contracts.TransportBot
	}
	return contracts.NormalizedTelegramEvent{
		EventID:              fmt.Sprintf("scheduled-job:%d", job.ID),
		TenantID:             job.TenantID,
		ChatID:               job.ChatID,
		ChatDisplayName:      chat.DisplayName,
		ChatType:             chat.ChatType,
		TelegramUpdateID:     -job.ID,
		BusinessConnectionID: chat.BusinessConnectionID,
		SourceMessageID:      fmt.Sprintf("scheduled:%d", job.ID),
		SenderID:             contracts.RawChatID(job.ChatID),
		MessageText:          text,
		MessageType:          "scheduled",
		CreatedAt:            s.now(),
		Transport:            transport,
		BotID:                botID,
	}, nil
}

func (s *Service) sendScheduledOwnerNotification(ctx context.Context, event contracts.NormalizedTelegramEvent, job store.ScheduledJobRecord, text string) error {
	sender, chatID, err := s.scheduledOwnerNotificationRoute(event)
	if err != nil {
		return err
	}
	decisionID := fmt.Sprintf("scheduled-owner:%d", job.ID)
	if s.repository.MarkSend(store.SendRecord{
		Key:             contracts.SendIdempotencyKey(job.TenantID, job.ChatID, event.SourceMessageID, decisionID),
		TenantID:        job.TenantID,
		ChatID:          job.ChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      decisionID,
		Text:            text,
		CreatedAt:       s.now(),
	}) {
		return nil
	}
	_, err = sendMessage(ctx, sender, telegram.OutboundMessage{
		TenantID:        job.TenantID,
		ChatID:          chatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      decisionID,
		Text:            "⏰ " + text,
	})
	return err
}

func (s *Service) scheduledOwnerNotificationRoute(event contracts.NormalizedTelegramEvent) (telegram.Sender, string, error) {
	if event.Transport == contracts.TransportBot {
		sender, err := s.recipientSender(event)
		if err != nil {
			return nil, "", err
		}
		chatID := contracts.RawChatID(event.ChatID)
		if strings.TrimSpace(chatID) == "" {
			return nil, "", fmt.Errorf("scheduled bot chat not found")
		}
		return sender, chatID, nil
	}
	ownerChatID := s.ownerChatIDForEvent(event)
	if ownerChatID == "" {
		return nil, "", fmt.Errorf("owner chat not found")
	}
	return s.sender, ownerChatID, nil
}

func (s *Service) createScheduledDraft(ctx context.Context, event contracts.NormalizedTelegramEvent, job store.ScheduledJobRecord, text string) error {
	decisionID := fmt.Sprintf("scheduled-job:%d", job.ID)
	draft := s.createDraft(event, decisionID, text, contracts.TextFormatPlain)
	draft.DraftID = fmt.Sprintf("draft:scheduled:%d", job.ID)
	s.repository.SaveDraft(draft)
	if s.notify != nil {
		s.notify(job.TenantID, "new_draft", map[string]any{
			"chat_id":       job.ChatID,
			"draft_id":      draft.DraftID,
			"response_text": text,
			"source_text":   event.MessageText,
			"created_at":    draft.CreatedAt.UTC().Format(time.RFC3339),
		})
		return nil
	}
	ownerID := strings.TrimPrefix(job.TenantID, "tenant:user:")
	if ownerID == job.TenantID {
		ownerID = ""
	}
	if s.ownerChatIDForEvent(event) == "" {
		return fmt.Errorf("owner chat not found")
	}
	s.sendDraftToOwner(ctx, event, draft, ownerID)
	return nil
}
