package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

func (s *Service) handleQFormCallback(ctx context.Context, cbq telegram.CallbackQueryUpdate, callbackSender telegram.Sender, formID int64, questionIdx, optionIdx int, freeText bool, idempotencyKey string) InboundResult {
	event := contracts.NormalizedTelegramEvent{
		EventID:          fmt.Sprintf("%d:callback_query:%s", cbq.UpdateID, cbq.QueryID),
		TenantID:         cbq.TenantID,
		ChatID:           contracts.BotChatID(cbq.BotID, cbq.ChatID),
		TelegramUpdateID: cbq.UpdateID,
		SourceMessageID:  cbq.QueryID,
		SenderID:         cbq.SenderID,
		MessageType:      "callback_query",
		CreatedAt:        s.now(),
		Transport:        contracts.TransportBot,
		BotID:            cbq.BotID,
	}
	if cbq.Transport != contracts.TransportBot || cbq.TenantID == "" || cbq.BotID == "" {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_not_bot_transport"}
	}
	if s.repository.MarkCallback(idempotencyKey) {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_qform_callback", Duplicate: true}
	}
	form, ok := s.repository.ActiveInteractionFormAt(cbq.TenantID, event.ChatID, s.now())
	if !ok || form.ID != formID {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_not_found"}
	}
	if form.BotID != "" && form.BotID != cbq.BotID {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_bot_mismatch"}
	}
	if form.CurrentIndex != questionIdx {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_question_mismatch"}
	}
	question, ok := form.CurrentQuestion()
	if !ok {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_question_not_found"}
	}
	if freeText {
		_ = callbackSender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: cbq.TenantID,
			ChatID:   cbq.ChatID,
			Text:     "✏️\n" + question.Prompt,
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "form_free_text_requested"}
	}
	if optionIdx < 0 || optionIdx >= len(question.Options) {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_option_out_of_range"}
	}
	answer := question.Options[optionIdx]
	if cbq.OriginalMsgID != "" {
		selectedText := question.Prompt + "\n\n✓ " + answer
		if err := callbackSender.EditMessageText(ctx, cbq.ChatID, cbq.OriginalMsgID, selectedText, telegram.InlineKeyboardMarkup{}); err != nil {
			s.logger.Warn("interaction form message edit failed",
				"tenant_id", cbq.TenantID, "form_id", formID, "error", err)
		}
	}
	updated, ok := s.repository.RecordFormAnswer(form.ID, answer)
	if !ok {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "qform_not_active"}
	}
	s.logger.Info("interaction form option selected",
		"tenant_id", cbq.TenantID, "chat_id", event.ChatID,
		"form_id", formID, "question_idx", questionIdx, "option_idx", optionIdx)
	ownerID := strings.TrimPrefix(cbq.TenantID, "tenant:user:")
	if ownerID == cbq.TenantID {
		ownerID = ""
	}
	if updated.QuestionsRemaining() {
		if sentMsgID, err := s.sendInteractionQuestion(ctx, event, ownerID, updated, s.deliverySettingsFor(event)); err != nil {
			s.logger.Error("interaction form next question failed",
				"tenant_id", cbq.TenantID, "form_id", formID, "error", err)
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "qform_send_error"}
		} else if nextQuestion, ok := updated.CurrentQuestion(); ok {
			s.saveOutgoingMessage(event, ownerID, sentMsgID, nextQuestion.Prompt, fmt.Sprintf("%s:form:%d:q:%d", event.EventID, updated.ID, updated.CurrentIndex), contracts.TextFormatPlain)
		}
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "form_question_sent"}
	}
	s.repository.CompleteForm(updated.ID)
	event.MessageText = interactionAnswersText(updated)
	result, err := s.processContactMessage(ctx, event, ownerID, "")
	if err != nil {
		s.logger.Error("interaction form completion failed",
			"tenant_id", cbq.TenantID, "form_id", formID, "error", err)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "qform_process_error"}
	}
	return result
}

func (s *Service) contactInteractionQuestions(event contracts.NormalizedTelegramEvent, proposal contracts.AIProcessMessageResponse) ([]contracts.Question, bool) {
	if proposal.ResponsePlan == nil || proposal.ResponsePlan.Shape != "questions" || !s.contactInteractionEnabled(event) {
		return nil, false
	}
	return normalizeInteractionQuestions(proposal.ResponsePlan.Questions)
}

