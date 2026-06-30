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

// TestScheduledJobsWithoutWallClockBehaveAsBeforeD is the regression guard for D:
// a job with no LocalTime/TZName must be selected by DueJobs purely on its
// stored UTC FireAt, exactly like before this feature existed.
func TestScheduledJobsWithoutWallClockBehaveAsBeforeD(t *testing.T) {
	repo := NewRepository()
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	job, err := repo.InsertScheduledJob(ScheduledJobRecord{
		TenantID: "tenant:user:1", ChatID: "chat", Kind: "reminder", Target: "owner",
		FireAt: now.Add(-time.Minute), Payload: map[string]any{"text": "legacy"},
		CreatedBy: "ai", IdempotencyKey: "job:legacy",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if job.LocalTime != "" || job.TZName != "" {
		t.Fatalf("expected empty wall-clock fields, got %+v", job)
	}
	due, err := repo.DueJobs(now, 10)
	if err != nil || len(due) != 1 || due[0].ID != job.ID || !due[0].FireAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("legacy job due selection = %+v err=%v", due, err)
	}
}

// TestScheduledJobsDueJobsRecomputesFromWallClockAndDoesNotFireEarly verifies D:
// when LocalTime/TZName are set, DueJobs must derive the authoritative fire
// instant from them (current tzdata), not the possibly-stale baked FireAt —
// and in particular must NOT treat a job as due just because a wrong/stale
// baked FireAt already lies in the past relative to "now".
//
// Scenario: a reminder for 2026-01-15 09:00 local time in Europe/Berlin (winter,
// CET = UTC+1, correct instant 08:00:00Z) was stored with a FireAt baked as if
// CEST (UTC+2, correct instant 07:00:00Z) were in effect — exactly the class of
// drift a DST/offset miscalculation between creation and firing produces. The
// stale baked value already looks "due" by a coarse fire_at<=now filter; the
// recompute must still defer firing until the true wall-clock instant.
func TestScheduledJobsDueJobsRecomputesFromWallClockAndDoesNotFireEarly(t *testing.T) {
	repo := NewRepository()
	correctUTC := time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC) // 09:00 CET (UTC+1)
	staleBakedUTC := time.Date(2026, 1, 15, 7, 0, 0, 0, time.UTC) // wrongly assumed UTC+2

	job, err := repo.InsertScheduledJob(ScheduledJobRecord{
		TenantID: "tenant:user:1", ChatID: "chat", Kind: "reminder", Target: "owner",
		FireAt: staleBakedUTC, Payload: map[string]any{"text": "зимняя встреча"},
		CreatedBy: "ai", IdempotencyKey: "job:dst-winter",
		LocalTime: "2026-01-15T09:00:00", TZName: "Europe/Berlin",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The stale baked FireAt (07:00Z) already lies in the past at 07:30Z, but
	// the recomputed true instant (08:00Z) does not — must not fire early.
	tooEarly := staleBakedUTC.Add(30 * time.Minute) // 07:30Z
	due, err := repo.DueJobs(tooEarly, 10)
	if err != nil {
		t.Fatalf("due jobs (too early): %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("job fired early using stale baked fire_at: %+v", due)
	}

	// Repeated polls before the true instant must agree (idempotent/deterministic).
	due2, err := repo.DueJobs(tooEarly, 10)
	if err != nil || len(due2) != 0 {
		t.Fatalf("repeated poll before due must stay empty, got %+v err=%v", due2, err)
	}

	// Once "now" reaches the true recomputed wall-clock instant, the job fires
	// with FireAt corrected to that instant — not the stale baked value.
	atTrueInstant := correctUTC.Add(time.Second)
	due3, err := repo.DueJobs(atTrueInstant, 10)
	if err != nil || len(due3) != 1 || due3[0].ID != job.ID {
		t.Fatalf("due jobs at true instant = %+v err=%v", due3, err)
	}
	if !due3[0].FireAt.Equal(correctUTC) {
		t.Fatalf("recomputed FireAt = %v, want %v (the wall-clock-correct instant)", due3[0].FireAt, correctUTC)
	}
}

// TestScheduledJobsInvalidTimezoneFallsBackToStoredFireAt verifies the IANA
// fail-safe (mirrors apiSaveUserSettings/time.LoadLocation): an unresolvable
// TZName must never panic DueJobs, and the job keeps using its stored FireAt
// as if it had no wall-clock identity at all.
func TestScheduledJobsInvalidTimezoneFallsBackToStoredFireAt(t *testing.T) {
	repo := NewRepository()
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	job, err := repo.InsertScheduledJob(ScheduledJobRecord{
		TenantID: "tenant:user:1", ChatID: "chat", Kind: "reminder", Target: "owner",
		FireAt: now.Add(-time.Minute), Payload: map[string]any{"text": "garbage tz"},
		CreatedBy: "ai", IdempotencyKey: "job:bad-tz",
		LocalTime: "2026-06-22T15:00:00", TZName: "Not/AZone",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	due, err := repo.DueJobs(now, 10)
	if err != nil || len(due) != 1 || due[0].ID != job.ID || !due[0].FireAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("invalid-timezone job due selection = %+v err=%v", due, err)
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

func TestScheduledJobsWalltimeMigrationIsIdempotentAndReversible(t *testing.T) {
	var up, down string
	for _, root := range []string{"../../../infra/migrations", "../../../../infra/migrations"} {
		if raw, err := os.ReadFile(filepath.Join(root, "119_scheduled_jobs_walltime.sql")); err == nil {
			up = string(raw)
		}
		if raw, err := os.ReadFile(filepath.Join(root, "119_scheduled_jobs_walltime.down.sql")); err == nil {
			down = string(raw)
		}
	}
	if up == "" || down == "" {
		t.Fatal("migration 119 files not found")
	}
	for _, required := range []string{
		"ADD COLUMN IF NOT EXISTS local_time text NULL",
		"ADD COLUMN IF NOT EXISTS tz_name text NULL",
	} {
		if !strings.Contains(up, required) {
			t.Errorf("up migration missing %q", required)
		}
	}
	for _, required := range []string{
		"DROP COLUMN IF EXISTS local_time",
		"DROP COLUMN IF EXISTS tz_name",
	} {
		if !strings.Contains(down, required) {
			t.Errorf("down migration missing %q", required)
		}
	}
}
