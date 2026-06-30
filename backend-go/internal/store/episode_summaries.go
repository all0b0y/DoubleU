package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// ============================================================
// Runtime State Layer (active_episodes table)
// ============================================================

// UpsertActiveEpisode creates or updates the active episode for a chat.
// On first message: inserts a new episode. On subsequent messages: updates last_message_at
// and message_count. The episode is closed (deleted) externally when silence > 6h or
// message_count > 40.
func (r *Repository) UpsertActiveEpisode(episode ActiveEpisodeRecord) {
	r.mu.Lock()
	r.activeEpisodes[chatKey(episode.TenantID, episode.ChatID)] = episode
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`INSERT INTO active_episodes (
		   tenant_id, chat_id, started_at, last_message_at, message_count,
		   current_topic, open_question_message_id, pending_owner_action, role_context_id, updated_at
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		 ON CONFLICT (tenant_id, chat_id) DO UPDATE SET
		   last_message_at         = EXCLUDED.last_message_at,
		   message_count           = active_episodes.message_count + 1,
		   current_topic           = COALESCE(EXCLUDED.current_topic, active_episodes.current_topic),
		   open_question_message_id = COALESCE(EXCLUDED.open_question_message_id, active_episodes.open_question_message_id),
		   pending_owner_action    = COALESCE(EXCLUDED.pending_owner_action, active_episodes.pending_owner_action),
		   role_context_id         = COALESCE(EXCLUDED.role_context_id, active_episodes.role_context_id),
		   updated_at              = now()`,
		episode.TenantID, episode.ChatID,
		episode.StartedAt, episode.LastMessageAt, episode.MessageCount,
		nullableString(episode.CurrentTopic),
		nullableString(episode.OpenQuestionMsgID),
		nullableString(episode.PendingOwnerAction),
		nullableInt64(episode.RoleContextID),
	)
	r.logWriteError("upsert_active_episode", episode.TenantID, episode.ChatID, err)
}

