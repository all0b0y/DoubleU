// Package telegram normalizes Telegram webhook payloads and sends outbound messages.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"doubleu/backend-go/internal/contracts"
)

// OutboundMessage is a recipient-facing Telegram message.
type OutboundMessage struct {
	TenantID             string
	ChatID               string
	BusinessConnectionID string
	SourceMessageID      string
	ReplyToMessageID     string // when set, sends as a Telegram reply to this message_id
	DecisionID           string
	Text                 string
	// ParseMode enables Telegram text formatting ("Markdown", "MarkdownV2",
	// "HTML"). LLM output is not guaranteed to be valid markup, so senders fall
	// back to a plain-text resend when Telegram rejects the entities.
	ParseMode   string
	ReplyMarkup ReplyMarkup
}

// Sender sends recipient-facing messages and answers inline button callbacks.
type Sender interface {
	SendMessage(ctx context.Context, message OutboundMessage) error
	AnswerCallbackQuery(ctx context.Context, queryID string) error
	EditMessageReplyMarkup(ctx context.Context, chatID, messageID string, markup InlineKeyboardMarkup) error
	EditMessageText(ctx context.Context, chatID, messageID, text string, markup InlineKeyboardMarkup) error
	DeleteMessage(ctx context.Context, chatID, messageID string) error
}

// ReplyMarkup marks supported Telegram reply markup payloads.
type ReplyMarkup interface {
	isReplyMarkup()
}

// KeyboardButton describes a Telegram custom keyboard button.
type KeyboardButton struct {
	Text string `json:"text"`
}

