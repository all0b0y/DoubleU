package store

import (
	"sort"

	"github.com/lib/pq"

	"doubleu/backend-go/internal/contracts"
)

// DraftStatus is the owner approval lifecycle status for a draft.
type DraftStatus string

const (
	// DraftPending means the draft is waiting for owner action.
	DraftPending DraftStatus = "pending"
	// DraftApproved means the owner approved the draft.
	DraftApproved DraftStatus = "approved"
	// DraftRejected means the owner rejected the draft.
	DraftRejected DraftStatus = "rejected"
	// DraftSent means the approved draft was sent to the recipient.
	DraftSent DraftStatus = "sent"
)

// SaveDraft stores an owner-facing draft in memory and, when a DB is configured,
// persists it to the drafts table so owner_message_id survives restarts.
func (r *Repository) SaveDraft(draft DraftRecord) {
	r.mu.Lock()
	r.drafts[draft.DraftID] = draft
	r.draftsByMsg[messageKey(draft.TenantID, draft.ChatID, draft.SourceMessageID)] = draft.DraftID
	r.mu.Unlock()
	if r.db != nil {
		var traceArg any = nil
		if draft.TraceID != "" {
			traceArg = draft.TraceID
		}
		_, _ = r.db.Exec(
			`INSERT INTO drafts (
			   draft_id, decision_id, tenant_id, chat_id, business_connection_id,
			   source_message_id, source_message_text, response_text,
			   text_format, draft_type, suggested_options, status,
			   owner_chat_id, owner_message_id, sent_message_id, action_history,
			   trace_id, created_at, updated_at
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$18)
			 ON CONFLICT (draft_id) DO NOTHING`,
			draft.DraftID, draft.DecisionID, draft.TenantID, draft.ChatID,
			draft.BusinessConnectionID, draft.SourceMessageID, draft.SourceMessageText,
			draft.ResponseText, string(draft.TextFormat.Normalize()), valueOrDefault(draft.DraftType, "reply"),
			pq.Array(nilToEmpty(draft.SuggestedOptions)), string(valueOrDefault(string(draft.Status), "pending")),
			draft.OwnerChatID, draft.OwnerMessageID, draft.SentMessageID,
			pq.Array(nilToEmpty(draft.ActionHistory)), traceArg, draft.CreatedAt,
		)
	}
}

// UpdateDraft persists changes to an existing draft (status, owner message IDs, etc.).
// Callers are responsible for setting UpdatedAt before calling.
func (r *Repository) UpdateDraft(draft DraftRecord) {
	r.mu.Lock()
	r.drafts[draft.DraftID] = draft
	r.mu.Unlock()
	if r.db != nil {
		_, _ = r.db.Exec(
			`UPDATE drafts SET
			   response_text    = $2,
			   text_format      = $3,
			   draft_type       = $4,
			   suggested_options= $5,
			   status           = $6,
			   owner_chat_id    = $7,
			   owner_message_id = $8,
			   sent_message_id  = $9,
			   action_history   = $10,
			   updated_at       = now()
			 WHERE draft_id = $1`,
			draft.DraftID, draft.ResponseText, string(draft.TextFormat.Normalize()),
			valueOrDefault(draft.DraftType, "reply"),
			pq.Array(nilToEmpty(draft.SuggestedOptions)), string(valueOrDefault(string(draft.Status), "pending")),
			draft.OwnerChatID, draft.OwnerMessageID, draft.SentMessageID,
			pq.Array(nilToEmpty(draft.ActionHistory)),
		)
	}
}

// Draft returns a draft by ID. Falls back to the DB when not in memory (e.g. after restart).
func (r *Repository) Draft(draftID string) (DraftRecord, bool) {
	r.mu.Lock()
	draft, ok := r.drafts[draftID]
	r.mu.Unlock()
	if ok {
		return draft, true
	}
	if r.db != nil {
		d, dbOK := r.dbDraft(draftID)
		if dbOK {
			r.mu.Lock()
			r.drafts[d.DraftID] = d
			r.mu.Unlock()
		}
		return d, dbOK
	}
	return DraftRecord{}, false
}

// dbDraft fetches a single draft row from the database.
func (r *Repository) dbDraft(draftID string) (DraftRecord, bool) {
	var d DraftRecord
	var status string
	var draftType string
	var textFormat string
	var opts pq.StringArray
	var history pq.StringArray
	err := r.db.QueryRow(
		`SELECT draft_id, COALESCE(decision_id,''), tenant_id, chat_id,
		        COALESCE(business_connection_id,''), COALESCE(source_message_id,''),
		        COALESCE(source_message_text,''), COALESCE(response_text,''),
		        COALESCE(text_format,'plain'), draft_type, suggested_options, status,
		        COALESCE(owner_chat_id,''), COALESCE(owner_message_id,''),
		        COALESCE(sent_message_id,''), action_history,
		        COALESCE(trace_id,''), created_at, updated_at
		   FROM drafts WHERE draft_id = $1`,
		draftID,
	).Scan(
		&d.DraftID, &d.DecisionID, &d.TenantID, &d.ChatID,
		&d.BusinessConnectionID, &d.SourceMessageID,
		&d.SourceMessageText, &d.ResponseText,
		&textFormat, &draftType, &opts, &status,
		&d.OwnerChatID, &d.OwnerMessageID,
		&d.SentMessageID, &history, &d.TraceID, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return DraftRecord{}, false
	}
	d.TextFormat = contracts.TextFormat(textFormat).Normalize()
	d.DraftType = draftType
	d.Status = DraftStatus(status)
	d.SuggestedOptions = []string(opts)
	d.ActionHistory = []string(history)
	return d, true
}

// DraftByMessage returns the latest draft for a message.
// Falls back to the DB when the draft is not in memory (e.g. after restart).
func (r *Repository) DraftByMessage(tenantID, chatID, sourceMessageID string) (DraftRecord, bool) {
	r.mu.Lock()
	draftID, ok := r.draftsByMsg[messageKey(tenantID, chatID, sourceMessageID)]
	r.mu.Unlock()
	if ok {
		return r.Draft(draftID)
	}
	if r.db != nil {
		var id string
		err := r.db.QueryRow(
			`SELECT draft_id FROM drafts
			  WHERE tenant_id = $1 AND chat_id = $2 AND source_message_id = $3
			  ORDER BY created_at DESC LIMIT 1`,
			tenantID, chatID, sourceMessageID,
		).Scan(&id)
		if err == nil {
			return r.Draft(id)
		}
	}
	return DraftRecord{}, false
}

// DraftsForChat returns all drafts for a tenant/chat pair, newest first.
func (r *Repository) DraftsForChat(tenantID, chatID string) []DraftRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []DraftRecord
	for _, draft := range r.drafts {
		if draft.TenantID == tenantID && draft.ChatID == chatID {
			result = append(result, draft)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// DraftBySentMessageID returns the draft whose reply was sent as the given Telegram message_id.
func (r *Repository) DraftBySentMessageID(tenantID, chatID, sentMessageID string) (DraftRecord, bool) {
	if sentMessageID == "" {
		return DraftRecord{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.drafts {
		if d.TenantID == tenantID && d.ChatID == chatID && d.SentMessageID == sentMessageID {
			return d, true
		}
	}
	return DraftRecord{}, false
}
