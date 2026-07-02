package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"doubleu/backend-go/internal/contracts"
)

// CreateInteractionForm stores a new questionnaire and supersedes any form that
// is still active in the same chat, so the one-active-per-chat invariant (and
// the partial unique index from migration 116) always holds.
func (r *Repository) CreateInteractionForm(form InteractionFormRecord) (InteractionFormRecord, error) {
	if form.TenantID == "" || form.ChatID == "" {
		return InteractionFormRecord{}, fmt.Errorf("interaction form requires tenant and chat scope")
	}
	if len(form.Questions) == 0 {
		return InteractionFormRecord{}, fmt.Errorf("interaction form requires at least one question")
	}
	form.Status = "active"
	form.CurrentIndex = 0
	form.Answers = nil
	if form.CreatedAt.IsZero() {
		form.CreatedAt = time.Now().UTC()
	}
	if form.ExpiresAt.IsZero() {
		form.ExpiresAt = form.CreatedAt.Add(interactionFormTTL)
	}

	if r.db != nil {
		questions, err := json.Marshal(form.Questions)
		if err != nil {
			return InteractionFormRecord{}, err
		}
		tx, err := r.db.Begin()
		if err != nil {
			return InteractionFormRecord{}, err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(
			`UPDATE interaction_forms SET status = 'cancelled'
			  WHERE tenant_id = $1 AND chat_id = $2 AND status = 'active'`,
			form.TenantID, form.ChatID,
		); err != nil {
			return InteractionFormRecord{}, err
		}
		var botID any
		if form.BotID != "" {
			parsed, parseErr := strconv.ParseInt(form.BotID, 10, 64)
			if parseErr != nil {
				return InteractionFormRecord{}, parseErr
			}
			botID = parsed
		}
		if err := tx.QueryRow(
			`INSERT INTO interaction_forms
			   (tenant_id, chat_id, bot_id, questions, current_index, answers,
			    status, source_message_id, created_at, expires_at)
			 VALUES ($1,$2,$3,$4::jsonb,0,'[]'::jsonb,'active',$5,$6,$7)
			 RETURNING id`,
			form.TenantID, form.ChatID, botID, string(questions),
			form.SourceMessageID, form.CreatedAt, form.ExpiresAt,
		).Scan(&form.ID); err != nil {
			return InteractionFormRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return InteractionFormRecord{}, err
		}
		return cloneInteractionForm(form), nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.interactionForms {
		if existing.TenantID == form.TenantID && existing.ChatID == form.ChatID && existing.Status == "active" {
			existing.Status = "cancelled"
			r.interactionForms[id] = existing
		}
	}
	r.interactionFormSeq++
	form.ID = r.interactionFormSeq
	r.interactionForms[form.ID] = cloneInteractionForm(form)
	return cloneInteractionForm(form), nil
}

// ActiveInteractionForm returns the chat's in-progress questionnaire, if one is
// active and not expired.
func (r *Repository) ActiveInteractionForm(tenantID, chatID string) (InteractionFormRecord, bool) {
	return r.ActiveInteractionFormAt(tenantID, chatID, time.Now().UTC())
}

// ActiveInteractionFormAt is the clock-injected variant used by service flows
// that create and consume forms under a testable service clock.
func (r *Repository) ActiveInteractionFormAt(tenantID, chatID string, now time.Time) (InteractionFormRecord, bool) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if r.db != nil {
		row := r.db.QueryRow(
			`SELECT id, tenant_id, chat_id, bot_id, questions, current_index, answers,
			        status, source_message_id, created_at, expires_at
			   FROM interaction_forms
			  WHERE tenant_id = $1 AND chat_id = $2 AND status = 'active'
			    AND expires_at > $3
			  ORDER BY id DESC
			  LIMIT 1`, tenantID, chatID, now,
		)
		form, err := scanInteractionForm(row)
		if err != nil {
			return InteractionFormRecord{}, false
		}
		return form, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, form := range r.interactionForms {
		if form.TenantID == tenantID && form.ChatID == chatID &&
			form.Status == "active" && form.ExpiresAt.After(now) {
			return cloneInteractionForm(form), true
		}
	}
	return InteractionFormRecord{}, false
}

// RecordFormAnswer appends the contact's answer to the current question and
// advances the cursor. It returns the updated record so the caller can decide
// whether more questions remain. The form must still be active.
func (r *Repository) RecordFormAnswer(id int64, answer string) (InteractionFormRecord, bool) {
	if r.db != nil {
		tx, err := r.db.Begin()
		if err != nil {
			return InteractionFormRecord{}, false
		}
		defer func() { _ = tx.Rollback() }()
		row := tx.QueryRow(
			`SELECT id, tenant_id, chat_id, bot_id, questions, current_index, answers,
			        status, source_message_id, created_at, expires_at
			   FROM interaction_forms
			  WHERE id = $1 AND status = 'active'
			  FOR UPDATE`, id,
		)
		form, err := scanInteractionForm(row)
		if err != nil {
			return InteractionFormRecord{}, false
		}
		appendFormAnswer(&form, answer)
		answers, err := json.Marshal(form.Answers)
		if err != nil {
			return InteractionFormRecord{}, false
		}
		if _, err := tx.Exec(
			`UPDATE interaction_forms SET current_index = $2, answers = $3::jsonb
			  WHERE id = $1`, id, form.CurrentIndex, string(answers),
		); err != nil {
			return InteractionFormRecord{}, false
		}
		if err := tx.Commit(); err != nil {
			return InteractionFormRecord{}, false
		}
		return form, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	form, ok := r.interactionForms[id]
	if !ok || form.Status != "active" {
		return InteractionFormRecord{}, false
	}
	appendFormAnswer(&form, answer)
	r.interactionForms[id] = cloneInteractionForm(form)
	return cloneInteractionForm(form), true
}

// CompleteForm marks a questionnaire finished once every question is answered.
func (r *Repository) CompleteForm(id int64) bool {
	return r.setInteractionFormStatus(id, "completed")
}

// CancelForm abandons a questionnaire (e.g. expiry or owner override).
func (r *Repository) CancelForm(id int64) bool {
	return r.setInteractionFormStatus(id, "cancelled")
}

func (r *Repository) setInteractionFormStatus(id int64, status string) bool {
	if r.db != nil {
		result, err := r.db.Exec(
			`UPDATE interaction_forms SET status = $2 WHERE id = $1 AND status = 'active'`,
			id, status,
		)
		if err != nil {
			r.logWriteError("set_interaction_form_"+status, "", "", err)
			return false
		}
		rows, _ := result.RowsAffected()
		return rows == 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	form, ok := r.interactionForms[id]
	if !ok || form.Status != "active" {
		return false
	}
	form.Status = status
	r.interactionForms[id] = form
	return true
}

// interactionFormTTL bounds how long a half-finished questionnaire stays active
// before it is treated as abandoned.
const interactionFormTTL = 24 * time.Hour

// appendFormAnswer records the answer to the question at CurrentIndex and moves
// the cursor forward, pairing each answer with its prompt for later recall.
func appendFormAnswer(form *InteractionFormRecord, answer string) {
	prompt := ""
	if form.CurrentIndex >= 0 && form.CurrentIndex < len(form.Questions) {
		prompt = form.Questions[form.CurrentIndex].Prompt
	}
	form.Answers = append(form.Answers, FormAnswer{Prompt: prompt, Answer: answer})
	form.CurrentIndex++
}

// QuestionsRemaining reports whether the form still has unanswered questions.
func (f InteractionFormRecord) QuestionsRemaining() bool {
	return f.CurrentIndex < len(f.Questions)
}

// CurrentQuestion returns the question awaiting an answer, if any.
func (f InteractionFormRecord) CurrentQuestion() (contracts.Question, bool) {
	if f.CurrentIndex < 0 || f.CurrentIndex >= len(f.Questions) {
		return contracts.Question{}, false
	}
	return f.Questions[f.CurrentIndex], true
}

func scanInteractionForm(row rowScanner) (InteractionFormRecord, error) {
	var form InteractionFormRecord
	var botID sql.NullInt64
	var questions, answers []byte
	var sourceMessageID sql.NullString
	if err := row.Scan(
		&form.ID, &form.TenantID, &form.ChatID, &botID, &questions, &form.CurrentIndex,
		&answers, &form.Status, &sourceMessageID, &form.CreatedAt, &form.ExpiresAt,
	); err != nil {
		return InteractionFormRecord{}, err
	}
	if botID.Valid {
		form.BotID = strconv.FormatInt(botID.Int64, 10)
	}
	if sourceMessageID.Valid {
		form.SourceMessageID = sourceMessageID.String
	}
	if err := json.Unmarshal(questions, &form.Questions); err != nil {
		return InteractionFormRecord{}, err
	}
	if len(answers) > 0 {
		if err := json.Unmarshal(answers, &form.Answers); err != nil {
			return InteractionFormRecord{}, err
		}
	}
	return form, nil
}

func cloneInteractionForm(form InteractionFormRecord) InteractionFormRecord {
	if form.Questions != nil {
		questions := make([]contracts.Question, len(form.Questions))
		copy(questions, form.Questions)
		form.Questions = questions
	}
	if form.Answers != nil {
		answers := make([]FormAnswer, len(form.Answers))
		copy(answers, form.Answers)
		form.Answers = answers
	}
	return form
}
