package core

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/telegram"
)

func (s *Service) contactInteractionOptions(event contracts.NormalizedTelegramEvent, proposal contracts.AIProcessMessageResponse) ([]string, bool) {
	if proposal.ResponsePlan == nil || proposal.ResponsePlan.Shape != "buttons" || !s.contactInteractionEnabled(event) {
		return nil, false
	}
	if len(proposal.ResponsePlan.Options) < 2 || len(proposal.ResponsePlan.Options) > 4 {
		return nil, false
	}
	options := make([]string, 0, len(proposal.ResponsePlan.Options))
	seen := make(map[string]struct{}, len(proposal.ResponsePlan.Options))
	for _, option := range proposal.ResponsePlan.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			return nil, false
		}
		if _, duplicate := seen[option]; duplicate {
			return nil, false
		}
		seen[option] = struct{}{}
		options = append(options, option)
	}
	return options, true
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
	return ok && contracts.ToolEnabled(role.ToolSettings, "interaction.present_buttons")
}

func contactOptionsKeyboard(draftID string, options []string) telegram.InlineKeyboardMarkup {
	rows := make([][]telegram.InlineKeyboardButton, 0, len(options))
	for index, option := range options {
		if index > 3 {
			break
		}
		label := []rune(option)
		if len(label) > 40 {
			label = append(label[:37], '.', '.', '.')
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         string(label),
			CallbackData: contracts.CallbackDataEncodeAIButton(draftID, index),
		}})
	}
	return telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func interactionDraftID(decisionID string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(decisionID)) // hash.Hash never returns an error.
	return fmt.Sprintf("draft:aibtn-%x", hash.Sum64())
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
