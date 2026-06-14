// Package botservice ingests Telegram messages through user-supplied bots.
//
// Each tenant registers one bot token; the manager runs one long-polling
// goroutine per active bot (getUpdates), normalizes message updates into the
// frozen contract event and hands them to the core pipeline. Outbound replies
// for bot-transport chats are sent through the same bot via SenderFor.
//
// The package is deliberately decoupled from internal/core (narrow Handler
// interface) so it can be extracted into a standalone service later; the
// long-polling transport hides behind the manager so a webhook-based
// UpdateSource can replace it without touching core.
//
// Security invariant: bot tokens are credentials — they never appear in logs
// or error messages (see redactToken).
package botservice

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
)

// defaultAPIBaseURL is the production Telegram Bot API endpoint.
const defaultAPIBaseURL = "https://api.telegram.org"

// Handler consumes normalized bot-transport events (implemented by
// core.Service through an adapter in cmd/backend).
type Handler interface {
	HandleBotMessage(ctx context.Context, event contracts.NormalizedTelegramEvent) error
}

// HandlerFunc adapts a plain function to Handler.
type HandlerFunc func(ctx context.Context, event contracts.NormalizedTelegramEvent) error

// HandleBotMessage calls f.
func (f HandlerFunc) HandleBotMessage(ctx context.Context, event contracts.NormalizedTelegramEvent) error {
	return f(ctx, event)
}

// Store is the persistence surface the manager needs; *store.Repository implements it.
type Store interface {
	ActiveUserBots() []store.UserBotRecord
	SaveUserBot(record store.UserBotRecord)
	UserBotByTenant(tenantID string) (store.UserBotRecord, bool)
	UpdateUserBotOffset(botID int64, offset int64)
	UpdateUserBotStatus(botID int64, status string)
	DeleteUserBot(tenantID string)
	TenantIngestionMode(tenantID string) contracts.IngestionMode
}

// Option configures a Manager.
type Option func(*Manager)

