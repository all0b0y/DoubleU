package core

import (
	"context"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// TestSchedulerListAndCancelRoundTrip verifies the backend surfaces pending
// reminders to the AI when the list/cancel tools are enabled and applies the
// cancellations the AI returns.
func TestSchedulerListAndCancelRoundTrip(t *testing.T) {
	repository := store.NewRepository()
	role := repository.SaveAIRole(store.AIRoleRecord{
		TenantID: "tenant:user:77", Name: "Planner",
		ToolSettings: map[string]bool{
			"scheduler.create_reminder": true,
			"scheduler.list_reminders":  true,
			"scheduler.cancel_reminder": true,
		},
	})
	repository.SaveUserBot(store.UserBotRecord{
		TenantID: "tenant:user:77", BotID: 555, Status: "active", AIRoleID: role.ID,
	})
	chatID := contracts.BotChatID("555", "42")
	repository.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: chatID, Mode: contracts.ChatModeAuto,
	})
	job, err := repository.InsertScheduledJob(store.ScheduledJobRecord{
		TenantID: "tenant:user:77", ChatID: chatID, BotID: "555",
		Kind: "reminder", Target: "owner", FireAt: time.Now().Add(time.Hour),
		Payload: map[string]any{"text": "позвонить"}, CreatedBy: "ai",
		IdempotencyKey: "scheduler:keep-1",
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision: contracts.DecisionAutoReply, ResponseText: "Отменил напоминание.",
		Confidence: 0.99, RiskLevel: contracts.RiskLow, Intent: "acknowledgement",
		CancelledJobIDs: []int64{job.ID},
	}}
	service := NewService(repository, ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: &fakeSender{}}),
	)

	if _, err := service.HandleBotMessage(context.Background(), botEvent(t, 9700, 42, 30, "отмени напоминание")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// The pending reminder reached the AI request so the tools could reference it.
	if len(ai.lastRequest.ExistingScheduledJobs) != 1 || ai.lastRequest.ExistingScheduledJobs[0].ID != job.ID {
		t.Fatalf("existing jobs not surfaced: %+v", ai.lastRequest.ExistingScheduledJobs)
	}
	if ai.lastRequest.ExistingScheduledJobs[0].Text != "позвонить" {
		t.Fatalf("existing job text = %q", ai.lastRequest.ExistingScheduledJobs[0].Text)
	}
	// The cancellation the AI returned moved the job out of pending.
	jobs := repository.ListJobs("tenant:user:77", chatID)
	if len(jobs) != 1 || jobs[0].Status != "cancelled" {
		t.Fatalf("job not cancelled: %+v", jobs)
	}
}

// TestSchedulerExistingJobsOmittedWhenToolsDisabled keeps the create-only common
// case from paying for the pending-jobs read.
func TestSchedulerExistingJobsOmittedWhenToolsDisabled(t *testing.T) {
	repository := store.NewRepository()
	role := repository.SaveAIRole(store.AIRoleRecord{
		TenantID: "tenant:user:77", Name: "Planner",
		ToolSettings: map[string]bool{"scheduler.create_reminder": true},
	})
	repository.SaveUserBot(store.UserBotRecord{
		TenantID: "tenant:user:77", BotID: 555, Status: "active", AIRoleID: role.ID,
	})
	chatID := contracts.BotChatID("555", "42")
	repository.SaveChatSettings(contracts.ChatSettings{
		TenantID: "tenant:user:77", ChatID: chatID, Mode: contracts.ChatModeAuto,
	})
	if _, err := repository.InsertScheduledJob(store.ScheduledJobRecord{
		TenantID: "tenant:user:77", ChatID: chatID, BotID: "555",
		Kind: "reminder", Target: "owner", FireAt: time.Now().Add(time.Hour),
		Payload: map[string]any{"text": "x"}, CreatedBy: "ai", IdempotencyKey: "scheduler:keep-2",
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	ai := &fakeAI{response: contracts.AIProcessMessageResponse{
		Decision: contracts.DecisionAutoReply, ResponseText: "ок",
		Confidence: 0.99, RiskLevel: contracts.RiskLow, Intent: "acknowledgement",
	}}
	service := NewService(repository, ai, &fakeSender{},
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
		WithDefaultIngestionMode(contracts.IngestionModeFull),
		WithBotSenderProvider(&fakeBotSenders{sender: &fakeSender{}}),
	)
	if _, err := service.HandleBotMessage(context.Background(), botEvent(t, 9701, 42, 31, "привет")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(ai.lastRequest.ExistingScheduledJobs) != 0 {
		t.Fatalf("pending jobs must not be read when list/cancel are disabled: %+v", ai.lastRequest.ExistingScheduledJobs)
	}
}
