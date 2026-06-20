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

// InputRichMessage carries Rich Markdown for Telegram Bot API sendRichMessage.
type InputRichMessage struct {
	Markdown string `json:"markdown,omitempty"`
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
	// apiBase is the Telegram API host. Real senders use the public host; tests
	// point it at an httptest server. Both the bot-method endpoints
	// (apiBase/bot<token>/<method>) and the file-download endpoint
	// (apiBase/file/bot<token>/<path>) derive from it.
	apiBase string
}

// telegramAPIHost is the public Telegram API host used by production senders.
const telegramAPIHost = "https://api.telegram.org"

// maxDownloadBytes is the Telegram Bot API getFile download cap. Files above it
// cannot be fetched through a bot token, so DownloadFile rejects them instead of
// buffering unbounded data, letting callers degrade gracefully.
const maxDownloadBytes = 20 * 1024 * 1024

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
	return &BotSender{token: token, client: client, apiBase: telegramAPIHost}
}

// methodURL builds the URL for a Telegram bot API method.
func (s *BotSender) methodURL(method string) string {
	return s.apiBase + "/bot" + s.token + "/" + method
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

// richMessageBody builds the sendRichMessage JSON body using vendored Bot API
// fields for phase 1: chat_id, rich_message.markdown, business_connection_id,
// reply_parameters and inline reply_markup.
func (s *BotSender) richMessageBody(message OutboundMessage) (map[string]any, error) {
	body := map[string]any{
		"chat_id":      message.ChatID,
		"rich_message": InputRichMessage{Markdown: message.Text},
	}
	if message.BusinessConnectionID != "" {
		body["business_connection_id"] = message.BusinessConnectionID
	}
	if replyID, ok := parseMessageID(message.ReplyToMessageID); ok {
		body["reply_parameters"] = map[string]any{"message_id": replyID}
	}
	if message.ReplyMarkup != nil {
		inline, ok := message.ReplyMarkup.(InlineKeyboardMarkup)
		if !ok {
			return nil, fmt.Errorf("sendRichMessage supports only inline reply markup")
		}
		body["reply_markup"] = inline
	}
	return body, nil
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
	url := s.methodURL("sendMessage")
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

// sendRichMessageRaw posts a sendRichMessage request and returns the raw response body.
func (s *BotSender) sendRichMessageRaw(ctx context.Context, body map[string]any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := s.methodURL("sendRichMessage")
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
		return nil, fmt.Errorf("telegram sendRichMessage status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
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

// SendRichMessage sends Telegram Rich Markdown via sendRichMessage and returns the message_id when available.
func (s *BotSender) SendRichMessage(ctx context.Context, message OutboundMessage) (string, error) {
	if s.token == "" {
		return "", fmt.Errorf("telegram bot token is empty")
	}
	body, err := s.richMessageBody(message)
	if err != nil {
		return "", err
	}
	raw, err := s.sendRichMessageRaw(ctx, body)
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

// SendRichMessageDraft streams an ephemeral rich-message preview in a private bot chat.
func (s *BotSender) SendRichMessageDraft(ctx context.Context, chatID string, draftID int64, markdown string) error {
	if s.token == "" {
		return fmt.Errorf("telegram bot token is empty")
	}
	if draftID == 0 {
		return fmt.Errorf("draft_id must be non-zero")
	}
	parsedChatID, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("chat_id must be an integer for sendRichMessageDraft: %w", err)
	}
	return s.post(ctx, "sendRichMessageDraft", map[string]any{
		"chat_id":      parsedChatID,
		"draft_id":     draftID,
		"rich_message": InputRichMessage{Markdown: markdown},
	})
}

// SendChatAction broadcasts a transient typing indicator to the given private chat.
// Telegram shows the indicator for up to 5 seconds; call before starting a long operation
// so the user sees "bot is typing…" instead of silence. Returns immediately.
// action is typically "typing"; other values ("upload_document" etc.) are accepted but
// this codebase only uses "typing".
func (s *BotSender) SendChatAction(ctx context.Context, chatID string, action string) error {
	if s.token == "" {
		return fmt.Errorf("telegram bot token is empty")
	}
	parsedChatID, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("chat_id must be an integer for sendChatAction: %w", err)
	}
	return s.post(ctx, "sendChatAction", map[string]any{
		"chat_id": parsedChatID,
		"action":  action,
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
	url := s.methodURL(method)
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

// FileDownloader is the subset of sender behaviour needed to fetch a media
// attachment's bytes. *BotSender implements it; NoopSender does not, so callers
// type-assert and skip media extraction when the active sender cannot download.
type FileDownloader interface {
	GetFilePath(ctx context.Context, fileID string) (string, error)
	DownloadFile(ctx context.Context, filePath string) ([]byte, string, error)
}

// GetFilePath resolves a Telegram file_id to its relative file_path via getFile.
// The path feeds DownloadFile. The getFile result is short-lived (Telegram may
// expire it), so callers should download promptly. Returns an error for an empty
// token, transport failure, non-2xx status, or a response without a file_path.
func (s *BotSender) GetFilePath(ctx context.Context, fileID string) (string, error) {
	if s.token == "" {
		return "", fmt.Errorf("telegram bot token is empty")
	}
	data, err := json.Marshal(map[string]any{"file_id": fileID})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.methodURL("getFile"), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("telegram getFile status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if !parsed.OK || parsed.Result.FilePath == "" {
		return "", fmt.Errorf("telegram getFile: missing file_path")
	}
	return parsed.Result.FilePath, nil
}

// DownloadFile fetches the bytes of a resolved Telegram file_path. It enforces
// the Bot API download cap (maxDownloadBytes), erroring instead of buffering an
// oversized body so callers degrade gracefully. The returned string is the
// response Content-Type (best-effort; Telegram does not always set a precise one).
func (s *BotSender) DownloadFile(ctx context.Context, filePath string) ([]byte, string, error) {
	if s.token == "" {
		return nil, "", fmt.Errorf("telegram bot token is empty")
	}
	url := s.apiBase + "/file/bot" + s.token + "/" + filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, "", fmt.Errorf("telegram download status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	// Read one byte past the cap so a file exactly at the limit still succeeds
	// while anything larger is detected and rejected.
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxDownloadBytes {
		return nil, "", fmt.Errorf("telegram file exceeds %d byte download cap", maxDownloadBytes)
	}
	return raw, res.Header.Get("Content-Type"), nil
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
		EventID:  fmt.Sprintf("%d:bot:%s:%s", *update.UpdateID, bot, sourceMessageID),
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
	UpdateID           *int64                 `json:"update_id"`
	BusinessConnection telegramBusinessConn   `json:"business_connection"`
	BusinessMessage    *telegramMessage       `json:"business_message"`
	Message            *telegramMessage       `json:"message"`
	CallbackQuery      *telegramCallbackQuery `json:"callback_query"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
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

// MediaDescriptor identifies a downloadable Telegram attachment plus the minimum
// metadata needed to fetch and route it for text extraction. Kind is one of
// "voice", "audio", "photo", "document" — the kinds the extract-media path
// understands. Video, stickers and animations are deliberately excluded.
type MediaDescriptor struct {
	Kind     string
	FileID   string
	MimeType string
	FileSize int64
	Duration int64 // seconds; voice/audio only, 0 otherwise
}

// telegramFile holds the Telegram file fields shared by voice, audio, document
// and photo-size objects — only the subset needed for download and routing.
type telegramFile struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	MimeType string `json:"mime_type"`
	Duration int64  `json:"duration"`
}

// extractMedia returns the downloadable attachment of a message, or nil when it
// carries no extractable media (plain text, sticker, video, animation, …).
// Photos arrive as an ascending-size array, so the largest (last) size is chosen
// for best extraction quality. Caption text is handled separately by the caller.
func extractMedia(m *telegramMessage) *MediaDescriptor {
	switch {
	case len(m.Photo) > 0:
		return fileDescriptor("photo", &m.Photo[len(m.Photo)-1])
	case m.Voice != nil:
		return fileDescriptor("voice", m.Voice)
	case m.Audio != nil:
		return fileDescriptor("audio", m.Audio)
	case m.Document != nil:
		return fileDescriptor("document", m.Document)
	default:
		return nil
	}
}

// fileDescriptor decodes a Telegram file sub-object into a MediaDescriptor,
// returning nil when the payload is malformed or carries no file_id (nothing to
// download).
func fileDescriptor(kind string, raw *json.RawMessage) *MediaDescriptor {
	var f telegramFile
	if err := json.Unmarshal(*raw, &f); err != nil || f.FileID == "" {
		return nil
	}
	return &MediaDescriptor{
		Kind:     kind,
		FileID:   f.FileID,
		MimeType: f.MimeType,
		FileSize: f.FileSize,
		Duration: f.Duration,
	}
}

// MediaFromBotUpdate parses a getUpdates payload and returns its downloadable
// media attachment, if any. Returns (nil, nil) when the update has no message or
// no extractable media. The bot poller calls this so the core service can turn
// media into text before AI processing without re-threading the raw payload.
func MediaFromBotUpdate(payload []byte) (*MediaDescriptor, error) {
	var update telegramUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		return nil, err
	}
	if update.Message == nil {
		return nil, nil
	}
	return extractMedia(update.Message), nil
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
