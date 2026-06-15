package botservice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// fakeTelegram is a scripted Bot API: each getUpdates call pops the next
// response from the queue; an empty queue long-polls by returning [] after a
// short delay so the poller loop keeps spinning without burning CPU.
type fakeTelegram struct {
	mu             sync.Mutex
	updateBatches  [][]json.RawMessage
	statusQueue    []int // non-200 statuses to serve before updates (e.g. 409)
	getUpdateCalls []int64
	webhookDeletes int
}

func (f *fakeTelegram) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 555, "username": "user_assistant_bot"},
			})
		case strings.HasSuffix(r.URL.Path, "/deleteWebhook"):
			f.webhookDeletes++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var body struct {
				Offset int64 `json:"offset"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.getUpdateCalls = append(f.getUpdateCalls, body.Offset)
			if len(f.statusQueue) > 0 {
				status := f.statusQueue[0]
				f.statusQueue = f.statusQueue[1:]
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": status, "description": "scripted"})
				return
			}
			if len(f.updateBatches) == 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
				return
			}
			batch := f.updateBatches[0]
			f.updateBatches = f.updateBatches[1:]
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": batch})
		default:
			t.Errorf("unexpected telegram call: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func rawUpdate(t *testing.T, updateID, chatID, messageID int64, text string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": messageID,
			"chat":       map[string]any{"id": chatID, "type": "private", "first_name": "Контакт"},
			"from":       map[string]any{"id": chatID},
			"text":       text,
			"date":       1700000000,
		},
	})
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}
	return payload
}

// collectingHandler records every event and signals when expected count arrives.
type collectingHandler struct {
	mu     sync.Mutex
	events []contracts.NormalizedTelegramEvent
	done   chan struct{}
	want   int
}

func (c *collectingHandler) HandleBotMessage(_ context.Context, event contracts.NormalizedTelegramEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	if len(c.events) == c.want {
		close(c.done)
	}
	return nil
}

// TestPollerDeliversUpdatesAndAdvancesOffset verifies the full poll loop:
// updates flow to the handler as normalized bot-transport events and the
// persisted offset moves past every delivered update.
func TestPollerDeliversUpdatesAndAdvancesOffset(t *testing.T) {
	api := &fakeTelegram{
		updateBatches: [][]json.RawMessage{
			{rawUpdate(t, 100, 42, 1, "раз"), rawUpdate(t, 101, 42, 2, "два")},
			{rawUpdate(t, 102, 43, 3, "три")},
		},
	}
	server := httptest.NewServer(api.handler(t))
	defer server.Close()

	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: "tenant:user:77", BotID: 555, Token: "secret-token", Status: "active"})
	handler := &collectingHandler{done: make(chan struct{}), want: 3}
	manager := NewManager(repo, handler, server.Client(),
		WithBaseURL(server.URL),
		WithDefaultIngestionMode(contracts.IngestionModeBot),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go manager.Run(ctx)

	select {
	case <-handler.done:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for events; got %d", len(handler.events))
	}

	// The follow-up poll must resume from 103 (past every delivered update).
	deadline := time.After(2 * time.Second)
	for {
		api.mu.Lock()
		resumed := len(api.getUpdateCalls) > 0 && api.getUpdateCalls[len(api.getUpdateCalls)-1] == 103
		api.mu.Unlock()
		if resumed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("poller did not resume from offset 103")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	handler.mu.Lock()
	defer handler.mu.Unlock()
	first := handler.events[0]
	if first.Transport != contracts.TransportBot || first.BotID != "555" || first.TenantID != "tenant:user:77" {
		t.Fatalf("unexpected event metadata: %+v", first)
	}
	if first.EventID != "100:bot:555:1" {
		t.Fatalf("event id = %q, want 100:bot:555:1", first.EventID)
	}
	if key := first.ReceiveIdempotencyKey(); key != "100:bot:555" {
		t.Fatalf("idempotency key = %q, want 100:bot:555", key)
	}
	// Offset persisted past the last delivered update.
	bot, ok := repo.UserBotByID("tenant:user:77", 555)
	if !ok || bot.LastUpdateOffset != 102 {
		t.Fatalf("persisted offset = %d, want 102", bot.LastUpdateOffset)
	}
}

// TestPollerResolvesWebhookConflict verifies a 409 from getUpdates triggers a
// one-time deleteWebhook and polling continues.
func TestPollerResolvesWebhookConflict(t *testing.T) {
	api := &fakeTelegram{
		statusQueue:   []int{http.StatusConflict},
		updateBatches: [][]json.RawMessage{{rawUpdate(t, 200, 42, 1, "после конфликта")}},
	}
	server := httptest.NewServer(api.handler(t))
	defer server.Close()

	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: "tenant:user:77", BotID: 555, Token: "secret-token", Status: "active"})
	handler := &collectingHandler{done: make(chan struct{}), want: 1}
	manager := NewManager(repo, handler, server.Client(), WithBaseURL(server.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go manager.Run(ctx)

	select {
	case <-handler.done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for post-conflict update")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.webhookDeletes != 1 {
		t.Fatalf("deleteWebhook calls = %d, want 1", api.webhookDeletes)
	}
}

// TestPollerStopsOnInvalidToken verifies a 401 marks the bot invalid and the
// poller exits instead of hammering Telegram.
func TestPollerStopsOnInvalidToken(t *testing.T) {
	api := &fakeTelegram{statusQueue: []int{http.StatusUnauthorized}}
	server := httptest.NewServer(api.handler(t))
	defer server.Close()

	repo := store.NewRepository()
	repo.SaveUserBot(store.UserBotRecord{TenantID: "tenant:user:77", BotID: 555, Token: "bad-token", Status: "active"})
	manager := NewManager(repo, &collectingHandler{done: make(chan struct{}), want: 1}, server.Client(), WithBaseURL(server.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go manager.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		bot, _ := repo.UserBotByID("tenant:user:77", 555)
		if bot.Status == "invalid" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("bot status = %q, want invalid", bot.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestRegisterBotValidatesTokenAndStartsPolling verifies getMe-backed
// registration plus the personal-mode gate that keeps the poller off.
func TestRegisterBotValidatesTokenAndStartsPolling(t *testing.T) {
	api := &fakeTelegram{updateBatches: [][]json.RawMessage{{rawUpdate(t, 300, 42, 1, "hi")}}}
	server := httptest.NewServer(api.handler(t))
	defer server.Close()

	repo := store.NewRepository()
	handler := &collectingHandler{done: make(chan struct{}), want: 1}
	manager := NewManager(repo, handler, server.Client(), WithBaseURL(server.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go manager.Run(ctx)

	record, err := manager.RegisterBot(ctx, "tenant:user:77", "secret-token")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if record.BotID != 555 || record.BotUsername != "user_assistant_bot" {
		t.Fatalf("unexpected registration: %+v", record)
	}
	select {
	case <-handler.done:
	case <-ctx.Done():
		t.Fatal("poller did not start after registration")
	}

	// SenderFor exposes the per-tenant outbound sender.
	if _, ok := manager.SenderFor("tenant:user:77", "555"); !ok {
		t.Fatal("expected sender for registered tenant")
	}
	if _, ok := manager.SenderFor("tenant:user:99", "555"); ok {
		t.Fatal("unexpected sender for unknown tenant")
	}

	// Personal mode stops the poller via Refresh; bot mode starts it again.
	repo.SetTenantIngestionMode("tenant:user:77", contracts.IngestionModePersonal)
	manager.Refresh("tenant:user:77")
	manager.mu.Lock()
	_, running := manager.pollers[555]
	manager.mu.Unlock()
	if running {
		t.Fatal("poller must stop when ingestion mode is personal")
	}
}