func normalizeInteractionQuestions(questions []contracts.Question) ([]contracts.Question, bool) {
	if len(questions) < 1 || len(questions) > 6 {
		return nil, false
	}
	normalized := make([]contracts.Question, 0, len(questions))
	for _, question := range questions {
		prompt := strings.TrimSpace(question.Prompt)
		if prompt == "" {
			return nil, false
		}
		options := make([]string, 0, len(question.Options))
		seen := make(map[string]struct{}, len(question.Options))
		for _, option := range question.Options {
			option = strings.TrimSpace(option)
			if option == "" {
				return nil, false
			}
			if _, duplicate := seen[option]; duplicate {
				return nil, false
			}
			seen[option] = struct{}{}
			options = append(options, option)
			if len(options) > 8 {
				return nil, false
			}
		}
		normalized = append(normalized, contracts.Question{Prompt: prompt, Options: options})
	}
	return normalized, true
}

func (s *Service) contactInteractionEnabled(event contracts.NormalizedTelegramEvent) bool {
	transport := event.Transport
	if transport == "" {
		transport = s.repository.ChatTransport(event.TenantID, event.ChatID)
	}
	if transport != contracts.TransportBot {
		return false
	}
	role, ok := s.resolveRole(event.TenantID, event.ChatID)
	return ok && contracts.ToolEnabled(role.ToolSettings, "interaction.ask_questions")
}

func qformQuestionKeyboard(formID int64, questionIdx int, question contracts.Question) telegram.InlineKeyboardMarkup {
	rows := make([][]telegram.InlineKeyboardButton, 0, len(question.Options)+1)
	for index, option := range question.Options {
		if index > 7 {
			break
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         truncateButtonLabel(option, 40),
			CallbackData: contracts.CallbackDataEncodeQForm(formID, questionIdx, index),
		}})
	}
	rows = append(rows, []telegram.InlineKeyboardButton{{
		Text:         "✏️",
		CallbackData: contracts.CallbackDataEncodeQFormFreeText(formID, questionIdx),
	}})
	return telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func truncateButtonLabel(label string, limit int) string {
	runes := []rune(label)
	if len(runes) <= limit {
		return label
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

func (s *Service) startInteractionForm(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, proposal contracts.AIProcessMessageResponse, questions []contracts.Question, delivery deliverySettings) (store.InteractionFormRecord, error) {
	form, err := s.repository.CreateInteractionForm(store.InteractionFormRecord{
		TenantID:        event.TenantID,
		ChatID:          event.ChatID,
		BotID:           event.BotID,
		Questions:       questions,
		SourceMessageID: event.SourceMessageID,
		CreatedAt:       s.now(),
		ExpiresAt:       s.now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		return store.InteractionFormRecord{}, err
	}
	if err := s.sendInteractionProse(ctx, event, ownerID, proposal, questions[0], delivery); err != nil {
		s.repository.CancelForm(form.ID)
		return store.InteractionFormRecord{}, err
	}
	if sentMsgID, err := s.sendInteractionQuestion(ctx, event, ownerID, form, delivery); err != nil {
		s.repository.CancelForm(form.ID)
		return store.InteractionFormRecord{}, err
	} else {
		question, _ := form.CurrentQuestion()
		s.saveOutgoingMessage(event, ownerID, sentMsgID, question.Prompt, fmt.Sprintf("%s:form:%d:q:%d", event.EventID, form.ID, form.CurrentIndex), contracts.TextFormatPlain)
	}
	return form, nil
}

func (s *Service) sendInteractionProse(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, proposal contracts.AIProcessMessageResponse, firstQuestion contracts.Question, delivery deliverySettings) error {
	for index, message := range proposal.Messages {
		text := interactionIntroText(message.Text, firstQuestion.Prompt)
		if text == "" {
			continue
		}
		decisionID := fmt.Sprintf("%s:message:%d", event.EventID, index)
		sentMsgID, err := s.sendRecipientWithMarkup(ctx, event, decisionID, text, message.TextFormat, false, delivery, nil)
		if err != nil {
			return err
		}
		s.saveOutgoingMessage(event, ownerID, sentMsgID, text, decisionID, message.TextFormat)
	}
	text := interactionIntroText(proposal.ResponseText, firstQuestion.Prompt)
	if text == "" {
		return nil
	}
	decisionID := event.EventID + ":intro"
	sentMsgID, err := s.sendRecipientWithMarkup(ctx, event, decisionID, text, proposal.TextFormat, false, delivery, nil)
	if err != nil {
		return err
	}
	s.saveOutgoingMessage(event, ownerID, sentMsgID, text, decisionID, proposal.TextFormat)
	return nil
}

func (s *Service) sendInteractionQuestion(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, form store.InteractionFormRecord, delivery deliverySettings) (string, error) {
	question, ok := form.CurrentQuestion()
	if !ok {
		return "", fmt.Errorf("interaction form has no current question")
	}
	decisionID := fmt.Sprintf("%s:form:%d:q:%d", event.EventID, form.ID, form.CurrentIndex)
	return s.sendRecipientWithMarkup(ctx, event, decisionID, question.Prompt, contracts.TextFormatPlain, false, delivery, qformQuestionKeyboard(form.ID, form.CurrentIndex, question))
}

func (s *Service) handleInteractionFormTextAnswer(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, form store.InteractionFormRecord) (contracts.NormalizedTelegramEvent, *InboundResult, error) {
	answer := strings.TrimSpace(event.MessageText)
	if answer == "" {
		result := InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "form_empty_answer"}
		return event, &result, nil
	}
	updated, ok := s.repository.RecordFormAnswer(form.ID, answer)
	if !ok {
		result := InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "form_not_active"}
		return event, &result, nil
	}
	if updated.QuestionsRemaining() {
		if sentMsgID, err := s.sendInteractionQuestion(ctx, event, ownerID, updated, s.deliverySettingsFor(event)); err != nil {
			return event, nil, err
		} else if question, ok := updated.CurrentQuestion(); ok {
			s.saveOutgoingMessage(event, ownerID, sentMsgID, question.Prompt, fmt.Sprintf("%s:form:%d:q:%d", event.EventID, updated.ID, updated.CurrentIndex), contracts.TextFormatPlain)
		}
		result := InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "form_question_sent"}
		return event, &result, nil
	}
	s.repository.CompleteForm(updated.ID)
	event.MessageText = interactionAnswersText(updated)
	return event, nil, nil
}

