package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduledJobsCRUDAndDueOrdering(t *testing.T) {
	repo := NewRepository()
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	first, err := repo.InsertScheduledJob(ScheduledJobRecord{
		TenantID: "tenant:user:1", ChatID: "chat", Kind: "reminder", Target: "owner",
		FireAt: now.Add(-time.Minute), Payload: map[string]any{"text": "первое"},
		CreatedBy: "ai", IdempotencyKey: "job:first", CreatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("insert first: %v", err)
	}
	duplicate, err := repo.InsertScheduledJob(ScheduledJobRecord{
		TenantID: "tenant:user:1", ChatID: "chat", Kind: "reminder", Target: "owner",
		FireAt: now, Payload: map[string]any{"text": "duplicate"},
		CreatedBy: "ai", IdempotencyKey: "job:first",
	})
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("idempotent insert = %+v err=%v", duplicate, err)
	}
	future, err := repo.InsertScheduledJob(ScheduledJobRecord{
		TenantID: "tenant:user:1", ChatID: "chat", Kind: "message", Target: "contact",
		FireAt: now.Add(time.Hour), Payload: map[string]any{"text": "позже"},
		CreatedBy: "owner", IdempotencyKey: "job:future",
	})
	if err != nil {
		t.Fatalf("insert future: %v", err)
	}

	due, err := repo.DueJobs(now, 10)
	if err != nil || len(due) != 1 || due[0].ID != first.ID {
		t.Fatalf("due jobs = %+v err=%v", due, err)
	}
	if !repo.ClaimScheduledJob(first.ID, now) || !repo.MarkJobSent(first.ID, now.Add(time.Second)) {
		t.Fatal("claim/mark sent failed")
	}
	if dueAgain, _ := repo.DueJobs(now, 10); len(dueAgain) != 0 {
		t.Fatalf("sent job returned due again: %+v", dueAgain)
	}
	if !repo.CancelJob("tenant:user:1", "chat", future.ID) {
		t.Fatal("cancel future job failed")
	}
	jobs := repo.ListJobs("tenant:user:1", "chat")
	if len(jobs) != 2 || jobs[0].ID != future.ID || jobs[0].Status != "cancelled" || jobs[1].Status != "sent" {
		t.Fatalf("listed jobs = %+v", jobs)
	}
	if repo.CancelJob("other", "chat", future.ID) {
		t.Fatal("cross-tenant cancellation succeeded")
	}
}

func TestScheduledJobsMigrationIsIdempotentAndReversible(t *testing.T) {
	var up, down string
	for _, root := range []string{"../../../infra/migrations", "../../../../infra/migrations"} {
		if raw, err := os.ReadFile(filepath.Join(root, "114_scheduled_jobs.sql")); err == nil {
			up = string(raw)
		}
		if raw, err := os.ReadFile(filepath.Join(root, "114_scheduled_jobs.down.sql")); err == nil {
			down = string(raw)
		}
	}
	if up == "" || down == "" {
		t.Fatal("migration 114 files not found")
	}
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS scheduled_jobs",
		"CREATE INDEX IF NOT EXISTS scheduled_jobs_status_fire_at_idx",
		"idempotency_key text NOT NULL UNIQUE",
	} {
		if !strings.Contains(up, required) {
			t.Errorf("up migration missing %q", required)
		}
	}
	if !strings.Contains(down, "DROP TABLE IF EXISTS scheduled_jobs") {
		t.Error("down migration is not idempotent")
	}
}
