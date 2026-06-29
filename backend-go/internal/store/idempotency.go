package store

// MarkReceive returns true when the receive idempotency key was already seen.
func (r *Repository) MarkReceive(key string) bool {
	if r.db != nil {
		return r.markIdempotency(key, "receive_update", SendRecord{})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.receiveKeys[key]; ok {
		return true
	}
	r.receiveKeys[key] = struct{}{}
	return false
}

// MarkCallback returns true when the approval callback key was already seen.
func (r *Repository) MarkCallback(key string) bool {
	if r.db != nil {
		return r.markIdempotency(key, "approval_callback", SendRecord{})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.callbackKeys[key]; ok {
		return true
	}
	r.callbackKeys[key] = struct{}{}
	return false
}

// MarkSend returns true when the send idempotency key was already seen.
func (r *Repository) MarkSend(record SendRecord) bool {
	if r.db != nil {
		return r.markIdempotency(record.Key, "send_reply", record)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sendKeys[record.Key]; ok {
		return true
	}
	r.sendKeys[record.Key] = record
	return false
}

// SendCount returns the number of unique outbound sends.
func (r *Repository) SendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sendKeys)
}

func (r *Repository) markIdempotency(key string, operation string, record SendRecord) bool {
	result, err := r.db.Exec(
		`INSERT INTO idempotency_records (
		   idempotency_key, operation, tenant_id, chat_id, source_message_id, decision_id, completed_at
		 ) VALUES ($1, $2::idempotency_operation, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), now())
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		key,
		operation,
		record.TenantID,
		record.ChatID,
		record.SourceMessageID,
		record.DecisionID,
	)
	if err != nil {
		return false
	}
	affected, err := result.RowsAffected()
	return err == nil && affected == 0
}
