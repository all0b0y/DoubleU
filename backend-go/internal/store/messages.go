package store

import (
	"fmt"
	"sort"
	"time"

	"doubleu/backend-go/internal/contracts"
	tracepkg "doubleu/backend-go/internal/trace"
)

// SaveMessage stores a normalized message record as non-quarantined.
// Call sites that classify quarantine should use SaveMessageQuarantined directly.
func (r *Repository) SaveMessage(event contracts.NormalizedTelegramEvent) {
	r.SaveMessageQuarantinedWithRoleContext(event, false, 0)
}

// SaveMessageWithRoleContext stores a normalized non-quarantined message in a
// reusable bot role memory context. A zero roleContextID preserves legacy NULL.
func (r *Repository) SaveMessageWithRoleContext(event contracts.NormalizedTelegramEvent, roleContextID int64) {
	r.SaveMessageQuarantinedWithRoleContext(event, false, roleContextID)
}

// SaveMessageQuarantined stores a normalized message record with an explicit
// quarantine flag. Quarantined messages persist for audit but are excluded from
// every memory surface (RecentMessages, summarizeEpisode, extraction).
func (r *Repository) SaveMessageQuarantined(event contracts.NormalizedTelegramEvent, quarantined bool) {
	r.SaveMessageQuarantinedWithRoleContext(event, quarantined, 0)
}

// SaveMessageQuarantinedWithRoleContext stores a message and tags bot memory
// writes with the active role context captured by the caller.
func (r *Repository) SaveMessageQuarantinedWithRoleContext(event contracts.NormalizedTelegramEvent, quarantined bool, roleContextID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := messageKey(event.TenantID, event.ChatID, event.SourceMessageID)
	record, existed := r.messages[key]
	record.Event = event
	record.Quarantined = quarantined
	if roleContextID != 0 || !existed {
		record.RoleContextID = roleContextID
	}
	r.messages[key] = record
	if !existed {
		// Per-chat insertion-order index so in-memory RecentMessages reads a
		// bounded slice instead of scanning every stored message (ARCH-13).
		ck := chatKey(event.TenantID, event.ChatID)
		r.chatMessageKeys[ck] = append(r.chatMessageKeys[ck], key)
	}
	if r.db != nil {
		_, _ = r.db.Exec(
			`INSERT INTO messages (
			   tenant_id, chat_id, source_message_id, event_id, telegram_update_id,
			   business_connection_id, sender_id, message_text, message_type, received_at,
			   quarantined, role_context_id
			 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			 ON CONFLICT (tenant_id, chat_id, source_message_id) DO UPDATE SET
			   event_id = EXCLUDED.event_id,
			   telegram_update_id = EXCLUDED.telegram_update_id,
			   business_connection_id = EXCLUDED.business_connection_id,
			   sender_id = EXCLUDED.sender_id,
			   message_text = EXCLUDED.message_text,
			   message_type = EXCLUDED.message_type,
			   quarantined = EXCLUDED.quarantined,
			   role_context_id = COALESCE(EXCLUDED.role_context_id, messages.role_context_id)`,
			event.TenantID,
			event.ChatID,
			event.SourceMessageID,
			event.EventID,
			event.TelegramUpdateID,
			event.BusinessConnectionID,
			event.SenderID,
			event.MessageText,
			event.MessageType,
			event.CreatedAt,
			quarantined,
			nullableInt64(roleContextID),
		)
		traceID := tracepkg.DeriveTraceID(event.EventID)
		for _, state := range record.States {
			r.insertState(event.TenantID, event.ChatID, event.SourceMessageID, state, traceID)
		}
		// Raw messages are NOT stored in memory tables — they live in `messages` only (Working Layer).
	}
}

