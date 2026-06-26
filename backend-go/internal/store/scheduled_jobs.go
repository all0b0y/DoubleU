package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// InsertScheduledJob stores one idempotent deferred job.
func (r *Repository) InsertScheduledJob(job ScheduledJobRecord) (ScheduledJobRecord, error) {
	if job.Status == "" {
		job.Status = "pending"
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.Payload == nil {
		job.Payload = map[string]any{}
	}
	if job.TenantID == "" || job.ChatID == "" || job.IdempotencyKey == "" || job.FireAt.IsZero() {
		return ScheduledJobRecord{}, fmt.Errorf("scheduled job scope, fire_at and idempotency_key are required")
	}
	if job.Kind != "reminder" && job.Kind != "message" && job.Kind != "ai_followup" {
		return ScheduledJobRecord{}, fmt.Errorf("invalid scheduled job kind %q", job.Kind)
	}
	if job.Target != "owner" && job.Target != "contact" {
		return ScheduledJobRecord{}, fmt.Errorf("invalid scheduled job target %q", job.Target)
	}
	if job.Status != "pending" && job.Status != "sent" && job.Status != "cancelled" && job.Status != "failed" {
		return ScheduledJobRecord{}, fmt.Errorf("invalid scheduled job status %q", job.Status)
	}
	if job.CreatedBy != "ai" && job.CreatedBy != "owner" {
		return ScheduledJobRecord{}, fmt.Errorf("invalid scheduled job creator %q", job.CreatedBy)
	}
	if r.db != nil {
		payload, err := json.Marshal(job.Payload)
		if err != nil {
			return ScheduledJobRecord{}, err
		}
		var botID any
		if job.BotID != "" {
			parsed, parseErr := strconv.ParseInt(job.BotID, 10, 64)
			if parseErr != nil {
				return ScheduledJobRecord{}, parseErr
			}
			botID = parsed
		}
		err = r.db.QueryRow(
			`INSERT INTO scheduled_jobs
			   (tenant_id, chat_id, bot_id, kind, target, fire_at, payload,
			    status, created_by, idempotency_key, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11)
			 ON CONFLICT (idempotency_key) DO UPDATE
			 SET idempotency_key = EXCLUDED.idempotency_key
			 RETURNING id, created_at`,
			job.TenantID, job.ChatID, botID, job.Kind, job.Target, job.FireAt,
			string(payload), job.Status, job.CreatedBy, job.IdempotencyKey, job.CreatedAt,
		).Scan(&job.ID, &job.CreatedAt)
		return cloneScheduledJob(job), err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.scheduledJobs {
		if existing.IdempotencyKey == job.IdempotencyKey {
			return cloneScheduledJob(existing), nil
		}
	}
	r.scheduledJobSeq++
	job.ID = r.scheduledJobSeq
	r.scheduledJobs[job.ID] = cloneScheduledJob(job)
	return cloneScheduledJob(job), nil
}

// DueJobs returns pending jobs due by now. PostgreSQL workers skip rows locked
// by another transaction; ClaimScheduledJob provides the atomic post-read gate.
func (r *Repository) DueJobs(now time.Time, limit int) ([]ScheduledJobRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if r.db != nil {
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.Query(
			`SELECT id, tenant_id, chat_id, bot_id, kind, target, fire_at, payload,
			        status, created_by, idempotency_key, created_at, sent_at
			   FROM scheduled_jobs
			  WHERE status = 'pending' AND fire_at <= $1
			  ORDER BY fire_at, id
			  FOR UPDATE SKIP LOCKED
			  LIMIT $2`, now, limit,
		)
		if err != nil {
			return nil, err
		}
		jobs, err := scanScheduledJobs(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return jobs, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	jobs := make([]ScheduledJobRecord, 0)
	for _, job := range r.scheduledJobs {
		if job.Status == "pending" && !job.FireAt.After(now) {
			jobs = append(jobs, cloneScheduledJob(job))
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].FireAt.Equal(jobs[j].FireAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].FireAt.Before(jobs[j].FireAt)
	})
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

// ClaimScheduledJob atomically reserves a pending job by moving it to sent.
// Dispatch then either confirms sent_at or moves it to failed. This at-most-once
// claim prevents concurrent workers from performing the same visible action.
func (r *Repository) ClaimScheduledJob(id int64, claimedAt time.Time) bool {
	if r.db != nil {
		result, err := r.db.Exec(
			`UPDATE scheduled_jobs SET status = 'sent', sent_at = $2
			  WHERE id = $1 AND status = 'pending'`, id, claimedAt,
		)
		if err != nil {
			r.logWriteError("claim_scheduled_job", "", "", err)
			return false
		}
		rows, _ := result.RowsAffected()
		return rows == 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.scheduledJobs[id]
	if !ok || job.Status != "pending" {
		return false
	}
	job.Status = "sent"
	claimed := claimedAt.UTC()
	job.SentAt = &claimed
	r.scheduledJobs[id] = job
	return true
}

// MarkJobSent confirms successful dispatch.
func (r *Repository) MarkJobSent(id int64, sentAt time.Time) bool {
	return r.setScheduledJobStatus(id, "sent", sentAt)
}

// MarkJobFailed records a terminal dispatch failure.
func (r *Repository) MarkJobFailed(id int64) bool {
	return r.setScheduledJobStatus(id, "failed", time.Time{})
}

func (r *Repository) setScheduledJobStatus(id int64, status string, sentAt time.Time) bool {
	if r.db != nil {
		var result sql.Result
		var err error
		if sentAt.IsZero() {
			result, err = r.db.Exec(
				`UPDATE scheduled_jobs SET status = $2, sent_at = NULL WHERE id = $1`, id, status,
			)
		} else {
			result, err = r.db.Exec(
				`UPDATE scheduled_jobs SET status = $2, sent_at = $3 WHERE id = $1`,
				id, status, sentAt,
			)
		}
		if err != nil {
			r.logWriteError("mark_scheduled_job_"+status, "", "", err)
			return false
		}
		rows, _ := result.RowsAffected()
		return rows == 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.scheduledJobs[id]
	if !ok {
		return false
	}
	job.Status = status
	if !sentAt.IsZero() {
		value := sentAt.UTC()
		job.SentAt = &value
	} else {
		job.SentAt = nil
	}
	r.scheduledJobs[id] = job
	return true
}

// CancelJob cancels one still-pending job in the tenant/chat scope.
func (r *Repository) CancelJob(tenantID, chatID string, id int64) bool {
	if r.db != nil {
		result, err := r.db.Exec(
			`UPDATE scheduled_jobs SET status = 'cancelled'
			  WHERE id = $1 AND tenant_id = $2 AND chat_id = $3 AND status = 'pending'`,
			id, tenantID, chatID,
		)
		if err != nil {
			r.logWriteError("cancel_scheduled_job", tenantID, chatID, err)
			return false
		}
		rows, _ := result.RowsAffected()
		return rows == 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.scheduledJobs[id]
	if !ok || job.TenantID != tenantID || job.ChatID != chatID || job.Status != "pending" {
		return false
	}
	job.Status = "cancelled"
	r.scheduledJobs[id] = job
	return true
}

// ListJobs returns every job in a tenant/chat, newest fire time first.
func (r *Repository) ListJobs(tenantID, chatID string) []ScheduledJobRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, chat_id, bot_id, kind, target, fire_at, payload,
			        status, created_by, idempotency_key, created_at, sent_at
			   FROM scheduled_jobs
			  WHERE tenant_id = $1 AND chat_id = $2
			  ORDER BY fire_at DESC, id DESC`, tenantID, chatID,
		)
		if err != nil {
			return nil
		}
		jobs, scanErr := scanScheduledJobs(rows)
		_ = rows.Close()
		if scanErr != nil {
			return nil
		}
		return jobs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs := make([]ScheduledJobRecord, 0)
	for _, job := range r.scheduledJobs {
		if job.TenantID == tenantID && job.ChatID == chatID {
			jobs = append(jobs, cloneScheduledJob(job))
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].FireAt.Equal(jobs[j].FireAt) {
			return jobs[i].ID > jobs[j].ID
		}
		return jobs[i].FireAt.After(jobs[j].FireAt)
	})
	return jobs
}

func scanScheduledJobs(rows *sql.Rows) ([]ScheduledJobRecord, error) {
	jobs := make([]ScheduledJobRecord, 0)
	for rows.Next() {
		var job ScheduledJobRecord
		var botID sql.NullInt64
		var payload []byte
		var sentAt sql.NullTime
		if err := rows.Scan(
			&job.ID, &job.TenantID, &job.ChatID, &botID, &job.Kind, &job.Target,
			&job.FireAt, &payload, &job.Status, &job.CreatedBy,
			&job.IdempotencyKey, &job.CreatedAt, &sentAt,
		); err != nil {
			return nil, err
		}
		if botID.Valid {
			job.BotID = strconv.FormatInt(botID.Int64, 10)
		}
		if err := json.Unmarshal(payload, &job.Payload); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			value := sentAt.Time
			job.SentAt = &value
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func cloneScheduledJob(job ScheduledJobRecord) ScheduledJobRecord {
	if job.Payload != nil {
		job.Payload = cloneJSONMap(job.Payload)
	}
	if job.SentAt != nil {
		value := *job.SentAt
		job.SentAt = &value
	}
	return job
}

func cloneJSONMap(input map[string]any) map[string]any {
	raw, err := json.Marshal(input)
	if err != nil {
		return map[string]any{}
	}
	var output map[string]any
	if json.Unmarshal(raw, &output) != nil {
		return map[string]any{}
	}
	return output
}