// WithLogger sets the structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(m *Manager) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// WithBaseURL overrides the Telegram API base URL (tests point it at httptest).
func WithBaseURL(baseURL string) Option {
	return func(m *Manager) {
		if baseURL != "" {
			m.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithDefaultIngestionMode sets the mode applied to tenants that have not
// chosen one explicitly (env INGESTION_MODE_DEFAULT).
func WithDefaultIngestionMode(mode contracts.IngestionMode) Option {
	return func(m *Manager) {
		if mode != "" {
			m.defaultMode = mode
		}
	}
}

// Manager owns the per-bot pollers and the per-tenant outbound senders.
type Manager struct {
	repo        Store
	handler     Handler
	logger      *slog.Logger
	baseURL     string
	pollClient  *http.Client // long-poll: timeout must exceed the 50s getUpdates hold
	sendClient  *http.Client // short-timeout client shared with the control-bot sender
	defaultMode contracts.IngestionMode

	mu      sync.Mutex
	runCtx  context.Context // set by Run; parent of every poller goroutine
	pollers map[int64]context.CancelFunc
}

// NewManager creates a bot ingestion manager. sendClient is the short-timeout
// HTTP client used for outbound sends and registration calls (getMe).
func NewManager(repo Store, handler Handler, sendClient *http.Client, options ...Option) *Manager {
	if sendClient == nil {
		sendClient = &http.Client{Timeout: 10 * time.Second}
	}
	m := &Manager{
		repo:    repo,
		handler: handler,
		logger:  slog.Default(),
		baseURL: defaultAPIBaseURL,
		// getUpdates holds the connection for up to longPollTimeoutSeconds;
		// the client timeout must be comfortably above that.
		pollClient:  &http.Client{Timeout: (longPollTimeoutSeconds + 15) * time.Second},
		sendClient:  sendClient,
		defaultMode: contracts.IngestionModeBot,
		pollers:     make(map[int64]context.CancelFunc),
	}
	for _, option := range options {
		option(m)
	}
	return m
}

// Run starts pollers for every active bot whose tenant allows bot ingestion
// and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
	bots := m.repo.ActiveUserBots()
	for _, bot := range bots {
		if !m.ingestionMode(bot.TenantID).AllowsBot() {
			continue
		}
		m.startPoller(bot)
	}
	m.logger.Info("bot ingestion manager started", "active_bots", len(bots))
	<-ctx.Done()
}

// RegisterBot validates the token via getMe, persists the registration and
// starts polling when the tenant's ingestion mode allows it. An existing
// registration is replaced; re-registering the same bot keeps its offset so
// no acknowledged updates are replayed.
func (m *Manager) RegisterBot(ctx context.Context, tenantID, token string) (store.UserBotRecord, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return store.UserBotRecord{}, fmt.Errorf("bot token is empty")
	}
	me, err := getMe(ctx, m.sendClient, m.baseURL, token)
	if err != nil {
		return store.UserBotRecord{}, fmt.Errorf("telegram getMe: %w", redactToken(err, token))
	}
	record := store.UserBotRecord{
		TenantID:    tenantID,
		BotID:       me.ID,
		BotUsername: me.Username,
		Token:       token,
		Status:      "active",
	}
	if existing, ok := m.repo.UserBotByTenant(tenantID); ok {
		m.stopPoller(existing.BotID)
		if existing.BotID == record.BotID {
			record.LastUpdateOffset = existing.LastUpdateOffset
		}
	}
	m.repo.SaveUserBot(record)
	if m.ingestionMode(tenantID).AllowsBot() {
		m.startPoller(record)
	}
	m.logger.Info("user bot registered",
		"tenant_id", tenantID, "bot_id", record.BotID, "bot_username", record.BotUsername)
	return record, nil
}

// DisconnectBot stops polling and removes the tenant's bot registration.
func (m *Manager) DisconnectBot(tenantID string) {
	if existing, ok := m.repo.UserBotByTenant(tenantID); ok {
		m.stopPoller(existing.BotID)
	}
	m.repo.DeleteUserBot(tenantID)
	m.logger.Info("user bot disconnected", "tenant_id", tenantID)
}

// Refresh re-evaluates the tenant's poller after an ingestion-mode change:
// starts it when the mode now allows bot ingestion, stops it otherwise.
func (m *Manager) Refresh(tenantID string) {
	bot, ok := m.repo.UserBotByTenant(tenantID)
	if !ok || bot.Status != "active" {
		return
	}
	if m.ingestionMode(tenantID).AllowsBot() {
		m.startPoller(bot)
	} else {
		m.stopPoller(bot.BotID)
	}
}

// SenderFor returns a Telegram sender bound to the tenant's bot token.
// ok is false when the tenant has no active bot.
func (m *Manager) SenderFor(tenantID string) (telegram.Sender, bool) {
	bot, found := m.repo.UserBotByTenant(tenantID)
	if !found || bot.Status != "active" {
		return nil, false
	}
	return telegram.NewBotSender(bot.Token, m.sendClient), true
}

// ingestionMode resolves the tenant's effective mode (explicit or default).
func (m *Manager) ingestionMode(tenantID string) contracts.IngestionMode {
	if mode := m.repo.TenantIngestionMode(tenantID); mode != "" {
		return mode
	}
	return m.defaultMode
}

// startPoller launches the long-poll goroutine for one bot (idempotent).
func (m *Manager) startPoller(bot store.UserBotRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, running := m.pollers[bot.BotID]; running {
		return
	}
	parent := m.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.pollers[bot.BotID] = cancel
	p := &poller{
		bot:     bot,
		baseURL: m.baseURL,
		client:  m.pollClient,
		handler: m.handler,
		repo:    m.repo,
		logger:  m.logger,
	}
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.pollers, bot.BotID)
			m.mu.Unlock()
		}()
		p.run(ctx)
	}()
	m.logger.Info("bot poller started", "tenant_id", bot.TenantID, "bot_id", bot.BotID, "bot_username", bot.BotUsername)
}

// stopPoller cancels the poller goroutine for one bot (no-op when not running).
func (m *Manager) stopPoller(botID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.pollers[botID]; ok {
		cancel()
		delete(m.pollers, botID)
	}
}

// redactToken strips the bot token from an error before it can reach logs:
// net/http errors embed the full request URL, which contains the token.
func redactToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), token, "***")
	return fmt.Errorf("%s", msg)
}
