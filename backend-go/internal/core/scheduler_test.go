package core

import (
	"context"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func TestSchedulerWorkerMarksOwnerReminderSentOnce(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{
		TenantID: "tenant:user:77", ChatID: "42", DisplayName: "Контакт",
		Transport: contracts.TransportBusiness,
	})
	job, err := repo.InsertScheduledJob(store.ScheduledJobRecord{
		TenantID: "tenant:user:77", ChatID: "42", Kind: "reminder", Target: "owner",
		FireAt: now.Add(-time.Second), Payload: map[string]any{"text": "Позвонить клиенту"},
		CreatedBy: "ai", IdempotencyKey: "scheduler-owner-1",
	})
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	service := NewService(repo, nil, telegram.NoopSender{}, WithClock(func() time.Time { return now }))

	service.runSchedulerOnce(context.Background())
	service.runSchedulerOnce(context.Background())

	jobs := repo.ListJobs("tenant:user:77", "42")
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Status != "sent" || jobs[0].SentAt == nil {
		t.Fatalf("job after worker = %+v", jobs)
	}
}

func TestSchedulerContactDeliveryRespectsCurrentChatMode(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := store.NewRepository()
	for _, chatID := range []string{"auto-chat", "manual-chat"} {
		repo.SaveChat(store.ChatRecord{
			TenantID: "tenant:user:77", ChatID: chatID, DisplayName: chatID,
			Transport: contracts.TransportBusiness,
		})
	}
	repo.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: "auto-chat", Mode: contracts.ChatModeAuto,
	})
	repo.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: "manual-chat", Mode: contracts.ChatModeManual,
	})
	for _, chatID := range []string{"auto-chat", "manual-chat"} {
		_, err := repo.InsertScheduledJob(store.ScheduledJobRecord{
			TenantID: "tenant:user:77", ChatID: chatID, Kind: "message", Target: "contact",
			FireAt: now.Add(-time.Second), Payload: map[string]any{"text": "Напоминание " + chatID},
			CreatedBy: "ai", IdempotencyKey: "scheduler-contact-" + chatID,
		})
		if err != nil {
			t.Fatalf("insert %s: %v", chatID, err)
		}
	}
	sender := &fakeSender{}
	service := NewService(repo, nil, sender, WithClock(func() time.Time { return now }))

	service.runSchedulerOnce(context.Background())

	if len(sender.sent) != 2 {
		t.Fatalf("expected contact send plus owner draft card, got %+v", sender.sent)
	}
	if sender.sent[0].ChatID != "auto-chat" || sender.sent[0].ReplyMarkup != nil {
		t.Fatalf("auto delivery = %+v", sender.sent[0])
	}
	if sender.sent[1].ChatID != "77" {
		t.Fatalf("manual delivery must notify owner, got %+v", sender.sent[1])
	}
	markup, ok := sender.sent[1].ReplyMarkup.(telegram.InlineKeyboardMarkup)
	if !ok || len(markup.InlineKeyboard) == 0 {
		t.Fatalf("manual reminder card has no approval keyboard: %#v", sender.sent[1].ReplyMarkup)
	}
	drafts := repo.DraftsForChat("tenant:user:77", "manual-chat")
	if len(drafts) != 1 || drafts[0].Status != store.DraftPending {
		t.Fatalf("manual scheduled drafts = %+v", drafts)
	}
	if autoDrafts := repo.DraftsForChat("tenant:user:77", "auto-chat"); len(autoDrafts) != 0 {
		t.Fatalf("auto mode unexpectedly created drafts: %+v", autoDrafts)
	}
}

func TestSchedulerAIFollowupRunsNewAITurn(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{
		TenantID: "tenant:user:77", ChatID: "42", DisplayName: "Контакт",
		Transport: contracts.TransportBusiness,
	})
	repo.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeManual,
	})
	job, err := repo.InsertScheduledJob(store.ScheduledJobRecord{
		TenantID: "tenant:user:77", ChatID: "42", Kind: "ai_followup", Target: "contact",
		FireAt: now.Add(-time.Second), Payload: map[string]any{"text": "Уточни статус договора"},
		CreatedBy: "ai", IdempotencyKey: "scheduler-followup-1",
	})
	if err != nil {
		t.Fatalf("insert followup: %v", err)
	}
	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision: contracts.DecisionIgnore, RiskLevel: contracts.RiskLow,
	}}
	service := NewService(
		repo, ai, telegram.NoopSender{},
		WithClock(func() time.Time { return now }), WithReplyDelay(0),
	)

	service.runSchedulerOnce(context.Background())

	if ai.calls != 1 || ai.lastRequest.Event.MessageText != "Уточни статус договора" {
		t.Fatalf("AI followup calls=%d request=%+v", ai.calls, ai.lastRequest.Event)
	}
	jobs := repo.ListJobs("tenant:user:77", "42")
	if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Status != "sent" {
		t.Fatalf("followup job = %+v", jobs)
	}
}

func TestSchedulerRechecksPrivacyAtFireTime(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	repo := store.NewRepository()
	repo.SaveChat(store.ChatRecord{
		TenantID: "tenant:user:77", ChatID: "42", Transport: contracts.TransportBusiness,
	})
	repo.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: "42", Mode: contracts.ChatModeAuto,
		PrivacyFlags: contracts.PrivacyFlags{contracts.PrivacyPrivate},
	})
	_, err := repo.InsertScheduledJob(store.ScheduledJobRecord{
		TenantID: "tenant:user:77", ChatID: "42", Kind: "message", Target: "contact",
		FireAt: now.Add(-time.Second), Payload: map[string]any{"text": "Не отправлять"},
		CreatedBy: "ai", IdempotencyKey: "scheduler-private-1",
	})
	if err != nil {
		t.Fatalf("insert private job: %v", err)
	}
	sender := &fakeSender{}
	service := NewService(repo, nil, sender, WithClock(func() time.Time { return now }))

	service.runSchedulerOnce(context.Background())

	jobs := repo.ListJobs("tenant:user:77", "42")
	if len(jobs) != 1 || jobs[0].Status != "failed" || jobs[0].SentAt != nil {
		t.Fatalf("privacy-gated job = %+v", jobs)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("privacy-gated job sent messages: %+v", sender.sent)
	}
}