func sameInteractionText(left, right string) bool {
	return canonicalInteractionText(left) == canonicalInteractionText(right)
}

func canonicalInteractionText(text string) string {
	fields := strings.Fields(strings.ToLower(text))
	for i, field := range fields {
		fields[i] = strings.TrimFunc(field, func(r rune) bool {
			return r == '?' || r == '!' || r == '.' || r == ',' || r == ':' || r == ';' ||
				r == '؟' || r == '？' || r == '！' || r == '。'
		})
	}
	return strings.Join(fields, " ")
}

func interactionIntroText(text, firstQuestion string) string {
	text = strings.TrimSpace(text)
	if text == "" || sameInteractionText(text, firstQuestion) {
		return ""
	}
	stripped := stripTrailingQuestionSentences(text)
	if stripped == "" || sameInteractionText(stripped, firstQuestion) {
		return ""
	}
	return stripped
}

func stripTrailingQuestionSentences(text string) string {
	current := strings.TrimSpace(text)
	for endsWithQuestionMark(current) {
		start := lastSentenceStart(current)
		if start <= 0 {
			return ""
		}
		current = strings.TrimSpace(current[:start])
	}
	return current
}

func endsWithQuestionMark(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	runes := []rune(trimmed)
	switch runes[len(runes)-1] {
	case '?', '؟', '？':
		return true
	default:
		return false
	}
}

func lastSentenceStart(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	finalQuestion := strings.LastIndexAny(trimmed, "?؟？")
	if finalQuestion < 0 {
		return len(trimmed)
	}
	start := 0
	for idx, r := range trimmed[:finalQuestion] {
		switch r {
		case '.', '!', '?', '؟', '？', '！', '。', '\n', '\r':
			start = idx + len(string(r))
		}
	}
	return start
}

