package core

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// sendRecipient sends a reply to the contact and returns the Telegram message_id of the sent message.
// delivery holds the pre-computed rich-formatting and streaming toggles; callers on the hot path
// supply deliverySettingsFromUser (no extra repository read) while others use deliverySettingsFor.
func (s *Service) sendRecipient(ctx context.Context, event contracts.NormalizedTelegramEvent, decisionID, text string, textFormat contracts.TextFormat, allowStreaming bool, delivery deliverySettings) (string, error) {
	return s.sendRecipientWithMarkup(ctx, event, decisionID, text, textFormat, allowStreaming, delivery, nil)
}

func (s *Service) sendRecipientWithMarkup(ctx context.Context, event contracts.NormalizedTelegramEvent, decisionID, text string, textFormat contracts.TextFormat, allowStreaming bool, delivery deliverySettings, replyMarkup telegram.ReplyMarkup) (string, error) {
	key := contracts.SendIdempotencyKey(event.TenantID, event.ChatID, event.SourceMessageID, decisionID)
	duplicate := s.repository.MarkSend(store.SendRecord{
		Key:             key,
		TenantID:        event.TenantID,
		ChatID:          event.ChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      decisionID,
		Text:            text,
		CreatedAt:       s.now(),
	})
	if duplicate {
		return "", nil
	}
	msg := telegram.OutboundMessage{
		TenantID: event.TenantID,
		// Address Telegram with the RAW chat id: bot-transport chat ids are
		// namespaced ("bot:{botID}:{rawID}") for identity, but the wire id must
		// be the bare Telegram chat id. Business transport ids pass through.
		ChatID:               contracts.RawChatID(event.ChatID),
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      event.SourceMessageID,
		ReplyToMessageID:     event.SourceMessageID,
		DecisionID:           decisionID,
		Text:                 text,
		ReplyMarkup:          replyMarkup,
	}
	if event.MessageType == "callback_query" || event.MessageType == "scheduled" {
		msg.ReplyToMessageID = ""
	}
	sender, err := s.recipientSender(event)
	if err != nil {
		return "", err
	}
	if textFormat.Normalize() == contracts.TextFormatRich {
		if !delivery.RichFormattingEnabled {
			// Rich formatting is disabled; degrade to HTML so the LLM-produced
			// markdown never appears raw (### ** ~~) in the chat.
			htmlMsg := msg
			htmlMsg.Text = telegram.MarkdownToHTML(msg.Text)
			htmlMsg.ParseMode = "HTML"
			return sendMessage(ctx, sender, htmlMsg)
		}
		if s.richDraftStreamingAllowedFor(event) {
			s.sendInitialRichThinkingDraft(ctx, sender, msg)
		}
		if allowStreaming && delivery.StreamingEnabled && s.richDraftStreamingAllowedFor(event) {
			return s.sendStreamingRichRecipient(ctx, sender, msg)
		}
		return s.sendRichRecipient(ctx, sender, msg)
	}
	// Plain format: the model is asked to reply without rich markup, but it still
	// occasionally emits stray markdown (**bold**, ### heading) that would leak as
	// raw tokens into the chat. Run it through the HTML converter and switch to
	// parse_mode=HTML ONLY when the conversion actually changed something —
	// otherwise send the text verbatim with no parse_mode, exactly as before.
	// MarkdownToHTML only transforms matched constructs, so an ordinary plain reply
	// (incl. incidental "5 * 5" or snake_case) is returned unchanged and takes the
	// verbatim path.
	if html := telegram.MarkdownToHTML(msg.Text); html != msg.Text {
		htmlMsg := msg
		htmlMsg.Text = html
		htmlMsg.ParseMode = "HTML"
		return sendMessage(ctx, sender, htmlMsg)
	}
	return sendMessage(ctx, sender, msg)
}

type deliverySettings struct {
	RichFormattingEnabled bool
	StreamingEnabled      bool
}

// deliverySettingsFor resolves rich-formatting and streaming toggles from all config layers.
// Callers on the hot path that have already fetched UserSettings should call
// deliverySettingsFromUser instead to avoid a redundant repository read.
func (s *Service) deliverySettingsFor(event contracts.NormalizedTelegramEvent) deliverySettings {
	if us, ok := s.repository.UserSettings(event.TenantID); ok {
		return s.deliverySettingsFromUser(event, &us)
	}
	return s.deliverySettingsFromUser(event, nil)
}

