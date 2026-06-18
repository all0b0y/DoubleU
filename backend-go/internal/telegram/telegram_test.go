package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeUpdateBusinessMessage(t *testing.T) {
	payload := []byte(`{
		"update_id": 123,
		"business_message": {
			"message_id": 55,
			"business_connection_id": "bc-1",
			"chat": {"id": 42, "type": "private", "username": "customer_a"},
			"from": {"id": 77},
			"text": "hello",
			"date": 1700000000
		}
	}`)

	event, err := NormalizeUpdate(payload, "tenant-1", time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if event.EventID != "123:bc-1:55" {
		t.Fatalf("event id = %s", event.EventID)
	}
	if event.TenantID != "tenant-1" || event.ChatID != "42" || event.SenderID != "77" {
		t.Fatalf("event = %+v", event)
	}
	if event.ChatDisplayName != "@customer_a" || event.ChatType != "private" {
		t.Fatalf("chat metadata = name:%q type:%q", event.ChatDisplayName, event.ChatType)
	}
	if event.ReceiveIdempotencyKey() != "123:bc-1" {
		t.Fatalf("receive key = %s", event.ReceiveIdempotencyKey())
	}
}

func TestNormalizeUpdateRequiresBusinessConnection(t *testing.T) {
	payload := []byte(`{"update_id": 123, "message": {"message_id": 55, "chat": {"id": 42}, "from": {"id": 77}, "text": "hello"}}`)
	if _, err := NormalizeUpdate(payload, "", time.Now()); err == nil {
		t.Fatalf("expected missing business connection error")
	}
}

func TestParseBotCommandUpdate(t *testing.T) {
	payload := []byte(`{"update_id": 124, "message": {"message_id": 56, "chat": {"id": 42}, "from": {"id": 77}, "text": "/start bizChat7001"}}`)

	command, ok, err := ParseBotCommandUpdate(payload)
	if err != nil {
		t.Fatalf("parse command: %v", err)
	}
	if !ok {
		t.Fatalf("expected command update")
	}
	if command.Command != "/start" || command.Arguments != "bizChat7001" {
		t.Fatalf("command = %+v", command)
	}
	if command.ReceiveIdempotencyKey != "124:bot_command:56" {
		t.Fatalf("key = %s", command.ReceiveIdempotencyKey)
	}
}

func TestParseBusinessConnectionUpdate(t *testing.T) {
	payload := []byte(`{
		"update_id": 125,
		"business_connection": {
			"id": "bc-1",
			"user": {"id": 77},
			"user_chat_id": 88,
			"date": 1700000000,
			"can_reply": true,
			"is_enabled": true
		}
	}`)

	connection, ok, err := ParseBusinessConnectionUpdate(payload, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("parse connection: %v", err)
	}
	if !ok {
		t.Fatalf("expected business connection")
	}
	if connection.ID != "bc-1" || connection.UserID != "77" || connection.UserChatID != "88" || !connection.CanReply {
		t.Fatalf("connection = %+v", connection)
	}
}

func TestBotSenderConfiguresWebhookCommandsAndMessages(t *testing.T) {
	requests := make([]string, 0)
	bodies := make([]string, 0)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(request.Body)
		requests = append(requests, request.URL.Path)
		bodies = append(bodies, string(data))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		}, nil
	})}
	sender := NewBotSender("token", client)

	if err := sender.SetWebhook(context.Background(), "https://example.com/telegram/webhook", "secret"); err != nil {
		t.Fatalf("set webhook: %v", err)
	}
	if err := sender.SetCommands(context.Background(), []BotCommand{{Command: "start", Description: "setup"}}); err != nil {
		t.Fatalf("set commands: %v", err)
	}
	if err := sender.SendMessage(context.Background(), OutboundMessage{
		ChatID:               "42",
		BusinessConnectionID: "bc-1",
		Text:                 "hello",
		ReplyMarkup: ReplyKeyboardMarkup{
			ResizeKeyboard: true,
			Keyboard:       [][]KeyboardButton{{{Text: "/chats"}}},
		},
	}); err != nil {
		t.Fatalf("send message: %v", err)
	}
	joinedRequests := strings.Join(requests, "\n")
	if !strings.Contains(joinedRequests, "/setWebhook") || !strings.Contains(joinedRequests, "/setMyCommands") || !strings.Contains(joinedRequests, "/sendMessage") {
		t.Fatalf("requests = %v", requests)
	}
	joinedBodies := strings.Join(bodies, "\n")
	if !strings.Contains(joinedBodies, "business_connection") || !strings.Contains(joinedBodies, "business_connection_id") || !strings.Contains(joinedBodies, "reply_markup") {
		t.Fatalf("bodies = %s", joinedBodies)
	}
}