func interactionAnswersText(form store.InteractionFormRecord) string {
	payload := struct {
		InteractionAnswers []store.FormAnswer `json:"interaction_answers"`
	}{InteractionAnswers: form.Answers}
	data, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// handleChoiceOption processes an ApprovalChooseOption callback: the owner has selected one of the
// AI-suggested options on a question card. The selection is fed back to the AI as a hint to
// generate a full reply, then that reply is auto-sent or presented as a regular draft.
func (s *Service) handleChoiceOption(ctx context.Context, event contracts.NormalizedTelegramEvent, tenantID string, cbq telegram.CallbackQueryUpdate, optionIdx int, draftID, idempotencyKey string) InboundResult {
	if s.repository.MarkCallback(idempotencyKey) {
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_choice_callback", Duplicate: true}
	}
	draft, ok := s.repository.Draft(draftID)
	if !ok || draft.DraftType != "question" {
		s.logger.Warn("choice option: draft not found or not a question card", "draft_id", draftID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "choice_draft_not_found"}
	}
	if optionIdx >= len(draft.SuggestedOptions) {
		s.logger.Warn("choice option: index out of range", "draft_id", draftID, "option_idx", optionIdx, "options_count", len(draft.SuggestedOptions))
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "choice_option_out_of_range"}
	}
	optionText := draft.SuggestedOptions[optionIdx]
	s.logger.Info("choice option selected", "tenant_id", draft.TenantID, "chat_id", draft.ChatID, "draft_id", draftID, "option", optionText)

	// Remove the question card from the owner's bot chat.
	s.removeDraftCard(ctx, draft)

	// Convert to a regular reply draft and ask the AI to elaborate on the chosen option.
	draft.DraftType = "reply"
	draft.SuggestedOptions = nil
	hint := "Пользователь выбрал: " + optionText + ". Составь полноценный ответ на вопрос собеседника с учётом этого выбора."
	newDraft, err := s.regenerateDraftWithHint(ctx, draft, hint)
	if err != nil {
		s.logger.Error("choice option: ai hint failed, sending option text directly", "error", err, "draft_id", draftID)
		srcEvent := s.resolveSourceEvent(draft)
		_, _ = s.sendRecipient(ctx, srcEvent, draft.DecisionID+":choice_fallback", optionText, contracts.TextFormatPlain, false, s.deliverySettingsFor(srcEvent))
		return InboundResult{Event: event, Decision: contracts.DecisionAutoReply, Reason: "choice_fallback_sent", DraftID: draftID}
	}

	settings := s.repository.ChatSettings(newDraft.TenantID, newDraft.ChatID)
	ownerID := ""
	if conn, connOK := s.repository.BusinessConnection(draft.BusinessConnectionID); connOK {
		ownerID = conn.UserID
	}
	srcEvent := s.resolveSourceEvent(newDraft)
	if settings.Mode == contracts.ChatModeAuto {
		_, _ = s.sendRecipient(ctx, srcEvent, newDraft.DecisionID, newDraft.ResponseText, newDraft.TextFormat, true, s.deliverySettingsFor(srcEvent))
		s.saveOutgoingMessage(srcEvent, ownerID, "", newDraft.ResponseText, newDraft.DecisionID, newDraft.TextFormat)
		if s.notify != nil {
			s.notify(newDraft.TenantID, "new_message", map[string]any{"chat_id": newDraft.ChatID})
		}
		return InboundResult{Event: event, Decision: contracts.DecisionAutoReply, Reason: "choice_auto_sent", DraftID: newDraft.DraftID}
	}
	s.sendDraftToOwner(ctx, srcEvent, newDraft, ownerID)
	if s.notify != nil {
		s.notify(newDraft.TenantID, "new_draft", map[string]any{
			"chat_id":       newDraft.ChatID,
			"draft_id":      newDraft.DraftID,
			"response_text": newDraft.ResponseText,
			"source_text":   newDraft.SourceMessageText,
			"created_at":    newDraft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return InboundResult{Event: event, Decision: contracts.DecisionDraft, Reason: "choice_draft_sent", DraftID: newDraft.DraftID}
}

// parseOptions splits a multi-line string into trimmed, non-empty option lines.
func parseOptions(text string) []string {
	lines := strings.Split(text, "\n")
	options := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			options = append(options, line)
		}
	}
	return options
}
