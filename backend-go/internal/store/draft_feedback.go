package store

import (
	"doubleu/backend-go/internal/contracts"
)

// SaveDraftFeedback stores owner feedback on a generated draft.
func (r *Repository) SaveDraftFeedback(feedback contracts.DraftFeedback) {
	r.invalidateContextScore(feedback.TenantID, feedback.ChatID)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draftFeedbacks = append(r.draftFeedbacks, feedback)
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO draft_feedback (draft_id, tenant_id, chat_id, rating, feedback_text, response_text, model_used, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (draft_id, rating) DO NOTHING`,
			feedback.DraftID,
			feedback.TenantID,
			feedback.ChatID,
			feedback.Rating,
			feedback.FeedbackText,
			feedback.ResponseText,
			feedback.ModelUsed,
			feedback.CreatedAt,
		)
	}
}

// RecentFeedback returns the most recent draft feedback entries for a chat.
func (r *Repository) RecentFeedback(tenantID, chatID string, limit int) []contracts.DraftFeedback {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT draft_id, tenant_id, chat_id, rating, COALESCE(feedback_text,''), COALESCE(response_text,''), COALESCE(model_used,''), created_at
			   FROM draft_feedback
			  WHERE tenant_id = $1 AND chat_id = $2
			  ORDER BY created_at DESC
			  LIMIT $3`,
			tenantID, chatID, limit,
		)
		if err == nil {
			defer rows.Close()
			result := make([]contracts.DraftFeedback, 0)
			for rows.Next() {
				var fb contracts.DraftFeedback
				if rows.Scan(&fb.DraftID, &fb.TenantID, &fb.ChatID, &fb.Rating, &fb.FeedbackText, &fb.ResponseText, &fb.ModelUsed, &fb.CreatedAt) == nil {
					result = append(result, fb)
				}
			}
			return result
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]contracts.DraftFeedback, 0)
	for i := len(r.draftFeedbacks) - 1; i >= 0 && len(result) < limit; i-- {
		fb := r.draftFeedbacks[i]
		if fb.TenantID == tenantID && fb.ChatID == chatID {
			result = append(result, fb)
		}
	}
	return result
}