// Message returns a stored message and whether it exists.
func (r *Repository) Message(tenantID, chatID, sourceMessageID string) (MessageRecord, bool) {
	if r.db != nil {
		var event contracts.NormalizedTelegramEvent
		var quarantined bool
		var record MessageRecord
		err := r.db.QueryRow(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at,
			        quarantined, COALESCE(role_context_id, 0)
			   FROM messages
			  WHERE tenant_id = $1 AND chat_id = $2 AND source_message_id = $3`,
			tenantID,
			chatID,
			sourceMessageID,
		).Scan(
			&event.EventID,
			&event.TenantID,
			&event.ChatID,
			&event.TelegramUpdateID,
			&event.BusinessConnectionID,
			&event.SourceMessageID,
			&event.SenderID,
			&event.MessageText,
			&event.MessageType,
			&event.CreatedAt,
			&quarantined,
			&record.RoleContextID,
		)
		if err == nil {
			record.Event = event
			record.Quarantined = quarantined
			return record, true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.messages[messageKey(tenantID, chatID, sourceMessageID)]
	if !ok || record.Event.EventID == "" {
		return MessageRecord{}, false
	}
	return record, true
}

// AppendState appends a message state to the operational record.
func (r *Repository) AppendState(tenantID, chatID, sourceMessageID string, state contracts.MessageState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := messageKey(tenantID, chatID, sourceMessageID)
	record := r.messages[key]
	record.States = append(record.States, state)
	r.messages[key] = record
	// Production callers in HandleTelegramWebhook now run SaveMessage before any
	// AppendState, so this fallback only fires for no_memory chats (intentional —
	// the messages row is never persisted) and for the synthetic event constructed
	// on normalization failure (no usable tenant/chat/source_message_id triple).
	// In both cases skipping the DB write avoids an FK violation against messages.
	//
	// trace_id is derived from the stored event_id rather than threaded through
	// the call site: every webhook produces the same UUIDv5-derived trace_id so
	// state/audit rows for one event automatically share a trace.
	if r.db != nil && record.Event.EventID != "" {
		r.insertState(tenantID, chatID, sourceMessageID, state, tracepkg.DeriveTraceID(record.Event.EventID))
	}
}

// AppendAudit stores an audit event.
//
// When event.TraceID is empty but the parent message_states rows already carry
// a derived trace_id, we fill it from the stored event_id so audit and state
// rows for the same webhook always share a trace.
func (r *Repository) AppendAudit(event contracts.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// See AppendState — production callers persist the message row before any
	// audit write.  The skip below only triggers for no_memory chats (intentional)
	// and the synthetic normalization-error audit (no parent message row exists).
	msgRecord := r.messages[messageKey(event.TenantID, event.ChatID, event.SourceMessageID)]
	if event.TraceID == "" && msgRecord.Event.EventID != "" {
		event.TraceID = tracepkg.DeriveTraceID(msgRecord.Event.EventID)
	}
	r.audits = append(r.audits, event)
	if r.db == nil {
		return
	}
	if msgRecord.Event.EventID == "" {
		return
	}
	var traceArg any = nil
	if event.TraceID != "" {
		traceArg = event.TraceID
	}
	_, _ = r.db.Exec(
		`INSERT INTO audit_events (tenant_id, chat_id, source_message_id, decision, reason, trace_id)
		 VALUES ($1, $2, $3, $4::decision, $5, $6)
		 ON CONFLICT (tenant_id, chat_id, source_message_id, decision, reason) DO NOTHING`,
		event.TenantID,
		event.ChatID,
		event.SourceMessageID,
		string(event.Decision),
		event.Reason,
		traceArg,
	)
}

// Audits returns a copy of all audit events.
func (r *Repository) Audits() []contracts.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.AuditEvent(nil), r.audits...)
}

// RecentMessages returns recent stored messages for a tenant/chat, oldest first.
func (r *Repository) RecentMessages(tenantID, chatID string, limit int) []MessageRecord {
	if limit <= 0 {
		return nil
	}
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at,
			        COALESCE(role_context_id, 0)
			   FROM (
			     SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			            source_message_id, sender_id, message_text, message_type, received_at,
			            role_context_id
			       FROM messages
			      WHERE tenant_id = $1 AND chat_id = $2 AND quarantined = false
			      ORDER BY received_at DESC
			      LIMIT $3
			   ) recent
			  ORDER BY received_at ASC`,
			tenantID,
			chatID,
			limit,
		)
		if err == nil {
			defer rows.Close()
			messages := make([]MessageRecord, 0)
			for rows.Next() {
				var event contracts.NormalizedTelegramEvent
				var roleContextID int64
				if rows.Scan(
					&event.EventID,
					&event.TenantID,
					&event.ChatID,
					&event.TelegramUpdateID,
					&event.BusinessConnectionID,
					&event.SourceMessageID,
					&event.SenderID,
					&event.MessageText,
					&event.MessageType,
					&event.CreatedAt,
					&roleContextID,
				) == nil {
					messages = append(messages, MessageRecord{Event: event, RoleContextID: roleContextID})
				}
			}
			return messages
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Per-chat index (ARCH-13): only this chat's messages are touched instead
	// of scanning the whole message map on every call.
	keys := r.chatMessageKeys[chatKey(tenantID, chatID)]
	messages := make([]MessageRecord, 0, len(keys))
	for _, key := range keys {
		if record, ok := r.messages[key]; ok && record.Event.EventID != "" && !record.Quarantined {
			messages = append(messages, record)
		}
	}
	// Insertion order is normally chronological; sort to keep the contract
	// strict for out-of-order saves (synthetic outgoing messages, tests).
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Event.CreatedAt.Before(messages[j].Event.CreatedAt)
	})
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return messages
}