// ReplyKeyboardMarkup describes a Telegram custom reply keyboard.
type ReplyKeyboardMarkup struct {
	Keyboard        [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool               `json:"resize_keyboard,omitempty"`
	OneTimeKeyboard bool               `json:"one_time_keyboard,omitempty"`
}

// isReplyMarkup marks ReplyKeyboardMarkup as a sendMessage reply markup.
func (ReplyKeyboardMarkup) isReplyMarkup() {}

// InlineKeyboardButton describes a single inline keyboard button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// InlineKeyboardMarkup describes a Telegram inline keyboard attached to a message.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// isReplyMarkup marks InlineKeyboardMarkup as a sendMessage reply markup.
func (InlineKeyboardMarkup) isReplyMarkup() {}

// NoopSender accepts sends without external side effects.
type NoopSender struct{}

// SendMessage records no external side effects and returns success.
func (NoopSender) SendMessage(ctx context.Context, message OutboundMessage) error {
	return nil
}

// AnswerCallbackQuery records no external side effects and returns success.
func (NoopSender) AnswerCallbackQuery(ctx context.Context, queryID string) error {
	return nil
}

// EditMessageReplyMarkup records no external side effects and returns success.
func (NoopSender) EditMessageReplyMarkup(ctx context.Context, chatID, messageID string, markup InlineKeyboardMarkup) error {
	return nil
}

// EditMessageText records no external side effects and returns success.
func (NoopSender) EditMessageText(ctx context.Context, chatID, messageID, text string, markup InlineKeyboardMarkup) error {
	return nil
}

// DeleteMessage records no external side effects and returns success.
func (NoopSender) DeleteMessage(ctx context.Context, chatID, messageID string) error {
	return nil
}

// BotSender sends messages through the Telegram Bot API.
type BotSender struct {
	token  string
	client *http.Client
}

// BotCommand describes one Telegram command menu item.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// NewBotSender creates a Telegram Bot API sender.
func NewBotSender(token string, client *http.Client) *BotSender {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &BotSender{token: token, client: client}
}

// sendMessageBody builds the sendMessage JSON body from an OutboundMessage.
// Extracted so both SendMessage and SendMessageGetID share the same body construction.
func (s *BotSender) sendMessageBody(message OutboundMessage) map[string]any {
	body := map[string]any{
		"chat_id": message.ChatID,
		"text":    message.Text,
	}
	if message.ParseMode != "" {
		body["parse_mode"] = message.ParseMode
	}
	if message.BusinessConnectionID != "" {
		body["business_connection_id"] = message.BusinessConnectionID
	}
	if message.ReplyMarkup != nil {
		body["reply_markup"] = message.ReplyMarkup
	}
	if replyID, ok := parseMessageID(message.ReplyToMessageID); ok {
		body["reply_parameters"] = map[string]any{"message_id": replyID}
	}
	return body
}

// sendWithParseModeFallback posts sendMessage and, when Telegram rejects the
// request for a message that asked for formatting (typically "can't parse
// entities" — LLM output is not guaranteed valid markup), retries once as
// plain text so the reply is never lost to a formatting error.
func (s *BotSender) sendWithParseModeFallback(ctx context.Context, message OutboundMessage) ([]byte, error) {
	raw, err := s.sendMessageRaw(ctx, s.sendMessageBody(message))
	if err != nil && message.ParseMode != "" {
		plain := message
		plain.ParseMode = ""
		return s.sendMessageRaw(ctx, s.sendMessageBody(plain))
	}
	return raw, err
}

// sendMessageRaw posts a sendMessage request and returns the raw response body.
// It is the shared transport layer for SendMessage and SendMessageGetID.
func (s *BotSender) sendMessageRaw(ctx context.Context, body map[string]any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := "https://api.telegram.org/bot" + s.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("telegram sendMessage status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// SendMessage sends a message to Telegram.
func (s *BotSender) SendMessage(ctx context.Context, message OutboundMessage) error {
	if s.token == "" {
		return fmt.Errorf("telegram bot token is empty")
	}
	_, err := s.sendWithParseModeFallback(ctx, message)
	return err
}

// AnswerCallbackQuery dismisses the loading spinner on an inline button click.
func (s *BotSender) AnswerCallbackQuery(ctx context.Context, queryID string) error {
	return s.post(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": queryID})
}

// EditMessageReplyMarkup replaces the inline keyboard on a sent message.
func (s *BotSender) EditMessageReplyMarkup(ctx context.Context, chatID, messageID string, markup InlineKeyboardMarkup) error {
	return s.post(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": markup,
	})
}

// EditMessageText replaces both the text and inline keyboard of a sent message.
func (s *BotSender) EditMessageText(ctx context.Context, chatID, messageID, text string, markup InlineKeyboardMarkup) error {
	return s.post(ctx, "editMessageText", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"text":         text,
		"reply_markup": markup,
	})
}

// DeleteMessage removes a message from a chat.
func (s *BotSender) DeleteMessage(ctx context.Context, chatID, messageID string) error {
	return s.post(ctx, "deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
}

// SendMessageGetID sends a message and returns the Telegram message_id of the sent message.
func (s *BotSender) SendMessageGetID(ctx context.Context, message OutboundMessage) (string, error) {
	if s.token == "" {
		return "", fmt.Errorf("telegram bot token is empty")
	}
	raw, err := s.sendWithParseModeFallback(ctx, message)
	if err != nil {
		return "", err
	}
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.OK {
		return "", nil // sent OK but couldn't parse ID — not critical
	}
	return strconv.FormatInt(result.Result.MessageID, 10), nil
}

// SetWebhook configures Telegram to send Business and command updates to the backend.
func (s *BotSender) SetWebhook(ctx context.Context, publicURL string, secret string) error {
	body := map[string]any{
		"url": publicURL,
		"allowed_updates": []string{
			"message",
			"callback_query",
			"business_connection",
			"business_message",
			"edited_business_message",
			"deleted_business_messages",
		},
	}
	if secret != "" {
		body["secret_token"] = secret
	}
	return s.post(ctx, "setWebhook", body)
}

// SetCommands configures the Telegram command menu shown to users.
func (s *BotSender) SetCommands(ctx context.Context, commands []BotCommand) error {
	return s.post(ctx, "setMyCommands", map[string]any{"commands": commands})
}

func (s *BotSender) post(ctx context.Context, method string, body map[string]any) error {
	if s.token == "" {
		return fmt.Errorf("telegram bot token is empty")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := "https://api.telegram.org/bot" + s.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("telegram %s status %d: %s", method, res.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// CallbackQueryUpdate describes an owner inline-button callback.
type CallbackQueryUpdate struct {
	UpdateID              int64
	QueryID               string
	Data                  string
	SenderID              string
	ChatID                string
	OriginalMsgID         string
	ReceiveIdempotencyKey string
}

// ParseCallbackQueryUpdate returns a callback query update when present.
func ParseCallbackQueryUpdate(payload []byte) (CallbackQueryUpdate, bool, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return CallbackQueryUpdate{}, false, err
	}
	if update.UpdateID == nil || update.CallbackQuery == nil || update.CallbackQuery.ID == "" {
		return CallbackQueryUpdate{}, false, nil
	}
	cq := update.CallbackQuery
	senderID := strconv.FormatInt(cq.From.ID, 10)
	chatID := ""
	msgID := ""
	if cq.Message != nil {
		chatID = strconv.FormatInt(cq.Message.Chat.ID, 10)
		msgID = strconv.FormatInt(cq.Message.MessageID, 10)
	}
	return CallbackQueryUpdate{
		UpdateID:              *update.UpdateID,
		QueryID:               cq.ID,
		Data:                  cq.Data,
		SenderID:              senderID,
		ChatID:                chatID,
		OriginalMsgID:         msgID,
		ReceiveIdempotencyKey: fmt.Sprintf("%d:callback_query:%s", *update.UpdateID, cq.ID),
	}, true, nil
}

// BusinessConnectionUpdate describes a Telegram Business connection update.
type BusinessConnectionUpdate struct {
	UpdateID              int64
	ID                    string
	UserID                string
	UserChatID            string
	CanReply              bool
	IsEnabled             bool
	Date                  time.Time
	ReceiveIdempotencyKey string
}

// BotCommandUpdate describes an ordinary owner command sent to the bot.
type BotCommandUpdate struct {
	UpdateID              int64
	MessageID             string
	ChatID                string
	SenderID              string
	Text                  string
	Command               string
	Arguments             string
	ReceiveIdempotencyKey string
}

// ParseBusinessConnectionUpdate returns a Business connection update when present.
func ParseBusinessConnectionUpdate(payload []byte, now time.Time) (BusinessConnectionUpdate, bool, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return BusinessConnectionUpdate{}, false, err
	}
	if update.UpdateID == nil || update.BusinessConnection.ID == "" {
		return BusinessConnectionUpdate{}, false, nil
	}
	createdAt := now
	if update.BusinessConnection.Date > 0 {
		createdAt = time.Unix(update.BusinessConnection.Date, 0).UTC()
	}
	userID := strconv.FormatInt(update.BusinessConnection.User.ID, 10)
	userChatID := strconv.FormatInt(update.BusinessConnection.UserChatID, 10)
	if userChatID == "0" {
		userChatID = userID
	}
	return BusinessConnectionUpdate{
		UpdateID:              *update.UpdateID,
		ID:                    update.BusinessConnection.ID,
		UserID:                userID,
		UserChatID:            userChatID,
		CanReply:              update.BusinessConnection.CanReply,
		IsEnabled:             update.BusinessConnection.IsEnabled,
		Date:                  createdAt,
		ReceiveIdempotencyKey: fmt.Sprintf("%d:business_connection:%s", *update.UpdateID, update.BusinessConnection.ID),
	}, true, nil
}

// OwnerMessageUpdate describes a plain (non-command) text message from the owner to the bot.
type OwnerMessageUpdate struct {
	UpdateID  int64
	MessageID string
	ChatID    string
	SenderID  string
	Text      string
}

// ParseOwnerMessageUpdate returns an owner text message when it is a plain (non-command) message update.
func ParseOwnerMessageUpdate(payload []byte) (OwnerMessageUpdate, bool, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return OwnerMessageUpdate{}, false, err
	}
	if update.UpdateID == nil || update.Message == nil || update.Message.Text == "" {
		return OwnerMessageUpdate{}, false, nil
	}
	text := strings.TrimSpace(update.Message.Text)
	if strings.HasPrefix(text, "/") {
		return OwnerMessageUpdate{}, false, nil
	}
	messageID := strconv.FormatInt(update.Message.MessageID, 10)
	chatID := strconv.FormatInt(update.Message.Chat.ID, 10)
	senderID := strconv.FormatInt(update.Message.From.ID, 10)
	return OwnerMessageUpdate{
		UpdateID:  *update.UpdateID,
		MessageID: messageID,
		ChatID:    chatID,
		SenderID:  senderID,
		Text:      text,
	}, true, nil
}

// ParseBotCommandUpdate returns an ordinary command update when present.
func ParseBotCommandUpdate(payload []byte) (BotCommandUpdate, bool, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return BotCommandUpdate{}, false, err
	}
	if update.UpdateID == nil || update.Message == nil || update.Message.Text == "" {
		return BotCommandUpdate{}, false, nil
	}
	text := strings.TrimSpace(update.Message.Text)
	if !strings.HasPrefix(text, "/") {
		return BotCommandUpdate{}, false, nil
	}
	command, args := splitCommand(text)
	messageID := strconv.FormatInt(update.Message.MessageID, 10)
	chatID := strconv.FormatInt(update.Message.Chat.ID, 10)
	senderID := strconv.FormatInt(update.Message.From.ID, 10)
	return BotCommandUpdate{
		UpdateID:              *update.UpdateID,
		MessageID:             messageID,
		ChatID:                chatID,
		SenderID:              senderID,
		Text:                  text,
		Command:               command,
		Arguments:             args,
		ReceiveIdempotencyKey: fmt.Sprintf("%d:bot_command:%s", *update.UpdateID, messageID),
	}, true, nil
}

// IsDeletedBusinessMessagesUpdate returns true when the payload is a deleted_business_messages update.
// Such updates contain only a list of deleted message IDs and no message body, so they must be
// silently acknowledged rather than routed through the message-processing pipeline.
func IsDeletedBusinessMessagesUpdate(payload []byte) (bool, error) {
	var update struct {
		UpdateID                *int64           `json:"update_id"`
		DeletedBusinessMessages *json.RawMessage `json:"deleted_business_messages"`
	}
	if err := json.Unmarshal(payload, &update); err != nil {
		return false, err
	}
	return update.UpdateID != nil && update.DeletedBusinessMessages != nil, nil
}

// NormalizeUpdate converts a Telegram webhook payload into the frozen normalized event.
func NormalizeUpdate(payload []byte, tenantHint string, now time.Time) (contracts.NormalizedTelegramEvent, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return contracts.NormalizedTelegramEvent{}, err
	}
	if update.UpdateID == nil {
		return contracts.NormalizedTelegramEvent{}, fmt.Errorf("missing update_id")
	}
	message := update.BusinessMessage
	if message == nil {
		message = update.Message
	}
	if message == nil {
		return contracts.NormalizedTelegramEvent{}, fmt.Errorf("missing message")
	}

	businessConnectionID := firstNonEmpty(update.BusinessConnection.ID, message.BusinessConnectionID)
	if businessConnectionID == "" {
		return contracts.NormalizedTelegramEvent{}, fmt.Errorf("missing business_connection_id")
	}
	tenantID := tenantHint
	if tenantID == "" {
		tenantID = "tenant:" + businessConnectionID
	}
	createdAt := now
	if message.Date > 0 {
		createdAt = time.Unix(message.Date, 0).UTC()
	}
	text := firstNonEmpty(message.Text, message.Caption)
	messageType := detectMessageType(message)
	sourceMessageID := strconv.FormatInt(message.MessageID, 10)
	chatID := strconv.FormatInt(message.Chat.ID, 10)
	senderID := strconv.FormatInt(message.From.ID, 10)
	chatDisplayName := message.Chat.DisplayName()
	if chatDisplayName == "" {
		chatDisplayName = message.From.DisplayName()
	}
	eventID := fmt.Sprintf("%d:%s:%s", *update.UpdateID, businessConnectionID, sourceMessageID)
	return contracts.NormalizedTelegramEvent{
		EventID:              eventID,
		TenantID:             tenantID,
		ChatID:               chatID,
		ChatDisplayName:      chatDisplayName,
		ChatType:             message.Chat.Type,
		TelegramUpdateID:     *update.UpdateID,
		BusinessConnectionID: businessConnectionID,
		SourceMessageID:      sourceMessageID,
		SenderID:             senderID,
		MessageText:          text,
		MessageType:          messageType,
		CreatedAt:            createdAt,
	}, nil
}

// NormalizeBotUpdate converts a getUpdates payload received via a user-supplied
// bot into the frozen normalized event. Unlike NormalizeUpdate it does not
// require business_connection_id — the tenant is already known from the bot
// registration. Returns ok=false for updates without a message body (the
// poller requests only "message" updates, but Telegram may still deliver
// service payloads after an allowed_updates change).
func NormalizeBotUpdate(payload []byte, botID int64, tenantID string, now time.Time) (contracts.NormalizedTelegramEvent, bool, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return contracts.NormalizedTelegramEvent{}, false, err
	}
	if update.UpdateID == nil {
		return contracts.NormalizedTelegramEvent{}, false, fmt.Errorf("missing update_id")
	}
	message := update.Message
	if message == nil {
		return contracts.NormalizedTelegramEvent{}, false, nil
	}
	createdAt := now
	if message.Date > 0 {
		createdAt = time.Unix(message.Date, 0).UTC()
	}
	sourceMessageID := strconv.FormatInt(message.MessageID, 10)
	chatDisplayName := message.Chat.DisplayName()
	if chatDisplayName == "" {
		chatDisplayName = message.From.DisplayName()
	}
	bot := strconv.FormatInt(botID, 10)
	return contracts.NormalizedTelegramEvent{
		// update_id sequences are per-bot, so both the event id and the receive
		// idempotency key derived from it must be scoped by bot id.
		EventID: fmt.Sprintf("%d:bot:%s:%s", *update.UpdateID, bot, sourceMessageID),
		TenantID: tenantID,
		// Bot-transport chat identity is namespaced by bot id so a tenant's two
		// bots talking to the SAME Telegram user resolve to two distinct chats.
		// The raw Telegram chat id is recoverable via contracts.RawChatID for
		// outbound sends. Personal/business transport keeps the raw id (above).
		ChatID:           contracts.BotChatID(bot, strconv.FormatInt(message.Chat.ID, 10)),
		ChatDisplayName:  chatDisplayName,
		ChatType:         message.Chat.Type,
		TelegramUpdateID: *update.UpdateID,
		SourceMessageID:  sourceMessageID,
		SenderID:         strconv.FormatInt(message.From.ID, 10),
		MessageText:      firstNonEmpty(message.Text, message.Caption),
		MessageType:      detectMessageType(message),
		CreatedAt:        createdAt,
		Transport:        contracts.TransportBot,
		BotID:            bot,
	}, true, nil
}