func TestBotSenderSendRichMessageBuildsBody(t *testing.T) {
	var requestPath string
	var body map[string]any
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		requestPath = request.URL.Path
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`)),
		}, nil
	})}
	sender := NewBotSender("token", client)

	msgID, err := sender.SendRichMessage(context.Background(), OutboundMessage{
		ChatID:               "42",
		BusinessConnectionID: "bc-1",
		ReplyToMessageID:     "7",
		Text:                 "# Heading\n\n| A | B |\n|---|---|\n| 1 | 2 |",
		ReplyMarkup: InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Open", CallbackData: "open"}},
		}},
	})
	if err != nil {
		t.Fatalf("send rich: %v", err)
	}
	if msgID != "99" {
		t.Fatalf("message id = %q", msgID)
	}
	if !strings.Contains(requestPath, "/sendRichMessage") {
		t.Fatalf("request path = %s", requestPath)
	}
	if body["chat_id"] != "42" || body["business_connection_id"] != "bc-1" {
		t.Fatalf("body = %#v", body)
	}
	rich, ok := body["rich_message"].(map[string]any)
	if !ok || rich["markdown"] != "# Heading\n\n| A | B |\n|---|---|\n| 1 | 2 |" {
		t.Fatalf("rich_message = %#v", body["rich_message"])
	}
	reply, ok := body["reply_parameters"].(map[string]any)
	if !ok || reply["message_id"] != float64(7) {
		t.Fatalf("reply_parameters = %#v", body["reply_parameters"])
	}
	if _, ok := body["reply_markup"].(map[string]any); !ok {
		t.Fatalf("reply_markup missing: %#v", body)
	}
}

func TestBotSenderReportsTelegramStatusError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false}`)),
		}, nil
	})}
	sender := NewBotSender("token", client)
	if err := sender.SendMessage(context.Background(), OutboundMessage{ChatID: "42", Text: "hello"}); err == nil {
		t.Fatalf("expected status error")
	}
}

func TestBotSenderSendRichMessageReportsTelegramStatusError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false}`)),
		}, nil
	})}
	sender := NewBotSender("token", client)

	if _, err := sender.SendRichMessage(context.Background(), OutboundMessage{ChatID: "42", Text: "hello"}); err == nil {
		t.Fatalf("expected status error")
	}
}

func TestBotSenderSendRichMessageDraftBuildsBody(t *testing.T) {
	var requestPath string
	var body map[string]any
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(request.Body)
		requestPath = request.URL.Path
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`)),
		}, nil
	})}
	sender := NewBotSender("token", client)

	err := sender.SendRichMessageDraft(context.Background(), "42", 12345, "<tg-thinking>Thinking…</tg-thinking>")
	if err != nil {
		t.Fatalf("send rich draft: %v", err)
	}
	if !strings.Contains(requestPath, "/sendRichMessageDraft") {
		t.Fatalf("request path = %s", requestPath)
	}
	if body["chat_id"] != float64(42) || body["draft_id"] != float64(12345) {
		t.Fatalf("body = %#v", body)
	}
	rich, ok := body["rich_message"].(map[string]any)
	if !ok || rich["markdown"] != "<tg-thinking>Thinking…</tg-thinking>" {
		t.Fatalf("rich_message = %#v", body["rich_message"])
	}
	if _, ok := body["business_connection_id"]; ok {
		t.Fatalf("draft body must not include business_connection_id: %#v", body)
	}
}

// TestBotSenderMarkdownFallback verifies that a Markdown send rejected by
// Telegram (invalid entities in LLM output) is retried once as plain text so
// the reply is never lost to a formatting error.
func TestBotSenderMarkdownFallback(t *testing.T) {
	var bodies []string
	attempt := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(request.Body)
		bodies = append(bodies, string(data))
		attempt++
		if attempt == 1 {
			// First (Markdown) attempt fails as Telegram would on bad entities.
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"can't parse entities"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":7}}`)),
		}, nil
	})}
	sender := NewBotSender("token", client)

	if err := sender.SendMessage(context.Background(), OutboundMessage{
		ChatID:    "42",
		Text:      "*bold* _broken",
		ParseMode: "Markdown",
	}); err != nil {
		t.Fatalf("send with fallback: %v", err)
	}
	if attempt != 2 {
		t.Fatalf("expected 2 attempts (markdown then plain), got %d", attempt)
	}
	if !strings.Contains(bodies[0], `"parse_mode":"Markdown"`) {
		t.Fatalf("first attempt should carry parse_mode, body = %s", bodies[0])
	}
	if strings.Contains(bodies[1], "parse_mode") {
		t.Fatalf("fallback attempt must drop parse_mode, body = %s", bodies[1])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