// RecentMessagesForRoleContext returns recent stored messages for one bot role
// memory context. A zero roleContextID represents a missing active bot context
// and intentionally returns no messages instead of falling back to broad chat
// history.
func (r *Repository) RecentMessagesForRoleContext(tenantID, chatID string, limit int, roleContextID int64) []MessageRecord {
	if limit <= 0 || roleContextID == 0 {
		return nil
	}
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at,
			        COALESCE(role_context_id, 0)
			   FROM (
			     SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			            source_message_id, sender_id, message_text, message_type, received_at,
			            role_context_id
			       FROM messages
			      WHERE tenant_id = $1 AND chat_id = $2 AND role_context_id = $3 AND quarantined = false
			      ORDER BY received_at DESC
			      LIMIT $4
			   ) recent
			  ORDER BY received_at ASC`,
			tenantID,
			chatID,
			roleContextID,
			limit,
		)
		if err == nil {
			defer rows.Close()
			messages := make([]MessageRecord, 0)
			for rows.Next() {
				var event contracts.NormalizedTelegramEvent
				var scannedRoleContextID int64
				if rows.Scan(
					&event.EventID,
					&event.TenantID,
					&event.ChatID,
					&event.TelegramUpdateID,
					&event.BusinessConnectionID,
					&event.SourceMessageID,
					&event.SenderID,
					&event.MessageText,
					&event.MessageType,
					&event.CreatedAt,
					&scannedRoleContextID,
				) == nil {
					messages = append(messages, MessageRecord{Event: event, RoleContextID: scannedRoleContextID})
				}
			}
			return messages
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.chatMessageKeys[chatKey(tenantID, chatID)]
	messages := make([]MessageRecord, 0, len(keys))
	for _, key := range keys {
		if record, ok := r.messages[key]; ok &&
			record.Event.EventID != "" &&
			!record.Quarantined &&
			record.RoleContextID == roleContextID {
			messages = append(messages, record)
		}
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Event.CreatedAt.Before(messages[j].Event.CreatedAt)
	})
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	return messages
}

func (r *Repository) insertState(tenantID, chatID, sourceMessageID string, state contracts.MessageState, traceID string) {
	var traceArg any = nil
	if traceID != "" {
		traceArg = traceID
	}
	_, _ = r.db.Exec(
		`INSERT INTO message_states (tenant_id, chat_id, source_message_id, state, trace_id)
		 VALUES ($1, $2, $3, $4::message_state, $5)
		 ON CONFLICT (tenant_id, chat_id, source_message_id, state, actor) DO NOTHING`,
		tenantID,
		chatID,
		sourceMessageID,
		string(state),
		traceArg,
	)
}

// MessagesByDateRange returns all messages for a chat in the half-open interval [from, to).
func (r *Repository) MessagesByDateRange(tenantID, chatID string, from, to time.Time) []MessageRecord {
	if r.db != nil {
		rows, err := r.db.Query(
			`SELECT event_id, tenant_id, chat_id, telegram_update_id, business_connection_id,
			        source_message_id, sender_id, message_text, message_type, received_at,
			        quarantined, COALESCE(role_context_id, 0)
			   FROM messages
			  WHERE tenant_id = $1 AND chat_id = $2
			    AND received_at >= $3 AND received_at < $4
			  ORDER BY received_at ASC`,
			tenantID, chatID, from, to,
		)
		if err == nil {
			defer rows.Close()
			messages := make([]MessageRecord, 0)
			for rows.Next() {
				var event contracts.NormalizedTelegramEvent
				var quarantined bool
				var roleContextID int64
				if rows.Scan(
					&event.EventID, &event.TenantID, &event.ChatID,
					&event.TelegramUpdateID, &event.BusinessConnectionID,
					&event.SourceMessageID, &event.SenderID,
					&event.MessageText, &event.MessageType, &event.CreatedAt,
					&quarantined, &roleContextID,
				) == nil {
					messages = append(messages, MessageRecord{Event: event, Quarantined: quarantined, RoleContextID: roleContextID})
				}
			}
			return messages
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	messages := make([]MessageRecord, 0)
	for _, record := range r.messages {
		e := record.Event
		if e.TenantID == tenantID && e.ChatID == chatID && e.EventID != "" &&
			!e.CreatedAt.Before(from) && e.CreatedAt.Before(to) {
			messages = append(messages, record)
		}
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Event.CreatedAt.Before(messages[j].Event.CreatedAt)
	})
	return messages
}

func messageKey(tenantID, chatID, sourceMessageID string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, chatID, sourceMessageID)
}