// deliverySettingsFromUser is the core toggle resolver: accepts a pre-loaded user settings
// value so the hot path (processContactMessage) does not re-read the repository.
// Pass nil when UserSettings was not found — rich formatting then defaults to enabled.
func (s *Service) deliverySettingsFromUser(event contracts.NormalizedTelegramEvent, us *contracts.UserSettings) deliverySettings {
	settings := deliverySettings{RichFormattingEnabled: true}
	if us != nil {
		settings.RichFormattingEnabled = us.RichFormattingEnabled
		settings.StreamingEnabled = us.StreamingEnabled
	}
	if role, ok := s.resolveRole(event.TenantID, event.ChatID); ok {
		settings.RichFormattingEnabled = applyScopedBool(settings.RichFormattingEnabled, role.RichFormattingEnabled)
		settings.StreamingEnabled = applyScopedBool(settings.StreamingEnabled, role.StreamingEnabled)
	}
	if bot, ok := s.botForDeliverySettings(event); ok {
		settings.RichFormattingEnabled = applyScopedBool(settings.RichFormattingEnabled, bot.RichFormattingEnabled)
		settings.StreamingEnabled = applyScopedBool(settings.StreamingEnabled, bot.StreamingEnabled)
	}
	return settings
}

const richStreamingThinkingMarkdown = "<tg-thinking>Thinking…</tg-thinking>"

func (s *Service) richDraftStreamingAllowedFor(event contracts.NormalizedTelegramEvent) bool {
	if event.BusinessConnectionID != "" {
		return false
	}
	transport := event.Transport
	if transport == "" {
		transport = s.repository.ChatTransport(event.TenantID, event.ChatID)
	}
	if transport != contracts.TransportBot {
		return false
	}
	chatID := contracts.RawChatID(event.ChatID)
	parsedChatID, err := strconv.ParseInt(chatID, 10, 64)
	return err == nil && parsedChatID > 0
}

func (s *Service) sendInitialRichThinkingDraft(ctx context.Context, sender telegram.Sender, msg telegram.OutboundMessage) {
	draftSender, ok := sender.(interface {
		SendRichMessageDraft(context.Context, string, int64, string) error
	})
	if !ok {
		return
	}
	draftID := richDraftID(msg)
	if err := draftSender.SendRichMessageDraft(ctx, msg.ChatID, draftID, richStreamingThinkingMarkdown); err != nil {
		s.logger.Warn("telegram rich thinking draft send failed; continuing",
			"tenant_id", msg.TenantID,
			"chat_id", msg.ChatID,
			"decision_id", msg.DecisionID,
			"draft_id", draftID,
			"error", err,
		)
	}
}

func (s *Service) sendStreamingRichRecipient(ctx context.Context, sender telegram.Sender, msg telegram.OutboundMessage) (string, error) {
	// Broadcast a native "bot is typing…" indicator immediately — visible on all
	// Telegram clients while the rich preview frames stream in.
	if chatActionSender, ok := sender.(interface {
		SendChatAction(context.Context, string, string) error
	}); ok {
		if err := chatActionSender.SendChatAction(ctx, msg.ChatID, "typing"); err != nil {
			s.logger.Warn("telegram sendChatAction failed; continuing",
				"tenant_id", msg.TenantID,
				"chat_id", msg.ChatID,
				"error", err,
			)
		}
	}
	draftSender, ok := sender.(interface {
		SendRichMessageDraft(context.Context, string, int64, string) error
	})
	if !ok {
		s.logger.Warn("telegram rich draft sender unavailable; skipping draft preview",
			"tenant_id", msg.TenantID,
			"chat_id", msg.ChatID,
			"decision_id", msg.DecisionID,
		)
		return s.sendRichRecipient(ctx, sender, msg)
	}
	s.sendRichDraftPreview(ctx, draftSender, msg)
	return s.sendRichRecipient(ctx, sender, msg)
}

func (s *Service) sendRichDraftPreview(ctx context.Context, sender interface {
	SendRichMessageDraft(context.Context, string, int64, string) error
}, msg telegram.OutboundMessage) {
	draftID := richDraftID(msg)
	for i, markdown := range richDraftFrames(msg.Text) {
		if i > 0 && s.streamFramePause > 0 {
			time.Sleep(s.streamFramePause)
		}
		if err := sender.SendRichMessageDraft(ctx, msg.ChatID, draftID, markdown); err != nil {
			s.logger.Warn("telegram rich draft send failed; continuing with final send",
				"tenant_id", msg.TenantID,
				"chat_id", msg.ChatID,
				"decision_id", msg.DecisionID,
				"draft_id", draftID,
				"error", err,
			)
			return
		}
	}
}

// richDraftFrames returns the ordered sequence of markdown strings to stream as
// ephemeral draft previews before the final sendRichMessage.
//
// Frame 0 is always the thinking indicator (<tg-thinking>). Subsequent frames are
// cumulative paragraph chunks of finalMarkdown so the user sees the reply build up
// progressively. The last frame equals finalMarkdown so the draft matches the final
// message and the animated transition is seamless.
func richDraftFrames(finalMarkdown string) []string {
	frames := []string{richStreamingThinkingMarkdown}
	if finalMarkdown == "" {
		return frames
	}
	for _, chunk := range splitIntoChunks(finalMarkdown) {
		frames = append(frames, chunk)
	}
	return frames
}