type telegramUpdate struct {
	UpdateID           *int64               `json:"update_id"`
	BusinessConnection telegramBusinessConn `json:"business_connection"`
	BusinessMessage    *telegramMessage     `json:"business_message"`
	Message            *telegramMessage     `json:"message"`
	CallbackQuery      *telegramCallbackQuery `json:"callback_query"`
}

type telegramCallbackQuery struct {
	ID      string          `json:"id"`
	From    telegramUser    `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string          `json:"data"`
}

type telegramBusinessConn struct {
	ID         string       `json:"id"`
	User       telegramUser `json:"user"`
	UserChatID int64        `json:"user_chat_id"`
	Date       int64        `json:"date"`
	CanReply   bool         `json:"can_reply"`
	IsEnabled  bool         `json:"is_enabled"`
}

type telegramMessage struct {
	MessageID            int64             `json:"message_id"`
	BusinessConnectionID string            `json:"business_connection_id"`
	Chat                 telegramChat      `json:"chat"`
	From                 telegramUser      `json:"from"`
	Text                 string            `json:"text"`
	Caption              string            `json:"caption"`
	Date                 int64             `json:"date"`
	Photo                []json.RawMessage `json:"photo"`
	Video                *json.RawMessage  `json:"video"`
	Voice                *json.RawMessage  `json:"voice"`
	Document             *json.RawMessage  `json:"document"`
	Sticker              *json.RawMessage  `json:"sticker"`
	Audio                *json.RawMessage  `json:"audio"`
	VideoNote            *json.RawMessage  `json:"video_note"`
	Animation            *json.RawMessage  `json:"animation"`
}

type telegramChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// detectMessageType derives a human-readable message type from a Telegram message.
func detectMessageType(m *telegramMessage) string {
	if m.Text != "" {
		return "text"
	}
	switch {
	case len(m.Photo) > 0:
		return "photo"
	case m.Video != nil:
		return "video"
	case m.Voice != nil:
		return "voice"
	case m.Document != nil:
		return "document"
	case m.Sticker != nil:
		return "sticker"
	case m.Audio != nil:
		return "audio"
	case m.VideoNote != nil:
		return "video_note"
	case m.Animation != nil:
		return "animation"
	case m.Caption != "":
		return "caption"
	default:
		return "unknown"
	}
}

// parseMessageID converts a stringified Telegram message_id to int64.
// Returns (0, false) when the string is empty or not a valid integer.
func parseMessageID(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func splitCommand(text string) (string, string) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", ""
	}
	command := parts[0]
	if at := strings.Index(command, "@"); at >= 0 {
		command = command[:at]
	}
	if len(parts) == 1 {
		return command, ""
	}
	return command, strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
}

func (c telegramChat) DisplayName() string {
	if c.Title != "" {
		return c.Title
	}
	if c.Username != "" {
		return "@" + c.Username
	}
	name := strings.TrimSpace(strings.TrimSpace(c.FirstName) + " " + strings.TrimSpace(c.LastName))
	return name
}

func (u telegramUser) DisplayName() string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
}