// UpdateEpisodeRuntime sets runtime-state fields on the active episode without
// touching message_count (MEM-5): UpsertActiveEpisode increments the counter on
// conflict, so using it for post-AI runtime writes would inflate the count.
// Empty arguments leave the corresponding field unchanged.
func (r *Repository) UpdateEpisodeRuntime(tenantID, chatID, topic, openQuestionMsgID, pendingOwnerAction string) {
	r.mu.Lock()
	key := chatKey(tenantID, chatID)
	if ep, ok := r.activeEpisodes[key]; ok {
		if topic != "" {
			ep.CurrentTopic = topic
		}
		if openQuestionMsgID != "" {
			ep.OpenQuestionMsgID = openQuestionMsgID
		}
		if pendingOwnerAction != "" {
			ep.PendingOwnerAction = pendingOwnerAction
		}
		r.activeEpisodes[key] = ep
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`UPDATE active_episodes
		    SET current_topic            = COALESCE(NULLIF($3,''), current_topic),
		        open_question_message_id = COALESCE(NULLIF($4,''), open_question_message_id),
		        pending_owner_action     = COALESCE(NULLIF($5,''), pending_owner_action),
		        updated_at               = now()
		  WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID, topic, openQuestionMsgID, pendingOwnerAction,
	)
	r.logWriteError("update_episode_runtime", tenantID, chatID, err)
}

// ClearEpisodeRuntimeState nulls open_question_message_id and
// pending_owner_action on the active episode (MEM-5). A separate method is
// required because UpsertActiveEpisode COALESCEs those columns and therefore
// cannot reset them.
func (r *Repository) ClearEpisodeRuntimeState(tenantID, chatID string) {
	r.mu.Lock()
	key := chatKey(tenantID, chatID)
	if ep, ok := r.activeEpisodes[key]; ok {
		ep.OpenQuestionMsgID = ""
		ep.PendingOwnerAction = ""
		r.activeEpisodes[key] = ep
	}
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`UPDATE active_episodes
		    SET open_question_message_id = NULL,
		        pending_owner_action = NULL,
		        updated_at = now()
		  WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
	r.logWriteError("clear_episode_runtime_state", tenantID, chatID, err)
}

// ActiveEpisodeByChat returns the current open episode for a chat, if one exists.
func (r *Repository) ActiveEpisodeByChat(tenantID, chatID string) (ActiveEpisodeRecord, bool) {
	if r.db != nil {
		var ep ActiveEpisodeRecord
		var currentTopic sql.NullString
		var openQuestion sql.NullString
		var pendingAction sql.NullString
		err := r.db.QueryRow(
			`SELECT tenant_id, chat_id, started_at, last_message_at, message_count,
			        current_topic, open_question_message_id, pending_owner_action,
			        COALESCE(role_context_id, 0)
			   FROM active_episodes
			  WHERE tenant_id = $1 AND chat_id = $2`,
			tenantID, chatID,
		).Scan(
			&ep.TenantID, &ep.ChatID,
			&ep.StartedAt, &ep.LastMessageAt, &ep.MessageCount,
			&currentTopic, &openQuestion, &pendingAction, &ep.RoleContextID,
		)
		if err == nil {
			ep.CurrentTopic = currentTopic.String
			ep.OpenQuestionMsgID = openQuestion.String
			ep.PendingOwnerAction = pendingAction.String
			return ep, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.activeEpisodes[chatKey(tenantID, chatID)]
	return ep, ok
}

// ActiveEpisodeByChatForRoleContext returns the active episode only when it
// belongs to the requested bot role memory context.
func (r *Repository) ActiveEpisodeByChatForRoleContext(tenantID, chatID string, roleContextID int64) (ActiveEpisodeRecord, bool) {
	if roleContextID == 0 {
		return ActiveEpisodeRecord{}, false
	}
	if r.db != nil {
		var ep ActiveEpisodeRecord
		var currentTopic sql.NullString
		var openQuestion sql.NullString
		var pendingAction sql.NullString
		err := r.db.QueryRow(
			`SELECT tenant_id, chat_id, started_at, last_message_at, message_count,
			        current_topic, open_question_message_id, pending_owner_action,
			        COALESCE(role_context_id, 0)
			   FROM active_episodes
			  WHERE tenant_id = $1 AND chat_id = $2 AND role_context_id = $3`,
			tenantID, chatID, roleContextID,
		).Scan(
			&ep.TenantID, &ep.ChatID,
			&ep.StartedAt, &ep.LastMessageAt, &ep.MessageCount,
			&currentTopic, &openQuestion, &pendingAction, &ep.RoleContextID,
		)
		if err == nil {
			ep.CurrentTopic = currentTopic.String
			ep.OpenQuestionMsgID = openQuestion.String
			ep.PendingOwnerAction = pendingAction.String
			return ep, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.activeEpisodes[chatKey(tenantID, chatID)]
	return ep, ok && ep.RoleContextID == roleContextID
}

// CloseActiveEpisode deletes the active episode record, signalling that it has been
// materialised into episode_summaries.
func (r *Repository) CloseActiveEpisode(tenantID, chatID string) {
	r.mu.Lock()
	delete(r.activeEpisodes, chatKey(tenantID, chatID))
	r.mu.Unlock()
	if r.db == nil {
		return
	}
	_, err := r.db.Exec(
		`DELETE FROM active_episodes WHERE tenant_id = $1 AND chat_id = $2`,
		tenantID, chatID,
	)
	r.logWriteError("close_active_episode", tenantID, chatID, err)
}

// LogExtraction records one async fact-extraction attempt to the extraction_log table.
// Best-effort: a failure must never break the extraction path, but it is logged
// (MEM-10) so a broken diagnostics table is itself observable.
// No-op when running without a DB (unit tests use the in-memory repository).

// ============================================================
// Episodic Layer (episode_summaries table)
// ============================================================

// SaveEpisodeSummary stores a compressed summary of a closed conversation episode.
// Returns the inserted row ID (0 in memory-only mode), used by the embedding worker.
func (r *Repository) SaveEpisodeSummary(summary EpisodeSummaryRecord) int64 {
	r.mu.Lock()
	r.episodeSummaries = append(r.episodeSummaries, summary)
	r.mu.Unlock()
	if r.db == nil {
		return 0
	}
	var id int64
	err := r.db.QueryRow(
		`INSERT INTO episode_summaries (
		   tenant_id, chat_id, period_start, period_end, content,
		   message_count, closure_reason, source_message_ids, role_context_id
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id`,
		summary.TenantID, summary.ChatID,
		summary.PeriodStart, summary.PeriodEnd,
		summary.Content, summary.MessageCount,
		valueOrDefault(summary.ClosureReason, "manual"),
		pq.Array(nilToEmpty(summary.SourceMessageIDs)),
		nullableInt64(summary.RoleContextID),
	).Scan(&id)
	// id=0 already signals failure to callers; the log makes the cause visible
	// (a CHECK-constraint violation here once silently dropped all summaries — MEM-10).
	r.logWriteError("save_episode_summary", summary.TenantID, summary.ChatID, err)
	return id
}

// SaveEpisodeSummaryEmbedding persists the embedding vector for an episode summary row.
// Called by the embedding worker after the Python service generates the vector.
func (r *Repository) SaveEpisodeSummaryEmbedding(id int64, tenantID string, embedding []float64, model string) {
	if r.db == nil || len(embedding) == 0 {
		return
	}
	vec := float64SliceToVector(embedding)
	_, err := r.db.Exec(
		`UPDATE episode_summaries
		    SET embedding = $1::vector, embedding_model = $2
		  WHERE id = $3 AND tenant_id = $4`,
		vec, model, id, tenantID,
	)
	// A silent failure here once made the worker re-embed the same rows forever
	// (missing column, fixed in migration 012) — keep it observable (MEM-10).
	r.logWriteError("save_episode_summary_embedding", tenantID, "", err)
}

// EpisodeSummariesWithoutEmbeddings returns up to limit episode summary rows that
// have no embedding vector yet. Used by the embedding worker for batch embedding.
func (r *Repository) EpisodeSummariesWithoutEmbeddings(limit int) []EpisodeSummaryRecord {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.Query(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at,
		        COALESCE(role_context_id, 0)
		   FROM episode_summaries
		  WHERE embedding IS NULL
		    AND deleted_at IS NULL
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]EpisodeSummaryRecord, 0, limit)
	for rows.Next() {
		var s EpisodeSummaryRecord
		if rows.Scan(
			&s.ID, &s.TenantID, &s.ChatID,
			&s.PeriodStart, &s.PeriodEnd,
			&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
			&s.RoleContextID,
		) == nil {
			result = append(result, s)
		}
	}
	return result
}

// EpisodeSummaries returns recent episode summaries for a chat, newest first.
func (r *Repository) EpisodeSummaries(tenantID, chatID string, limit int) []EpisodeSummaryRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, chat_id, period_start, period_end,
			        content, closure_reason, message_count, created_at,
			        COALESCE(role_context_id, 0)
			   FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND deleted_at IS NULL
			  ORDER BY period_end DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			result := make([]EpisodeSummaryRecord, 0)
			for rows.Next() {
				var s EpisodeSummaryRecord
				if rows.Scan(
					&s.ID, &s.TenantID, &s.ChatID,
					&s.PeriodStart, &s.PeriodEnd,
					&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
					&s.RoleContextID,
				) == nil {
					result = append(result, s)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]EpisodeSummaryRecord, 0)
	for i := len(r.episodeSummaries) - 1; i >= 0 && len(result) < limit; i-- {
		s := r.episodeSummaries[i]
		if s.TenantID == tenantID && s.ChatID == chatID {
			result = append(result, s)
		}
	}
	return result
}

// EpisodeSummariesForRoleContext returns recent episode summaries for one bot
// role memory context, newest first.
func (r *Repository) EpisodeSummariesForRoleContext(tenantID, chatID string, limit int, roleContextID int64) []EpisodeSummaryRecord {
	if limit <= 0 || roleContextID == 0 {
		return nil
	}
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT id, tenant_id, chat_id, period_start, period_end,
			        content, closure_reason, message_count, created_at,
			        COALESCE(role_context_id, 0)
			   FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2 AND role_context_id = $3
			    AND deleted_at IS NULL
			  ORDER BY period_end DESC
			  LIMIT $4`,
			tenantID, chatID, roleContextID, limit,
		)
		if err == nil {
			defer rows.Close()
			result := make([]EpisodeSummaryRecord, 0)
			for rows.Next() {
				var s EpisodeSummaryRecord
				if rows.Scan(
					&s.ID, &s.TenantID, &s.ChatID,
					&s.PeriodStart, &s.PeriodEnd,
					&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
					&s.RoleContextID,
				) == nil {
					result = append(result, s)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]EpisodeSummaryRecord, 0)
	for i := len(r.episodeSummaries) - 1; i >= 0 && len(result) < limit; i-- {
		s := r.episodeSummaries[i]
		if s.TenantID == tenantID && s.ChatID == chatID && s.RoleContextID == roleContextID {
			result = append(result, s)
		}
	}
	return result
}

// float64SliceToVector converts a Go float64 slice to a pgvector literal string.
// Format: "[0.1, 0.2, ...]" — distinct from the PostgreSQL array literal "{0.1, ...}".
// Used to pass query vectors to ORDER BY embedding <=> $n::vector clauses.

// EpisodeSummariesByVector returns episode summaries ranked by cosine similarity to
// queryVector. Falls back to EpisodeSummaries (recency) when queryVector is empty or
// the embedding column has no data (pgvector unavailable or summaries not yet embedded).
func (r *Repository) EpisodeSummariesByVector(tenantID, chatID string, queryVector []float64, limit int) []EpisodeSummaryRecord {
	if r.db == nil || len(queryVector) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at,
		        COALESCE(role_context_id, 0)
		   FROM episode_summaries
		  WHERE tenant_id = $1 AND chat_id = $2
		    AND embedding IS NOT NULL
		    AND deleted_at IS NULL
		  ORDER BY %s ASC
		  LIMIT $4`,
		r.vectorOrderExpr("embedding", "$3"),
	),
		tenantID, chatID, vec, limit,
	)
	if err != nil {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	defer rows.Close()
	result := make([]EpisodeSummaryRecord, 0, limit)
	for rows.Next() {
		var s EpisodeSummaryRecord
		if rows.Scan(
			&s.ID, &s.TenantID, &s.ChatID,
			&s.PeriodStart, &s.PeriodEnd,
			&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
			&s.RoleContextID,
		) == nil {
			result = append(result, s)
		}
	}
	// If no embedded summaries found, fall back to recency.
	if len(result) == 0 {
		return r.EpisodeSummaries(tenantID, chatID, limit)
	}
	return result
}

// EpisodeSummariesByVectorForRoleContext returns episode summaries ranked by
// vector similarity inside one bot role memory context.
func (r *Repository) EpisodeSummariesByVectorForRoleContext(tenantID, chatID string, queryVector []float64, limit int, roleContextID int64) []EpisodeSummaryRecord {
	if roleContextID == 0 {
		return nil
	}
	if r.db == nil || len(queryVector) == 0 {
		return r.EpisodeSummariesForRoleContext(tenantID, chatID, limit, roleContextID)
	}
	vec := float64SliceToVector(queryVector)
	rows, err := r.db.Query(fmt.Sprintf(
		`SELECT id, tenant_id, chat_id, period_start, period_end,
		        content, closure_reason, message_count, created_at,
		        COALESCE(role_context_id, 0)
		   FROM episode_summaries
		  WHERE tenant_id = $1 AND chat_id = $2 AND role_context_id = $3
		    AND embedding IS NOT NULL
		    AND deleted_at IS NULL
		  ORDER BY %s ASC
		  LIMIT $5`,
		r.vectorOrderExpr("embedding", "$4"),
	),
		tenantID, chatID, roleContextID, vec, limit,
	)
	if err != nil {
		return r.EpisodeSummariesForRoleContext(tenantID, chatID, limit, roleContextID)
	}
	defer rows.Close()
	result := make([]EpisodeSummaryRecord, 0, limit)
	for rows.Next() {
		var s EpisodeSummaryRecord
		if rows.Scan(
			&s.ID, &s.TenantID, &s.ChatID,
			&s.PeriodStart, &s.PeriodEnd,
			&s.Content, &s.ClosureReason, &s.MessageCount, &s.CreatedAt,
			&s.RoleContextID,
		) == nil {
			result = append(result, s)
		}
	}
	if len(result) == 0 {
		return r.EpisodeSummariesForRoleContext(tenantID, chatID, limit, roleContextID)
	}
	return result
}

// EpisodeSummaryOverlaps reports whether any summary already covers part of
// the [start, end) period. This is the episode-closure idempotency check
// (MEM-6): the previous one-summary-per-day rule silently dropped the second
// episode of a day.
func (r *Repository) EpisodeSummaryOverlaps(tenantID, chatID string, start, end time.Time) bool {
	if r.db != nil {
		var exists bool
		err := r.db.QueryRow(
			`SELECT EXISTS(
			   SELECT 1 FROM episode_summaries
			    WHERE tenant_id = $1 AND chat_id = $2
			      AND period_end > $3 AND period_start < $4
			      AND deleted_at IS NULL
			 )`,
			tenantID, chatID, start, end,
		).Scan(&exists)
		if err == nil {
			return exists
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			s.PeriodEnd.After(start) && s.PeriodStart.Before(end) {
			return true
		}
	}
	return false
}

// LatestCoveredEnd returns the latest period_end among summaries overlapping
// [start, end), or the zero time when none overlap. The daily fallback uses it
// to summarise only the part of the day no episode summary covered (MEM-6).
func (r *Repository) LatestCoveredEnd(tenantID, chatID string, start, end time.Time) time.Time {
	if r.db != nil {
		var covered sql.NullTime
		err := r.db.QueryRow(
			`SELECT MAX(period_end) FROM episode_summaries
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND period_end > $3 AND period_start < $4
			    AND deleted_at IS NULL`,
			tenantID, chatID, start, end,
		).Scan(&covered)
		if err == nil {
			if covered.Valid {
				return covered.Time
			}
			return time.Time{}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest time.Time
	for _, s := range r.episodeSummaries {
		if s.TenantID == tenantID && s.ChatID == chatID &&
			s.PeriodEnd.After(start) && s.PeriodStart.Before(end) &&
			s.PeriodEnd.After(latest) {
			latest = s.PeriodEnd
		}
	}
	return latest
}