// splitIntoChunks splits markdown into progressive prefix chunks for draft streaming.
// Short texts (under 120 runes) are returned as a single chunk. Longer texts are split
// at paragraph breaks (\n\n); each chunk is the accumulated text up to that boundary, so
// the last chunk always equals the full text.
func splitIntoChunks(text string) []string {
	const minTotalRunes = 120
	if len([]rune(text)) < minTotalRunes {
		return []string{text}
	}
	paragraphs := strings.Split(text, "\n\n")
	if len(paragraphs) <= 1 {
		return []string{text}
	}
	chunks := make([]string, 0, len(paragraphs))
	var acc string
	for i, para := range paragraphs {
		if i > 0 {
			acc += "\n\n"
		}
		acc += para
		chunks = append(chunks, acc)
	}
	return chunks
}

func richDraftID(msg telegram.OutboundMessage) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(msg.TenantID + "\x00" + msg.ChatID + "\x00" + msg.SourceMessageID + "\x00" + msg.DecisionID)) // hash.Hash never returns an error.
	id := int64(h.Sum64() & 0x7fffffffffffffff)
	if id == 0 {
		return 1
	}
	return id
}

func (s *Service) sendRichRecipient(ctx context.Context, sender telegram.Sender, msg telegram.OutboundMessage) (string, error) {
	if richSender, ok := sender.(interface {
		SendRichMessage(context.Context, telegram.OutboundMessage) (string, error)
	}); ok {
		msgID, err := richSender.SendRichMessage(ctx, msg)
		if err == nil {
			return msgID, nil
		}
		s.logger.Warn("telegram rich send failed; falling back to plain text",
			"tenant_id", msg.TenantID,
			"chat_id", msg.ChatID,
			"decision_id", msg.DecisionID,
			"error", err,
		)
	} else {
		s.logger.Warn("telegram rich sender unavailable; falling back to HTML",
			"tenant_id", msg.TenantID,
			"chat_id", msg.ChatID,
			"decision_id", msg.DecisionID,
		)
	}
	// Degrade through HTML → stripped plain so raw markdown never leaks.
	htmlMsg := msg
	htmlMsg.Text = telegram.MarkdownToHTML(msg.Text)
	htmlMsg.ParseMode = "HTML"
	if msgID, err := sendMessage(ctx, sender, htmlMsg); err == nil {
		return msgID, nil
	}
	s.logger.Warn("telegram HTML fallback failed; stripping markdown to plain",
		"tenant_id", msg.TenantID,
		"chat_id", msg.ChatID,
		"decision_id", msg.DecisionID,
	)
	stripped := msg
	stripped.Text = telegram.StripMarkdown(msg.Text)
	stripped.ParseMode = ""
	return sendMessage(ctx, sender, stripped)
}

func (s *Service) botForDeliverySettings(event contracts.NormalizedTelegramEvent) (store.UserBotRecord, bool) {
	if event.BotID != "" {
		botID, err := strconv.ParseInt(event.BotID, 10, 64)
		if err == nil {
			return s.repository.UserBotByID(event.TenantID, botID)
		}
	}
	return s.botForChat(event.TenantID, event.ChatID)
}

func applyScopedBool(current bool, override *bool) bool {
	if override == nil {
		return current
	}
	return *override
}

func sendMessage(ctx context.Context, sender telegram.Sender, msg telegram.OutboundMessage) (string, error) {
	if idSender, ok := sender.(interface {
		SendMessageGetID(context.Context, telegram.OutboundMessage) (string, error)
	}); ok {
		return idSender.SendMessageGetID(ctx, msg)
	}
	return "", sender.SendMessage(ctx, msg)
}

// recipientSender picks the outbound channel for a contact-facing reply:
// bot-transport chats answer through the tenant's own bot, everything else
// through the Business connection on the control bot. The reply must leave
// through the same channel the message arrived from.
func (s *Service) recipientSender(event contracts.NormalizedTelegramEvent) (telegram.Sender, error) {
	transport := event.Transport
	if transport == "" && event.BusinessConnectionID == "" {
		// Reconstructed events (approval callbacks build a minimal event from
		// the draft) carry no transport; the chat row recorded it on receipt.
		transport = s.repository.ChatTransport(event.TenantID, event.ChatID)
	}
	if transport != contracts.TransportBot {
		return s.sender, nil
	}
	if s.botSenders == nil {
		return nil, fmt.Errorf("bot transport reply for tenant %s: no bot sender provider configured", event.TenantID)
	}
	// Reply through the exact bot the message arrived on. The bot id is on the
	// event; reconstructed events (approval callbacks) recover it from the
	// namespaced chat id.
	botID := event.BotID
	if botID == "" {
		if id, _, ok := contracts.ParseBotChatID(event.ChatID); ok {
			botID = id
		}
	}
	sender, ok := s.botSenders.SenderFor(event.TenantID, botID)
	if !ok {
		return nil, fmt.Errorf("bot transport reply for tenant %s: no active bot registered", event.TenantID)
	}
	return sender, nil
}
