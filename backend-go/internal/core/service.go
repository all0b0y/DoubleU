// Package core contains backend business workflows and final policy decisions.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/bot"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
	tracepkg "doubleu/backend-go/internal/trace"
)

// DecisionEngine applies backend-owned policy to AI proposals.
type DecisionEngine struct{}

// NewDecisionEngine creates a final decision engine.
func NewDecisionEngine() DecisionEngine {
	return DecisionEngine{}
}

// Decide returns the backend-authoritative decision for an AI proposal.
func (DecisionEngine) Decide(settings contracts.ChatSettings, proposal contracts.AIProcessMessageResponse) (contracts.Decision, string) {
	settings = settings.WithDefaults()
	if !settings.Mode.IsValid() {
		return contracts.DecisionErrorFallback, "invalid_chat_mode"
	}
	if !settings.PrivacyFlags.IsValid() {
		return contracts.DecisionErrorFallback, "invalid_privacy_flags"
	}
	if !proposal.RiskLevel.IsValid() {
		return contracts.DecisionDraft, "invalid_risk_level"
	}
	if proposal.Decision == contracts.DecisionErrorFallback {
		return contracts.DecisionErrorFallback, "ai_error_fallback"
	}
	if proposal.Decision == contracts.DecisionIgnore {
		return contracts.DecisionIgnore, "ai_requested_ignore"
	}
	if settings.Mode == contracts.ChatModeDisabled {
		return contracts.DecisionIgnore, "chat_disabled"
	}
	if settings.Mode == contracts.ChatModeSilent {
		return contracts.DecisionObserve, "chat_silent"
	}
	if proposal.RiskLevel == contracts.RiskCritical {
		// OwnerNote carries the owner-facing warning (ARCH-1); ResponseText is a
		// legacy fallback for AI services that still put the warning there. In
		// both cases the text is owner-only and never sendable to the contact.
		if strings.TrimSpace(proposal.OwnerNote) == "" && strings.TrimSpace(proposal.ResponseText) == "" {
			return contracts.DecisionIgnore, "critical_risk_no_safe_draft"
		}
		return contracts.DecisionAskOwner, "critical_risk_owner_required"
	}
	if proposal.RiskLevel == contracts.RiskHigh {
		return contracts.DecisionDraft, "high_risk_draft_only"
	}
	if proposal.RiskLevel == contracts.RiskMedium {
		return contracts.DecisionDraft, "medium_risk_draft_default"
	}
	if settings.Mode == contracts.ChatModeManual {
		return contracts.DecisionDraft, "manual_mode_owner_required"
	}
	if settings.PrivacyFlags.Contains(contracts.PrivacyPrivate) {
		return contracts.DecisionDraft, "private_chat_blocks_auto_reply"
	}
	// AUTO mode: the owner explicitly opts into unsupervised replies and accepts
	// the risk, so the confidence gate is removed — every low-risk message
	// auto-sends (owner decision, 2026-06-16). MEDIUM/HIGH/CRITICAL risk already
	// returned a draft/ask_owner above, so only low risk reaches here. The empty
	// guard keeps a model that produced no text from sending a blank message.
	if settings.Mode == contracts.ChatModeAuto {
		if strings.TrimSpace(proposal.ResponseText) == "" {
			return contracts.DecisionDraft, "auto_mode_no_reply_text"
		}
		return contracts.DecisionAutoReply, "auto_mode_low_risk_send"
	}
	// SEMI-AUTO (and any other mode) still defer to the AI's auto-reply request
	// and the confidence threshold: send only when the model is confident.
	if proposal.Decision != contracts.DecisionAutoReply {
		return contracts.DecisionDraft, "ai_did_not_request_auto_reply"
	}
	if settings.Mode == contracts.ChatModeSemiAuto {
		if proposal.Confidence >= contracts.SemiAutoMinConfidence {
			return contracts.DecisionAutoReply, "semi_auto_confidence_met"
		}
		return contracts.DecisionDraft, "semi_auto_confidence_below_threshold"
	}
	return contracts.DecisionDraft, "safe_default_draft"
}

// Service coordinates Telegram inbound handling, AI calls, audit, and approvals.
type Service struct {
	repository *store.Repository
	ai         aiclient.Client
	sender     telegram.Sender
	botLayer   *bot.Layer
	engine     DecisionEngine
	logger     *slog.Logger
	now        func() time.Time
	debounce   sync.Map      // map[chatKey]string — latest event_id per chat for typing-delay debounce
	replyDelay       time.Duration // wait before calling AI (lets users finish sending multi-message bursts)
	streamFramePause time.Duration // pause between rich draft preview frames; 0 = no pause (tests)
	// notify pushes real-time events to connected Mini App clients.
	// When set, bot draft cards are suppressed — the Mini App is the primary UI.
	notify func(tenantID, eventType string, payload any)
	// scoreSender attaches owner feedback (approve / reject / thumb) to the
	// Langfuse trace of the draft.  nil means observability is off.
	scoreSender OwnerFeedbackSender
	// webhookWorkers are per-shard ordered queues for async webhook processing
	// (ARCH-7). Updates for the same chat hash to the same shard, preserving
	// per-chat ordering. nil until StartWebhookWorkers is called.
	webhookWorkers []chan webhookJob
	workersMu      sync.RWMutex
	// asyncLLM bounds concurrent fire-and-forget LLM tasks (extraction,
	// episode summarisation) so a message burst cannot become a request storm.
	asyncLLM chan struct{}
	// tenantLimiters gives each tenant its own token bucket over the shared
	// asyncLLM pool so one noisy tenant cannot starve the others (fair-share,
	// ARCH-7). Keyed by tenant id -> *rate.Limiter; built lazily.
	tenantLimiters sync.Map
	tenantRate     rate.Limit // sustained async-LLM tasks/sec per tenant
	tenantBurst    int        // burst allowance per tenant
	// webhookOverflow is a bounded spill queue: updates that cannot enter their
	// ordered shard land here instead of spawning unbounded goroutines. When it
	// too is full the enqueue is shed (HTTP 429) and Telegram retries.
	webhookOverflow chan webhookJob
	// embedNudge wakes the embedding worker right after new facts are inserted
	// (ARCH-12) instead of waiting for the next 5-minute tick, closing the
	// retrieval blind spot for fresh facts. Buffered(1): coalesces bursts.
	embedNudge chan struct{}
	// botSenders resolves per-tenant outbound senders for bot-transport chats
	// (replies go out through the same user bot the message arrived from).
	botSenders BotSenderProvider
	// media holds the media -> text extraction toggle and limits (Slice 3).
	media MediaConfig
	// mediaLimiters gives each tenant a token bucket over media extraction so a
	// media flood cannot run up provider cost. Keyed by tenant id; built lazily.
	mediaLimiters sync.Map
	// defaultIngestionMode applies to tenants that have not chosen a mode
	// explicitly (env INGESTION_MODE_DEFAULT; "bot" while the Business path
	// is shelved).
	defaultIngestionMode contracts.IngestionMode
	// aiMemorySource: "python" means the AI service persists extracted facts
	// and profile suggestions itself (pydantic-ai runtime with direct DB
	// access) and Go must skip its own write path to avoid double writes.
	// Default "go" keeps the legacy flow (AI_MEMORY_SOURCE flag).
	aiMemorySource string
	// reflectionMode gates sleep-time reflection in the nightly worker:
	// "off" (default), "shadow" (synthesise + log, no write), "enabled" (write
	// behavior_pattern facts). Set via REFLECTION_MODE (#5).
	reflectionMode string
	// summaryCache memoises on-demand chat summaries so repeated requests do not
	// re-call the LLM (and re-bill) when nothing new was said. Keyed by
	// "tenant:chat"; the entry's marker invalidates it as soon as a new message
	// lands, so a returned summary is always accurate for the current timeline.
	summaryCache sync.Map // chatKey -> summaryCacheEntry
}

// summaryCacheEntry is one memoised chat summary plus the message-timeline
// marker it was generated for.
type summaryCacheEntry struct {
	marker   string
	resp     contracts.ChatSummaryResponse
	msgCount int
}

// summaryMarker fingerprints a message timeline. It changes whenever a new
// message is appended (length grows or the latest event id differs), which is
// exactly when a cached summary must be regenerated.
func summaryMarker(messages []store.MessageRecord) string {
	if len(messages) == 0 {
		return "0"
	}
	return fmt.Sprintf("%d:%s", len(messages), messages[len(messages)-1].Event.EventID)
}

// BotSenderProvider resolves the outbound sender bound to one of a tenant's
// user-supplied bots. Implemented by botservice.Manager; the interface keeps
// core decoupled from the polling transport. botID selects which bot answers
// (a tenant may own several); the reply leaves through that exact bot.
type BotSenderProvider interface {
	SenderFor(tenantID, botID string) (telegram.Sender, bool)
}

// webhookJob is one queued Telegram update awaiting async processing.
type webhookJob struct {
	payload    []byte
	tenantHint string
}

const (
	// webhookWorkerCount is the number of ordered processing shards (ARCH-7).
	webhookWorkerCount = 4
	// webhookQueueDepth bounds each shard's backlog; overflow spills to the
	// bounded overflow pool rather than spawning unbounded goroutines.
	webhookQueueDepth = 256
	// webhookOverflowDepth bounds the shared spill queue used when a shard is
	// full; beyond it the service sheds load (429) and relies on Telegram retry.
	webhookOverflowDepth = 512
	// webhookOverflowWorkers drain the overflow queue. Ordering is not preserved
	// here (it is the overload path), but idempotency keys make reprocessing safe.
	webhookOverflowWorkers = 4
	// maxAsyncLLMConcurrency caps simultaneous fire-and-forget LLM calls
	// (typed extraction, message-limit summarisation) across all chats.
	maxAsyncLLMConcurrency = 8
	// defaultTenantAsyncRate / defaultTenantAsyncBurst bound a single tenant's
	// share of the async-LLM pool: 5 tasks/sec sustained, bursts up to 8.
	defaultTenantAsyncRate  = rate.Limit(5)
	defaultTenantAsyncBurst = 8
)

// OwnerFeedbackSender is implemented by observability.ScoreSender; the
// interface keeps the core package decoupled from the OTel/Langfuse SDK.
type OwnerFeedbackSender interface {
	SendOwnerFeedback(ctx context.Context, traceID string, rating string, comment string)
}

// Option configures a Service.
type Option func(*Service)

// WithReplyDelay configures how long the service waits before calling the AI,
// giving users time to finish sending multi-message bursts before a draft is created.
func WithReplyDelay(d time.Duration) Option {
	return func(service *Service) {
		service.replyDelay = d
	}
}

// WithStreamFramePause sets the pause inserted between consecutive rich draft preview frames.
// Defaults to zero (no pause) so tests run instantly; production callers should set ~300 ms
// so the thinking indicator "breathes" visibly before the text appears.
func WithStreamFramePause(d time.Duration) Option {
	return func(service *Service) {
		service.streamFramePause = d
	}
}

// WithClock configures the service clock.
func WithClock(clock func() time.Time) Option {
	return func(service *Service) {
		service.now = clock
	}
}

// WithLogger configures the structured backend logger.
func WithLogger(logger *slog.Logger) Option {
	return func(service *Service) {
		if logger != nil {
			service.logger = logger
		}
	}
}

// WithNotifier configures a real-time notification callback for the Mini App.
// When set, new draft cards are NOT sent via bot — the Mini App is the primary UI.
func WithNotifier(notify func(tenantID, eventType string, payload any)) Option {
	return func(service *Service) {
		service.notify = notify
	}
}

// WithOwnerFeedbackSender wires a Langfuse-backed owner feedback forwarder.
func WithOwnerFeedbackSender(sender OwnerFeedbackSender) Option {
	return func(service *Service) {
		service.scoreSender = sender
	}
}

// WithBotSenderProvider wires per-tenant bot senders for bot-transport replies.
func WithBotSenderProvider(provider BotSenderProvider) Option {
	return func(service *Service) {
		service.botSenders = provider
	}
}

// WithDefaultIngestionMode sets the mode used for tenants without an explicit
// ingestion_mode selection.
func WithDefaultIngestionMode(mode contracts.IngestionMode) Option {
	return func(service *Service) {
		if mode != "" {
			service.defaultIngestionMode = mode
		}
	}
}

// WithAIMemorySource selects who persists extracted memory: "go" (legacy,
// default) or "python" (the pydantic-ai runtime writes directly to PostgreSQL
// and Go skips its own fact/suggestion writes).
func WithAIMemorySource(source string) Option {
	return func(service *Service) {
		service.aiMemorySource = source
	}
}

// WithReflectionMode sets sleep-time reflection behaviour: "off" (default),
// "shadow" (synthesise + log only) or "enabled" (write behavior_pattern facts).
func WithReflectionMode(mode string) Option {
	return func(service *Service) {
		service.reflectionMode = mode
	}
}

// WithTenantAsyncRate sets the per-tenant fair-share limit on async-LLM tasks
// (sustained tasks/sec and burst). Zero values keep the defaults.
func WithTenantAsyncRate(r rate.Limit, burst int) Option {
	return func(service *Service) {
		if r > 0 {
			service.tenantRate = r
		}
		if burst > 0 {
			service.tenantBurst = burst
		}
	}
}

// MediaConfig controls the media -> text extraction step (Slice 3 hardening):
// a global toggle plus pre-download caps that keep provider cost bounded.
type MediaConfig struct {
	// Enabled is the global kill switch. When false the message is labelled with
	// its media kind (so the bot still acknowledges it) and no provider call is made.
	Enabled bool
	// MaxBytes caps any attachment; stays at/under Telegram's 20 MB download hard limit.
	MaxBytes int64
	// MaxAudioBytes caps voice/audio specifically (OpenRouter STT rejects >10 MB).
	MaxAudioBytes int64
	// MaxAudioSeconds caps voice/audio duration (OpenRouter STT caps at 5 min).
	MaxAudioSeconds int64
	// RatePerSec / Burst bound per-tenant extraction frequency; over-rate messages
	// fall back to a placeholder rather than triggering a provider call.
	RatePerSec float64
	Burst      int
}

// DefaultMediaConfig returns production defaults: extraction on, caps aligned
// with Telegram (20 MB) and the OpenRouter STT limits (10 MB / 5 min).
func DefaultMediaConfig() MediaConfig {
	return MediaConfig{
		Enabled:         true,
		MaxBytes:        20 * 1024 * 1024,
		MaxAudioBytes:   10 * 1024 * 1024,
		MaxAudioSeconds: 300,
		RatePerSec:      1,
		Burst:           5,
	}
}

// WithMediaConfig overrides the media-extraction toggle and limits.
func WithMediaConfig(cfg MediaConfig) Option {
	return func(service *Service) {
		service.media = cfg
	}
}

// NewService creates a backend core service.
func NewService(repository *store.Repository, ai aiclient.Client, sender telegram.Sender, options ...Option) *Service {
	if repository == nil {
		repository = store.NewRepository()
	}
	if sender == nil {
		sender = telegram.NoopSender{}
	}
	service := &Service{
		repository:           repository,
		ai:                   ai,
		sender:               sender,
		botLayer:             bot.NewLayer(repository),
		engine:               NewDecisionEngine(),
		logger:               slog.Default(),
		now:                  time.Now,
		asyncLLM:             make(chan struct{}, maxAsyncLLMConcurrency),
		embedNudge:           make(chan struct{}, 1),
		defaultIngestionMode: contracts.IngestionModeBot,
		tenantRate:           defaultTenantAsyncRate,
		tenantBurst:          defaultTenantAsyncBurst,
		media:                DefaultMediaConfig(),
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// StartWebhookWorkers launches the ordered per-shard webhook processing pool
// (ARCH-7). Until this is called, EnqueueTelegramWebhook degrades to one
// goroutine per update (the previous behaviour) so tests and minimal setups
// keep working without lifecycle management.
func (s *Service) StartWebhookWorkers(ctx context.Context) {
	s.workersMu.Lock()
	if s.webhookWorkers != nil {
		s.workersMu.Unlock()
		return
	}
	drain := func(jobs <-chan webhookJob) {
		for {
			select {
			case job := <-jobs:
				if _, err := s.HandleTelegramWebhook(ctx, job.payload, job.tenantHint); err != nil {
					s.logger.Error("async webhook processing failed", "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}
	workers := make([]chan webhookJob, webhookWorkerCount)
	for i := range workers {
		workers[i] = make(chan webhookJob, webhookQueueDepth)
		go drain(workers[i])
	}
	// Bounded overflow pool: a fixed set of workers drains the shared spill
	// queue, replacing the old unbounded one-goroutine-per-update fallback.
	overflow := make(chan webhookJob, webhookOverflowDepth)
	for i := 0; i < webhookOverflowWorkers; i++ {
		go drain(overflow)
	}
	s.webhookWorkers = workers
	s.webhookOverflow = overflow
	s.workersMu.Unlock()
}

// EnqueueTelegramWebhook accepts a raw Telegram update for asynchronous
// processing and returns immediately, so the HTTP handler can ACK within
// Telegram's retry window (ARCH-7). Updates for the same business chat are
// routed to the same shard to preserve per-chat ordering; non-chat updates
// (commands, callbacks) have no ordering requirement and spread round-robin
// via the hash of the raw payload.
//
// Returns false only when both the shard and the bounded overflow pool are
// saturated; the caller should then answer 429 so Telegram retries the update
// (idempotency keys make the eventual reprocessing safe). This replaces the old
// unbounded one-goroutine-per-update fallback that could OOM under burst.
func (s *Service) EnqueueTelegramWebhook(payload []byte, tenantHint string) bool {
	s.workersMu.RLock()
	workers := s.webhookWorkers
	overflow := s.webhookOverflow
	s.workersMu.RUnlock()
	if workers == nil {
		go func() {
			_, _ = s.HandleTelegramWebhook(context.Background(), payload, tenantHint)
		}()
		return true
	}
	job := webhookJob{payload: payload, tenantHint: tenantHint}
	shard := webhookShard(payload, tenantHint)
	select {
	case workers[shard] <- job:
		return true
	default:
		// Shard backlog full: spill to the bounded overflow pool. Per-chat
		// ordering may be lost under this overload, but reprocessing is safe.
		select {
		case overflow <- job:
			s.logger.Warn("webhook shard full, spilled to overflow pool", "shard", shard)
			return true
		default:
			// Overflow saturated too: shed load. Telegram retries within its
			// window, so the update is deferred, not lost.
			s.logger.Warn("webhook queues saturated, shedding update", "shard", shard)
			return false
		}
	}
}

// webhookShard maps an update to a worker shard. Business messages hash by
// (tenantHint, chat id) so one chat is always processed in order.
func webhookShard(payload []byte, tenantHint string) int {
	var probe struct {
		BusinessMessage struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"business_message"`
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenantHint))
	if json.Unmarshal(payload, &probe) == nil && probe.BusinessMessage.Chat.ID != 0 {
		_, _ = fmt.Fprintf(h, "%d", probe.BusinessMessage.Chat.ID)
	} else {
		_, _ = h.Write(payload)
	}
	return int(h.Sum32() % webhookWorkerCount)
}

// tenantLimiter returns the token bucket for a tenant, creating it on first use.
func (s *Service) tenantLimiter(tenantID string) *rate.Limiter {
	if v, ok := s.tenantLimiters.Load(tenantID); ok {
		return v.(*rate.Limiter)
	}
	v, _ := s.tenantLimiters.LoadOrStore(tenantID, rate.NewLimiter(s.tenantRate, s.tenantBurst))
	return v.(*rate.Limiter)
}

// runAsyncLLMTask executes fn in the background under the shared LLM
// concurrency cap (ARCH-7): the goroutine is cheap, but the LLM call inside
// fn waits for a slot so bursts cannot fan out into a request storm.
//
// A per-tenant token bucket gates entry first, so one noisy tenant cannot take
// every slot and starve the others (fair-share). These tasks are best-effort
// background work (fact extraction, summarisation), so when a tenant exceeds
// its rate the task is dropped rather than queued — a later message re-triggers
// the same extraction, and the daily summariser backstops episode summaries.
func (s *Service) runAsyncLLMTask(tenantID string, fn func()) {
	if tenantID != "" && !s.tenantLimiter(tenantID).Allow() {
		s.logger.Warn("async LLM task shed: tenant rate exceeded", "tenant_id", tenantID)
		return
	}
	go func() {
		s.asyncLLM <- struct{}{}
		defer func() { <-s.asyncLLM }()
		fn()
	}()
}

// InboundResult describes the result of one Telegram webhook.
type InboundResult struct {
	Event     contracts.NormalizedTelegramEvent `json:"event"`
	Decision  contracts.Decision                `json:"decision"`
	Reason    string                            `json:"reason"`
	Duplicate bool                              `json:"duplicate"`
	DraftID   string                            `json:"draft_id,omitempty"`
}

// ApprovalResult describes the result of one owner callback.
type ApprovalResult struct {
	Action    contracts.ApprovalAction `json:"action"`
	Duplicate bool                     `json:"duplicate"`
	DraftID   string                   `json:"draft_id,omitempty"`
	Status    string                   `json:"status"`
}

// HandleTelegramWebhook normalizes and processes a Telegram webhook payload.
func (s *Service) HandleTelegramWebhook(ctx context.Context, payload []byte, tenantHint string) (InboundResult, error) {
	s.logger.Info("telegram webhook received",
		"tenant_hint", tenantHint,
		"payload_size_bytes", len(payload),
		"update_summary", telegramUpdateSummary(payload),
	)
	if connection, ok, err := telegram.ParseBusinessConnectionUpdate(payload, s.now()); err != nil {
		s.logger.Error("telegram business connection parse failed", "error", err)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "business_connection_parse_error:" + err.Error()}, err
	} else if ok {
		return s.handleBusinessConnection(ctx, connection), nil
	}
	if cbq, ok, err := telegram.ParseCallbackQueryUpdate(payload); err != nil {
		s.logger.Error("telegram callback query parse failed", "error", err)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "callback_query_parse_error:" + err.Error()}, err
	} else if ok {
		return s.handleCallbackQuery(ctx, cbq), nil
	}
	if command, ok, err := telegram.ParseBotCommandUpdate(payload); err != nil {
		s.logger.Error("telegram command parse failed", "error", err)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "bot_command_parse_error:" + err.Error()}, err
	} else if ok {
		return s.handleBotCommand(ctx, command), nil
	}
	if ownerMsg, ok, err := telegram.ParseOwnerMessageUpdate(payload); err == nil && ok {
		return s.handleOwnerMessage(ctx, ownerMsg), nil
	}
	if ok, err := telegram.IsDeletedBusinessMessagesUpdate(payload); err != nil {
		s.logger.Error("telegram deleted messages parse failed", "error", err, "tenant_hint", tenantHint)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: "deleted_messages_parse_error:" + err.Error()}, err
	} else if ok {
		return InboundResult{Decision: contracts.DecisionIgnore, Reason: "deleted_business_messages"}, nil
	}

	event, err := telegram.NormalizeUpdate(payload, tenantHint, s.now())
	// Stamp the request context with the deterministic trace-id derived from
	// event_id.  Idempotent webhook replays share a trace (Phase 0 envelope).
	ctx = tracepkg.WithTraceID(ctx, tracepkg.DeriveTraceID(event.EventID))
	if err != nil {
		audit := s.auditEvent(contracts.NormalizedTelegramEvent{
			TenantID:        valueOrDefault(tenantHint, "unknown"),
			ChatID:          "unknown",
			SourceMessageID: "unknown",
		}, contracts.DecisionErrorFallback, "normalization_error:"+err.Error())
		s.repository.AppendAudit(audit)
		s.logger.Error("telegram business message normalization failed", "error", err, "tenant_hint", tenantHint)
		return InboundResult{Decision: contracts.DecisionErrorFallback, Reason: audit.Reason}, err
	}
	if tenantHint == "" {
		if connection, ok := s.repository.BusinessConnection(event.BusinessConnectionID); ok {
			event.TenantID = connection.TenantID
		} else if tenantID, ok := s.repository.TenantForBusinessConnection(event.BusinessConnectionID); ok {
			event.TenantID = tenantID
		}
		// Auto-derived tenants must look like a real user tenant; otherwise drop
		// to avoid poisoning unclaimed_chats.owner_tenant_id with a fallback value
		// such as the business connection ID.  An explicit tenantHint from the
		// webhook URL is trusted as-is and bypasses this check.
		if !strings.HasPrefix(event.TenantID, "tenant:user:") {
			event.TenantID = ""
		}
	}
	connection, hasConnection := s.repository.BusinessConnection(event.BusinessConnectionID)
	ownerID := ""
	if hasConnection {
		ownerID = connection.UserID
	} else if strings.HasPrefix(event.TenantID, "tenant:user:") {
		ownerID = strings.TrimPrefix(event.TenantID, "tenant:user:")
	} else if ownerTenant := s.repository.OwnerTenantForBusinessConnection(event.BusinessConnectionID); strings.HasPrefix(ownerTenant, "tenant:user:") {
		ownerID = strings.TrimPrefix(ownerTenant, "tenant:user:")
	}
	if event.ChatDisplayName == "" {
		event.ChatDisplayName = anonymousChatName(event.ChatID)
	}

	// Ingestion-mode gate: when the tenant runs on bot ingestion, business
	// messages are acknowledged but not processed. The Business path is
	// shelved, not removed — switching the mode back re-enables it.
	if !s.ingestionModeFor(event.TenantID).AllowsBusiness() {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "ingestion_mode_bot"))
		s.logger.Info("business message skipped by ingestion mode",
			"tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "ingestion_mode_bot"}, nil
	}
	event.Transport = contracts.TransportBusiness

	// Gate: only process chats the owner has explicitly added.
	// New contacts are staged in unclaimed_chats for owner decision.
	// When the owner tenant is known, messages are persisted and facts are extracted in the
	// background so history and memory are available immediately after the owner claims the chat.
	if event.BusinessConnectionID != "" && !s.repository.IsChatClaimed(event.TenantID, event.ChatID) {
		s.repository.SaveUnclaimedChat(store.UnclaimedChatRecord{
			OwnerTenantID:        event.TenantID,
			BusinessConnectionID: event.BusinessConnectionID,
			ChatID:               event.ChatID,
			DisplayName:          event.ChatDisplayName,
			ChatType:             event.ChatType,
			LastMessageAt:        s.now(),
			Status:               "pending",
		})
		if event.TenantID != "" {
			// Owner is known: persist the chat row + message so history survives claim.
			s.repository.SaveChat(store.ChatRecord{
				TenantID:             event.TenantID,
				ChatID:               event.ChatID,
				DisplayName:          event.ChatDisplayName,
				ChatType:             event.ChatType,
				BusinessConnectionID: event.BusinessConnectionID,
				LastSourceMessageID:  event.SourceMessageID,
				LastSeenAt:           s.now(),
				Status:               "unclaimed",
			})
			s.repository.SaveMessage(event)
			s.processPassive(ctx, event)
		}
		s.logger.Info("unclaimed business chat — passive RAG processing",
			"chat_id", event.ChatID,
			"business_connection_id", event.BusinessConnectionID,
			"owner_tenant_id", event.TenantID,
		)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "passive_processing"}, nil
	}

	replyBlockNote := ""
	if hasConnection && !connection.CanReply {
		replyBlockNote = "Telegram не дал боту право отвечать от имени владельца. Включи reply permission в Telegram Settings -> Chat Automation или ответь вручную."
	}
	return s.processContactMessage(ctx, event, ownerID, replyBlockNote)
}

// HandleBotMessage processes a normalized message that arrived through the
// tenant's user-supplied bot (botservice poller). Unlike the Business webhook
// path the tenant is always known from the bot registration, so there is no
// connection resolution and no unclaimed staging: the owner connected the bot
// deliberately, so its chats are managed immediately.
func (s *Service) HandleBotMessage(ctx context.Context, event contracts.NormalizedTelegramEvent) (InboundResult, error) {
	ctx = tracepkg.WithTraceID(ctx, tracepkg.DeriveTraceID(event.EventID))
	if !s.ingestionModeFor(event.TenantID).AllowsBot() {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "ingestion_mode_personal"))
		s.logger.Info("bot message skipped by ingestion mode",
			"tenant_id", event.TenantID, "chat_id", event.ChatID, "bot_id", event.BotID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "ingestion_mode_personal"}, nil
	}
	if event.ChatDisplayName == "" {
		event.ChatDisplayName = anonymousChatName(event.ChatID)
	}
	// Bot tenants are keyed "tenant:user:<telegram_user_id>" at registration,
	// so the owner id falls out of the tenant id without a connection lookup.
	ownerID := strings.TrimPrefix(event.TenantID, "tenant:user:")
	if ownerID == event.TenantID {
		ownerID = ""
	}
	s.logger.Info("bot message received",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"bot_id", event.BotID,
		"source_message_id", event.SourceMessageID,
	)
	return s.processContactMessage(ctx, event, ownerID, "")
}

// mediaExtractTimeout bounds the download + extract step so a slow or oversized
// attachment never stalls a bot poller's sequential loop indefinitely.
const mediaExtractTimeout = 60 * time.Second

// EnrichBotMedia turns a bot message's media attachment into text before AI
// processing so the agent stays text-only (media -> text). The enriched text is
// written to event.MessageText, where it flows through drafting, memory and
// embeddings unchanged.
//
// It is best-effort and never fails the message: with no media, no downloader,
// no media-capable AI client, or an extraction error/timeout it leaves a short
// placeholder so the bot still acknowledges the message instead of going silent.
// Raw media bytes are held only in memory for the call and never persisted.
func (s *Service) EnrichBotMedia(ctx context.Context, event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor) contracts.NormalizedTelegramEvent {
	if media == nil {
		return event
	}
	// Pre-download gates (toggle, size, duration, rate) so an oversized or flood
	// of media never triggers a wasted download or provider call.
	if reason := s.mediaRejectReason(event.TenantID, media); reason != "" {
		s.logger.Info("bot media skipped before extraction",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"kind", media.Kind, "size", media.FileSize, "duration", media.Duration, "reason", reason)
		return s.placeholderEvent(event, media, reason)
	}
	extractor, ok := s.ai.(aiclient.MediaExtractor)
	if !ok {
		return s.placeholderEvent(event, media, mediaReasonUnprocessable)
	}
	text, err := s.extractBotMedia(ctx, event, media, extractor)
	if err != nil || text == "" {
		s.logger.Warn("bot media extraction failed; using placeholder",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"kind", media.Kind, "error", err)
		return s.placeholderEvent(event, media, mediaReasonUnprocessable)
	}
	event.MessageText = mergeMediaText(event.MessageText, text, media.Kind)
	s.logger.Info("bot media extracted to text",
		"tenant_id", event.TenantID, "chat_id", event.ChatID,
		"kind", media.Kind, "chars", len(text))
	return event
}

// Placeholder reasons shown (as a label suffix) when media is not turned into
// text, so the contact-facing model can respond appropriately.
const (
	mediaReasonUnprocessable = "не удалось обработать"
	mediaReasonDisabled      = "обработка медиа отключена"
	mediaReasonTooLarge      = "слишком большое"
	mediaReasonTooLong       = "слишком длинное"
	mediaReasonRateLimited   = "слишком много медиа"
)

// mediaRejectReason returns a non-empty placeholder reason when the attachment
// must be skipped before any download (disabled, oversized, too long, or the
// tenant is over its media rate), or "" when it may be extracted. Missing size
// or duration (0) never rejects — the download hard cap still backstops size.
func (s *Service) mediaRejectReason(tenantID string, media *telegram.MediaDescriptor) string {
	if !s.media.Enabled {
		return mediaReasonDisabled
	}
	isAudio := media.Kind == "voice" || media.Kind == "audio"
	maxBytes := s.media.MaxBytes
	if isAudio && s.media.MaxAudioBytes > 0 && (maxBytes == 0 || s.media.MaxAudioBytes < maxBytes) {
		maxBytes = s.media.MaxAudioBytes
	}
	if maxBytes > 0 && media.FileSize > maxBytes {
		return mediaReasonTooLarge
	}
	if isAudio && s.media.MaxAudioSeconds > 0 && media.Duration > s.media.MaxAudioSeconds {
		return mediaReasonTooLong
	}
	// Rate gate last so a token is consumed only for an otherwise-acceptable
	// attachment we are about to extract.
	if tenantID != "" && !s.mediaLimiter(tenantID).Allow() {
		return mediaReasonRateLimited
	}
	return ""
}

// mediaLimiter returns the per-tenant media-extraction token bucket, creating it
// on first use. A non-positive configured rate disables the gate (infinite).
func (s *Service) mediaLimiter(tenantID string) *rate.Limiter {
	if v, ok := s.mediaLimiters.Load(tenantID); ok {
		return v.(*rate.Limiter)
	}
	limit := rate.Limit(s.media.RatePerSec)
	if s.media.RatePerSec <= 0 {
		limit = rate.Inf
	}
	burst := s.media.Burst
	if burst <= 0 {
		burst = 1
	}
	v, _ := s.mediaLimiters.LoadOrStore(tenantID, rate.NewLimiter(limit, burst))
	return v.(*rate.Limiter)
}

// extractBotMedia downloads the attachment through the message's own bot and
// converts it to text via the Python AI service. The download uses the per-bot
// token resolved by SenderFor (multibot), keeping the token on the Go side.
func (s *Service) extractBotMedia(ctx context.Context, event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor, extractor aiclient.MediaExtractor) (string, error) {
	if s.botSenders == nil {
		return "", fmt.Errorf("no bot sender provider")
	}
	sender, ok := s.botSenders.SenderFor(event.TenantID, event.BotID)
	if !ok {
		return "", fmt.Errorf("no sender for bot %s", event.BotID)
	}
	downloader, ok := sender.(telegram.FileDownloader)
	if !ok {
		return "", fmt.Errorf("sender does not support file download")
	}
	dlCtx, cancel := context.WithTimeout(ctx, mediaExtractTimeout)
	defer cancel()
	path, err := downloader.GetFilePath(dlCtx, media.FileID)
	if err != nil {
		return "", err
	}
	data, downloadedMime, err := downloader.DownloadFile(dlCtx, path)
	if err != nil {
		return "", err
	}
	// The message's declared mime (voice.mime_type, document.mime_type) is more
	// reliable than the download response's generic Content-Type.
	mime := media.MimeType
	if mime == "" {
		mime = downloadedMime
	}
	resp, err := extractor.ExtractMedia(dlCtx, aiclient.ExtractMediaRequest{
		Kind:       media.Kind,
		DataBase64: base64.StdEncoding.EncodeToString(data),
		MimeType:   mime,
		Duration:   media.Duration,
		TraceID:    tracepkg.DeriveTraceID(event.EventID),
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// placeholderEvent labels the message with its media kind and the reason the
// content was not extracted, so the AI can still respond (e.g. ask the user to
// resend as text) rather than seeing an empty message.
func (s *Service) placeholderEvent(event contracts.NormalizedTelegramEvent, media *telegram.MediaDescriptor, reason string) contracts.NormalizedTelegramEvent {
	placeholder := "[" + mediaLabel(media.Kind) + " — " + reason + "]"
	if existing := strings.TrimSpace(event.MessageText); existing != "" {
		event.MessageText = existing + "\n" + placeholder
	} else {
		event.MessageText = placeholder
	}
	return event
}

// mergeMediaText combines any caption with the extracted media text under a
// label so the model knows the content was transcribed/OCR'd from an attachment
// rather than typed by the contact.
func mergeMediaText(caption, extracted, kind string) string {
	var b strings.Builder
	if c := strings.TrimSpace(caption); c != "" {
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("[")
	b.WriteString(mediaLabel(kind))
	b.WriteString("]: ")
	b.WriteString(extracted)
	return b.String()
}

// mediaLabel is the contact-language (Russian) label for a media kind, used in
// the text handed to the model.
func mediaLabel(kind string) string {
	switch kind {
	case "voice":
		return "голосовое сообщение"
	case "audio":
		return "аудио"
	case "photo":
		return "изображение"
	case "document":
		return "документ"
	default:
		return "вложение"
	}
}

// pythonOwnsMemory reports whether fact persistence is delegated to the
// Python agent runtime (AI_MEMORY_SOURCE=python).
func (s *Service) pythonOwnsMemory() bool {
	return s.aiMemorySource == "python"
}

// ingestionModeFor resolves the tenant's effective ingestion mode
// (explicit per-tenant selection or the configured default).
func (s *Service) ingestionModeFor(tenantID string) contracts.IngestionMode {
	if mode := s.repository.TenantIngestionMode(tenantID); mode != "" {
		return mode
	}
	return s.defaultIngestionMode
}

// processContactMessage is the transport-agnostic inbound pipeline shared by
// the Business webhook path and the bot polling path: persist → dedupe →
// episode upkeep → debounce → AI call → policy decision → dispatch.
// replyBlockNote, when non-empty, short-circuits AI processing with an
// owner-facing draft explaining why replies are blocked (Business-specific
// can_reply permission).
func (s *Service) processContactMessage(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, replyBlockNote string) (InboundResult, error) {
	settings := s.repository.ChatSettings(event.TenantID, event.ChatID)
	s.repository.SaveChat(store.ChatRecord{
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		DisplayName:          event.ChatDisplayName,
		ChatType:             event.ChatType,
		BusinessConnectionID: event.BusinessConnectionID,
		LastSourceMessageID:  event.SourceMessageID,
		LastSeenAt:           s.now(),
		Transport:            event.Transport,
	})
	// Persist the message row BEFORE any state/audit write so the DB foreign
	// keys from message_states and audit_events back to messages always resolve.
	// no_memory chats skip persistence — for those, AppendState / AppendAudit
	// silently fall back to in-memory tracking inside the repository.
	// quarantined: exact duplicate of a recent contact message → stored for audit
	// but excluded from every memory surface (window-20, episode summaries, extraction).
	quarantined := false
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		recent := s.repository.RecentMessages(event.TenantID, event.ChatID, 5)
		quarantined = classifyQuarantine(event.MessageText, recent)
		s.repository.SaveMessageQuarantined(event, quarantined)
		if quarantined {
			s.logger.Info("message quarantined as duplicate",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"source_message_id", event.SourceMessageID,
			)
		}
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateReceived)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateNormalized)
	s.logger.Info("telegram chat observed and stored",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"chat_name", event.ChatDisplayName,
		"chat_type", event.ChatType,
		"business_connection_id", event.BusinessConnectionID,
		"source_message_id", event.SourceMessageID,
	)
	s.logger.Info("telegram business message received",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"chat_name", event.ChatDisplayName,
		"business_connection_id", event.BusinessConnectionID,
		"source_message_id", event.SourceMessageID,
	)
	if duplicate := s.repository.MarkReceive(event.ReceiveIdempotencyKey()); duplicate {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StatePolicyChecked)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateIgnored)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "duplicate_receive"))
		s.logger.Info("telegram duplicate update ignored", "tenant_id", event.TenantID, "chat_id", event.ChatID, "source_message_id", event.SourceMessageID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_receive", Duplicate: true}, nil
	}

	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		// Runtime State Layer: open or update the active episode for this chat (§6.2).
		now := s.now()
		ep := store.ActiveEpisodeRecord{
			TenantID:      event.TenantID,
			ChatID:        event.ChatID,
			StartedAt:     now,
			LastMessageAt: now,
			MessageCount:  1,
		}
		if existing, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
			ep.StartedAt = existing.StartedAt
			ep.MessageCount = existing.MessageCount + 1
		}
		s.repository.UpsertActiveEpisode(ep)

		// Topic tagging no longer fires a separate LLM call per message: the
		// structured draft response carries the topic (ARCH-4), persisted after
		// the process-message call below. /ai/tag-topic remains for episode workers.

		// Message-limit trigger: close and summarise the episode when it reaches
		// episodeMessageLimit messages (design §6.2). The closure is synchronous so a
		// concurrent handler cannot create a duplicate; summarisation runs in a goroutine.
		if ep.MessageCount >= episodeMessageLimit {
			s.repository.CloseActiveEpisode(event.TenantID, event.ChatID)
			startedAt, closedAt := ep.StartedAt, ep.LastMessageAt
			span := closedAt.Sub(startedAt)
			if span < minEpisodeSpan {
				// A sub-minEpisodeSpan burst of 40 is a flood, not a conversation.
				// Closing the episode resets the runtime counter; real content (if any)
				// is captured later by the time-based daily fallback (summarizeChatDay).
				s.logger.Info("episode closed by flood, summary skipped",
					"tenant_id", event.TenantID,
					"chat_id", event.ChatID,
					"message_count", ep.MessageCount,
					"span_seconds", span.Seconds(),
				)
			} else {
				s.logger.Info("episode closed by message limit",
					"tenant_id", event.TenantID,
					"chat_id", event.ChatID,
					"message_count", ep.MessageCount,
				)
				tenantID, chatID := event.TenantID, event.ChatID
				s.runAsyncLLMTask(event.TenantID, func() {
					s.summarizeEpisode(context.Background(), tenantID, chatID, startedAt, closedAt, "message_count")
				})
			}
		}
	}
	s.repository.IncrementChatMessageCount(event.TenantID, event.ChatID)
	if s.notify != nil {
		s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StatePolicyChecked)
	if isOwnerSideMessage(event, ownerID, event.Transport) {
		// The owner replied themselves: the open question is answered and no
		// owner action is pending any more (§6.2 lifecycle, MEM-5). On bot
		// transport this only fires for synthetic system messages — a human
		// (incl. the owner testing the bot) is a contact the AI answers.
		s.repository.ClearEpisodeRuntimeState(event.TenantID, event.ChatID)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "owner_message_observed"))
		s.logger.Info("owner-authored business message stored as context without ai reply",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"source_message_id", event.SourceMessageID,
			"sender_id", event.SenderID,
		)
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_observed"}, nil
	}

	if replyBlockNote != "" {
		draft := s.createDraft(event, event.EventID, replyBlockNote, contracts.TextFormatPlain)
		s.repository.SaveDraft(draft)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionAskOwner, "business_connection_cannot_reply"))
		s.logger.Info("ai analysis blocked by telegram reply permission", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionAskOwner, Reason: "business_connection_cannot_reply", DraftID: draft.DraftID}, nil
	}

	if settings.Mode == contracts.ChatModeDisabled {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateIgnored)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "chat_disabled"))
		s.logger.Info("ai skipped disabled chat", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "chat_disabled"}, nil
	}
	if settings.Mode == contracts.ChatModeSilent {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "chat_silent"))
		s.logger.Info("ai observing silent chat without reply", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "chat_silent"}, nil
	}
	if s.ai == nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "ai_client_unavailable"))
		s.logger.Error("ai client unavailable", "tenant_id", event.TenantID, "chat_id", event.ChatID)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "ai_client_unavailable"}, nil
	}

	// Burst debounce: mark this message as the latest in the chat, then wait replyDelay.
	// If a newer message arrives during the wait it will overwrite the map entry, so after
	// sleeping we only proceed to AI when we are still the latest message in the burst.
	// All burst messages are saved to context before this point, so the winning message
	// sees the full burst in RecentMessages.
	chatKey := event.TenantID + "/" + event.ChatID
	s.debounce.Store(chatKey, event.EventID)
	if s.replyDelay > 0 {
		select {
		case <-time.After(s.replyDelay):
		case <-ctx.Done():
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "context_cancelled"}, ctx.Err()
		}
		if latest, _ := s.debounce.Load(chatKey); latest != event.EventID {
			s.logger.Info("burst debounce skipped", "tenant_id", event.TenantID, "chat_id", event.ChatID, "event_id", event.EventID)
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "burst_debounce_skipped"}, nil
		}
	}

	aiRole := s.aiRoleFor(event.TenantID, event.ChatID)
	effectiveSettings, botPersonaAuto := botPersonaEffectiveSettings(event, settings, aiRole)
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIRequested)
	s.logger.Info("ai analysis started",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"mode", settings.Mode,
		"effective_mode", effectiveSettings.Mode,
	)
	if s.notify != nil {
		s.notify(event.TenantID, "ai_thinking", map[string]any{"chat_id": event.ChatID})
	}
	memoryContext, recentRecords := s.conversationContextAndMessages(ctx, event, ownerID, 20)
	request := contracts.AIProcessMessageRequest{
		RequestID:       event.EventID,
		TraceID:         tracepkg.DeriveTraceID(event.EventID),
		Event:           event,
		ChatSettings:    effectiveSettings,
		MemoryContext:   memoryContext,
		FeedbackContext: s.feedbackContext(event.TenantID, event.ChatID, 5),
	}
	if profile, ok := s.repository.ContactProfile(event.TenantID, event.ChatID); ok {
		request.ContactProfile = &profile
	}
	// AI role: the persona assigned to this chat (nil → default assistant persona).
	request.AIRole = aiRole
	// User Layer: owner identity and preferences (§3).
	// Also pre-compute delivery settings from the same read so sendRecipient below
	// does not need a second UserSettings lookup on this hot path.
	var hotDelivery deliverySettings
	if us, ok := s.repository.UserSettings(event.TenantID); ok {
		request.UserSettings = &us
		hotDelivery = s.deliverySettingsFromUser(event, &us)
	} else {
		hotDelivery = s.deliverySettingsFromUser(event, nil)
	}
	// Runtime State Layer: current open episode data for AI awareness (§6.2).
	if ep, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
		ae := contracts.ActiveEpisode{
			TenantID:           ep.TenantID,
			ChatID:             ep.ChatID,
			StartedAt:          ep.StartedAt,
			LastMessageAt:      ep.LastMessageAt,
			MessageCount:       ep.MessageCount,
			CurrentTopic:       ep.CurrentTopic,
			OpenQuestionMsgID:  ep.OpenQuestionMsgID,
			PendingOwnerAction: ep.PendingOwnerAction,
		}
		request.ActiveEpisode = &ae
	}
	s.logger.Info("ai process-message request prepared",
		"request_id", request.RequestID,
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"source_message_id", event.SourceMessageID,
		"message_type", event.MessageType,
		"message_text", truncateForLog(event.MessageText, 1000),
		"mode", settings.Mode,
		"effective_mode", effectiveSettings.Mode,
		"privacy_flags", settings.PrivacyFlags,
		"memory_context_count", len(request.MemoryContext),
	)
	proposal, err := s.ai.ProcessMessage(ctx, request)
	if s.notify != nil {
		s.notify(event.TenantID, "ai_done", map[string]any{"chat_id": event.ChatID})
	}
	if err != nil {
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "ai_process_error:"+err.Error()))
		s.logger.Error("ai analysis failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
		s.notifyAIError(event)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "ai_process_error"}, nil
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateAIGenerated)
	// Persist fast (regex) facts returned by process-message synchronously.
	// LLM-based extraction is fired asynchronously below, after the draft is dispatched,
	// so it never adds latency to the reply path (see backlog §7.1).
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) && !s.pythonOwnsMemory() {
		s.saveExtractedFacts(event, proposal.MemoryUsed)
	}
	s.logger.Info("ai analysis completed",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"proposed_decision", proposal.Decision,
		"risk_level", proposal.RiskLevel,
		"confidence", proposal.Confidence,
		"model_used", proposal.ModelUsed,
		"intent", proposal.Intent,
		"response_text", truncateForLog(proposal.ResponseText, 1000),
		"usage_input_tokens", proposal.Usage.InputTokens,
		"usage_output_tokens", proposal.Usage.OutputTokens,
		"memory_used_count", len(proposal.MemoryUsed),
	)

	decision, reason := s.engine.Decide(effectiveSettings, proposal)
	if botPersonaAuto && decision == contracts.DecisionAutoReply {
		reason = "bot_persona_low_risk_send"
	}
	s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDecisionMade)
	s.logger.Info("backend final decision made", "tenant_id", event.TenantID, "chat_id", event.ChatID, "decision", decision, "reason", reason)
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
		s.updateEpisodeRuntime(event, proposal, decision)
	}
	result := InboundResult{Event: event, Decision: decision, Reason: reason}
	switch decision {
	case contracts.DecisionAutoReply:
		sentMsgID, err := s.sendRecipient(ctx, event, event.EventID, proposal.ResponseText, proposal.TextFormat, true, hotDelivery)
		if err != nil {
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
			s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionErrorFallback, "send_error:"+err.Error()))
			s.logger.Error("telegram send failed", "tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "send_error"}, nil
		}
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateSentToRecipient)
		s.logger.Info("telegram auto reply sent", "tenant_id", event.TenantID, "chat_id", event.ChatID, "source_message_id", event.SourceMessageID)
		s.saveOutgoingMessage(event, ownerID, sentMsgID, proposal.ResponseText, event.EventID, proposal.TextFormat)
		// A reply went out on the owner's behalf: nothing is awaited from the owner.
		s.repository.ClearEpisodeRuntimeState(event.TenantID, event.ChatID)
		if s.notify != nil {
			s.notify(event.TenantID, "new_message", map[string]any{"chat_id": event.ChatID})
		}
	case contracts.DecisionDraft, contracts.DecisionAskOwner:
		// Critical-risk warning (ARCH-1): the note is owner-facing only. The draft
		// row carries an empty ResponseText and DraftType="warning" so no approval
		// path can ever deliver the service text to the contact.
		if ownerNote := s.ownerNoteFor(proposal); decision == contracts.DecisionAskOwner && ownerNote != "" {
			draft := s.createDraft(event, event.EventID, "", contracts.TextFormatPlain)
			draft.DraftType = "warning"
			s.repository.SaveDraft(draft)
			s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
			s.logger.Info("warning card prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID)
			result.DraftID = draft.DraftID
			if s.notify != nil {
				s.notify(draft.TenantID, "new_draft", map[string]any{
					"chat_id":     draft.ChatID,
					"draft_id":    draft.DraftID,
					"draft_type":  draft.DraftType,
					"owner_note":  ownerNote,
					"source_text": draft.SourceMessageText,
					"created_at":  draft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				})
			} else {
				s.sendWarningToOwner(ctx, event, draft, ownerNote)
			}
			break
		}
		// When the AI flags the incoming message as a choice question, replace the normal
		// draft flow with an interactive question card that lets the owner pick an option.
		if proposal.Intent == "choice_question" {
			// Structured output carries options directly (ARCH-4); the line-split
			// of ResponseText remains as a legacy fallback.
			options := proposal.Options
			if len(options) == 0 {
				options = parseOptions(proposal.ResponseText)
			}
			if len(options) > 0 {
				draft := s.createDraft(event, event.EventID, "", contracts.TextFormatPlain)
				draft.DraftType = "question"
				draft.SuggestedOptions = options
				s.repository.SaveDraft(draft)
				s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
				s.logger.Info("question card prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID, "options_count", len(options))
				result.DraftID = draft.DraftID
				if s.notify != nil {
					s.notify(draft.TenantID, "new_draft", map[string]any{
						"chat_id":     draft.ChatID,
						"draft_id":    draft.DraftID,
						"draft_type":  draft.DraftType,
						"options":     draft.SuggestedOptions,
						"source_text": draft.SourceMessageText,
						"created_at":  draft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
					})
				} else {
					s.sendQuestionToOwner(ctx, event, draft, ownerID)
				}
				break
			}
		}
		draft := s.createDraft(event, event.EventID, proposal.ResponseText, proposal.TextFormat)
		if proposal.Flagged {
			draft.ActionHistory = append(draft.ActionHistory, "flagged")
		}
		s.repository.SaveDraft(draft)
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateDraftSentToOwner)
		s.logger.Info("owner draft prepared", "tenant_id", event.TenantID, "chat_id", event.ChatID, "draft_id", draft.DraftID, "decision", decision, "flagged", proposal.Flagged)
		result.DraftID = draft.DraftID
		if s.notify != nil {
			s.notify(draft.TenantID, "new_draft", map[string]any{
				"chat_id":       draft.ChatID,
				"draft_id":      draft.DraftID,
				"response_text": draft.ResponseText,
				"source_text":   draft.SourceMessageText,
				"created_at":    draft.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			})
		} else {
			s.sendDraftToOwner(ctx, event, draft, ownerID)
		}
	case contracts.DecisionIgnore:
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateIgnored)
	case contracts.DecisionErrorFallback:
		s.repository.AppendState(event.TenantID, event.ChatID, event.SourceMessageID, contracts.StateFailed)
		// The AI produced no usable reply (transient model failure). Tell the
		// owner so the thinking indicator resolves into a retry notice instead of
		// silently vanishing, which reads as the bot being dead.
		s.notifyAIError(event)
	}
	s.repository.AppendAudit(s.auditEvent(event, decision, reason))

	// Async LLM fact extraction: fires after the draft is dispatched so it never blocks
	// the reply path.  Prefers TypedMemoryExtractor (/ai/extract-facts-typed) for proper
	// scope routing; falls back to MemoryExtractor (/ai/extract-memory) if unavailable.
	// Only runs when the AI client supports extraction, memory is not disabled, the message
	// is not quarantined (spam duplicate), and it passes the cheap fact-bearing pre-filter (MEM-13).
	if !settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) && !quarantined && shouldExtractFacts(event.MessageText) {
		privacyStrings := make([]string, len(settings.PrivacyFlags))
		for i, f := range settings.PrivacyFlags {
			privacyStrings[i] = string(f)
		}
		if typedExtractor, ok := s.ai.(aiclient.TypedMemoryExtractor); ok {
			// Context window for multi-turn fact detection (e.g. "учусь в МГУ"
			// followed by "на медицинском факультете"): reuse the messages already
			// fetched for the prompt (ARCH-8) and keep the last 5 of them.
			recentTexts := make([]string, 0, 5)
			for _, m := range recentRecords {
				if t := strings.TrimSpace(m.Event.MessageText); t != "" && m.Event.SourceMessageID != event.SourceMessageID {
					recentTexts = append(recentTexts, t)
				}
			}
			if len(recentTexts) > 5 {
				recentTexts = recentTexts[len(recentTexts)-5:]
			}
			s.runAsyncLLMTask(event.TenantID, func() {
				extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				start := s.now()
				resp, err := typedExtractor.ExtractFactsTyped(extractCtx, aiclient.TypedExtractionRequest{
					MessageText:    event.MessageText,
					TenantID:       event.TenantID,
					ChatID:         event.ChatID,
					PrivacyFlags:   privacyStrings,
					RecentMessages: recentTexts,
				})
				durationMS := int(s.now().Sub(start).Milliseconds())
				if err != nil {
					s.logger.Warn("async typed extraction failed",
						"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
					s.repository.LogExtraction(store.ExtractionLogRecord{
						TenantID:        event.TenantID,
						ChatID:          event.ChatID,
						InputMessageIDs: []string{event.SourceMessageID},
						Error:           err.Error(),
						DurationMS:      durationMS,
					})
					return
				}
				if resp.BlockedByPrivacy {
					return
				}
				// AI_MEMORY_SOURCE=python: the agent runtime already wrote the
				// facts/suggestions through upsert_semantic_fact; Go only logs.
				outcome := extractionOutcome{}
				if !s.pythonOwnsMemory() {
					outcome = s.saveTypedFacts(event, resp.Facts)
					s.saveProfileSuggestions(extractCtx, event, resp.ProfileSuggestions)
				}
				// Persist the attempt so a returned empty fact set is observable and
				// distinguishable from a silent failure (extraction observability).
				// Health-fact texts are redacted to ID markers first (§10.3, MEM-8).
				s.repository.LogExtraction(store.ExtractionLogRecord{
					TenantID:        event.TenantID,
					ChatID:          event.ChatID,
					InputMessageIDs: []string{event.SourceMessageID},
					ModelUsed:       resp.ModelUsed,
					RawOutput:       redactHealthFacts(resp.RawOutput, resp.Facts, outcome.HealthFactIDs),
					FactsCreated:    outcome.CreatedIDs,
					FactsSkipped:    outcome.Skipped,
					FactsMerged:     resp.DeterministicFactsCount,
					DurationMS:      durationMS,
				})
			})
		} else if extractor, ok := s.ai.(aiclient.MemoryExtractor); ok {
			s.runAsyncLLMTask(event.TenantID, func() {
				extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				resp, err := extractor.ExtractMemory(extractCtx, aiclient.MemoryExtractionRequest{
					MessageText:  event.MessageText,
					TenantID:     event.TenantID,
					ChatID:       event.ChatID,
					PrivacyFlags: privacyStrings,
				})
				if err != nil {
					s.logger.Warn("async memory extraction failed",
						"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
					return
				}
				if !resp.BlockedByPrivacy && !s.pythonOwnsMemory() {
					s.saveExtractedFacts(event, resp.Memories)
				}
			})
		}
	}

	return result, nil
}

func telegramUpdateSummary(payload []byte) map[string]any {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return map[string]any{"parse_error": err.Error()}
	}
	summary := make(map[string]any)
	if updateID, ok := raw["update_id"]; ok {
		var id int64
		if err := json.Unmarshal(updateID, &id); err == nil {
			summary["update_id"] = id
		}
	}
	for _, key := range []string{
		"message",
		"business_connection",
		"business_message",
		"edited_business_message",
		"deleted_business_messages",
		"callback_query",
	} {
		if _, ok := raw[key]; ok {
			summary["kind"] = key
			break
		}
	}
	if _, ok := summary["kind"]; !ok {
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		summary["keys"] = keys
	}
	return summary
}

// contextTokenBudget caps the assembled memory context size (design §8.1 / MEM-9).
// Tokens are estimated as len(text)/3 — coarse but dependency-free for ru/en mix.
const contextTokenBudget = 4000

// estimateTokens returns a rough token count for budget accounting (§8.1).
func estimateTokens(lines []string) int {
	total := 0
	for _, l := range lines {
		total += len(l) / 3
	}
	return total
}

// conversationContext assembles the full memory context slice for the AI.
// See conversationContextAndMessages; this wrapper discards the raw messages.
func (s *Service) conversationContext(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, limit int) []string {
	parts, _ := s.conversationContextAndMessages(ctx, event, ownerID, limit)
	return parts
}

// conversationContextAndMessages assembles the memory context for the AI and
// also returns the raw recent messages so callers (extraction window) reuse the
// same fetch instead of issuing a second query (ARCH-8).
//
// Independent reads run concurrently: the embedding call (~2-5 s) overlaps the
// DB reads instead of preceding them, and the vector-dependent retrieval runs
// once the embedding is ready. The result is budget-trimmed per §8.1: messages
// → episodic summaries → chat facts → pending drafts, in that order; runtime
// lines, commitments, and hard-include facts (taboo/health) are never trimmed.
func (s *Service) conversationContextAndMessages(ctx context.Context, event contracts.NormalizedTelegramEvent, ownerID string, limit int) ([]string, []store.MessageRecord) {
	now := s.now().UTC()

	// Phase 1 — independent reads in parallel (ARCH-8). The embedding API call
	// dominates (~2-5 s); everything else is a DB roundtrip that overlaps it.
	var (
		queryVector []float64
		records     []store.MessageRecord
		greeted     bool
		pausedHours float64
		commitments []store.SemanticFactRecord
		episode     store.ActiveEpisodeRecord
		hasEpisode  bool
		drafts      []store.DraftRecord
		bm25        []store.SemanticFactRecord
	)
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	run(func() {
		// Embed the incoming message for similarity-based retrieval (design §8).
		// Bounded so a slow provider never blocks the reply path; 5 s covers
		// typical OpenRouter embedding latency (~2–3 s) with margin.
		if embedder, ok := s.ai.(aiclient.TextEmbedder); ok && event.MessageText != "" {
			embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			resp, err := embedder.EmbedTexts(embedCtx, aiclient.EmbedTextsRequest{Texts: []string{event.MessageText}})
			if err == nil && len(resp.Embeddings) > 0 {
				queryVector = resp.Embeddings[0]
			}
		}
	})
	run(func() { records = s.repository.RecentMessages(event.TenantID, event.ChatID, limit) })
	run(func() { greeted = s.repository.GreetedToday(event.TenantID, event.ChatID, ownerID) })
	run(func() { pausedHours = s.repository.ChatPausedHours(event.TenantID, event.ChatID) })
	run(func() { commitments = s.repository.OpenCommitments(event.TenantID, event.ChatID) })
	run(func() { episode, hasEpisode = s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID) })
	run(func() { drafts = s.repository.DraftsForChat(event.TenantID, event.ChatID) })
	run(func() {
		if event.MessageText != "" {
			bm25 = s.repository.SemanticFactsByBM25(event.TenantID, event.ChatID, event.MessageText, 20)
		}
	})
	wg.Wait()

	// Phase 2 — vector-dependent retrieval.
	// Memory is strictly per-chat: only facts observed in THIS chat are recalled.
	chatVector := s.repository.SemanticFactsByVector(event.TenantID, event.ChatID, queryVector, 20)
	chatFacts := chatVector
	if len(bm25) > 0 {
		// RRF fusion: merge vector + BM25 ranked lists into a single ordered set,
		// then resolve fused IDs back to records (preserves RRF order).
		fusedChatIDs := rrfFuse(factIDs(chatVector), factIDs(bm25), 60)
		chatFacts = s.repository.SemanticFactsByIDs(event.TenantID, fusedChatIDs)
	}
	// Final ordering (ARCH-5): deterministic cosine reranking in SQL replaces the
	// listwise LLM reranker; hard-include facts (taboo/health, §8.2) always survive.
	chatFacts = s.finalizeFactOrder(event.TenantID, chatFacts, queryVector, 8)
	episodeSummaries := s.repository.EpisodeSummariesByVector(event.TenantID, event.ChatID, queryVector, 3)

	// --- Section assembly (then budget trimming per §8.1) ---

	// Header: dates, runtime flags, commitments — never trimmed.
	header := make([]string, 0, len(commitments)+6)
	header = append(header, fmt.Sprintf("[CURRENT DATE: %s]", now.Format("2006-01-02")))
	// Authoritative greeting state from the DB, always emitted (true AND false)
	// so the Python prompt builder can trust this flag and skip re-deriving it.
	header = append(header, fmt.Sprintf("[RUNTIME: greeting_sent_today=%t]", greeted))
	// Pause length from view_chat_paused_hours — the authoritative gap signal
	// for the Python greeting rule (MEM-9).
	header = append(header, fmt.Sprintf("[RUNTIME: paused_hours=%.1f]", pausedHours))
	for _, c := range commitments {
		header = append(header, fmt.Sprintf("[ОБЯЗАТЕЛЬСТВО: %s]", c.FactText))
	}
	if hasEpisode {
		if episode.CurrentTopic != "" {
			header = append(header, fmt.Sprintf("[RUNTIME: тема разговора] %s", episode.CurrentTopic))
		}
		if episode.PendingOwnerAction != "" {
			header = append(header, fmt.Sprintf("[RUNTIME: ожидается действие владельца] %s", episode.PendingOwnerAction))
		}
		// Unanswered contact question (§6.2, MEM-5): surface its text so the
		// draft can address it instead of only the newest message.
		if episode.OpenQuestionMsgID != "" && episode.OpenQuestionMsgID != event.SourceMessageID {
			if msg, ok := s.repository.Message(event.TenantID, event.ChatID, episode.OpenQuestionMsgID); ok {
				if text := strings.TrimSpace(msg.Event.MessageText); text != "" {
					header = append(header, fmt.Sprintf("[RUNTIME: незакрытый вопрос] %s", text))
				}
			}
		}
	}

	// Facts: hard-include (taboo/health) lines are exempt from trimming.
	hardFactLines := make([]string, 0, 2)
	softFactLines := make([]string, 0, len(chatFacts))
	for _, fact := range chatFacts {
		// Owner-subject facts stay chat-scoped but keep a distinct marker so the
		// prompt can separate "кто пишет" background from contact facts.
		line := fmt.Sprintf("[ФАКТ: %s] %s", fact.FactType, fact.FactText)
		if fact.Subject == "owner" {
			line = fmt.Sprintf("[ФАКТ О ВЛАДЕЛЬЦЕ: %s] %s", fact.FactType, fact.FactText)
		}
		if fact.FactType == "taboo" || fact.FactType == "health" {
			hardFactLines = append(hardFactLines, line)
		} else {
			softFactLines = append(softFactLines, line)
		}
	}

	// Episodic layer: compressed summaries of past closed episodes, oldest first (§7.5).
	summaryLines := make([]string, 0, len(episodeSummaries))
	for i := len(episodeSummaries) - 1; i >= 0; i-- {
		sm := episodeSummaries[i]
		summaryLines = append(summaryLines, fmt.Sprintf("[СВОДКА %s] %s",
			sm.PeriodEnd.UTC().Format("2006-01-02"), sm.Content))
	}

	// Working layer: recent raw messages from this chat (oldest first).
	// Transport is chat-fixed; resolve it once so OWNER/CONTACT labelling matches
	// the bot-vs-business identity rule (see isOwnerSideMessage). On bot transport
	// the human's messages — even the owner's — are CONTACT, and only the bot's
	// own replies ("out:" events) are OWNER.
	transport := s.repository.ChatTransport(event.TenantID, event.ChatID)
	messageLines := make([]string, 0, len(records))
	for _, record := range records {
		if record.Event.SourceMessageID == event.SourceMessageID {
			continue
		}
		text := strings.TrimSpace(record.Event.MessageText)
		if text == "" {
			continue
		}
		role := "CONTACT"
		if isOwnerSideMessage(record.Event, ownerID, transport) {
			role = "OWNER"
		}
		msgDate := record.Event.CreatedAt.UTC().Format("2006-01-02 15:04")
		messageLines = append(messageLines, fmt.Sprintf("[%s] %s: %s", msgDate, role, text))
	}

	// Pending drafts: unanswered obligations awaiting owner approval.
	draftLines := make([]string, 0, 4)
	for _, d := range drafts {
		if d.Status != store.DraftPending {
			continue
		}
		draftLines = append(draftLines, fmt.Sprintf(
			"[ЧЕРНОВИК НЕ ОТПРАВЛЕН] В ответ на: %q\nПредложено (не отправлено): %q",
			d.SourceMessageText, d.ResponseText,
		))
	}

	// Budget trimming, strictly in §8.1 order. Each step only runs while the
	// estimate is still above contextTokenBudget.
	estimate := func() int {
		return estimateTokens(header) + estimateTokens(hardFactLines) + estimateTokens(softFactLines) +
			estimateTokens(summaryLines) + estimateTokens(messageLines) + estimateTokens(draftLines)
	}
	trimmed := []string{}
	if estimate() > contextTokenBudget && len(messageLines) > 10 {
		messageLines = messageLines[len(messageLines)-10:]
		trimmed = append(trimmed, "messages→10")
	}
	if estimate() > contextTokenBudget && len(summaryLines) > 1 {
		// The most relevant summary is the last line (retrieval order was reversed
		// for oldest-first rendering) — keep that one.
		summaryLines = summaryLines[len(summaryLines)-1:]
		trimmed = append(trimmed, "summaries→1")
	}
	if estimate() > contextTokenBudget && len(softFactLines) > 5 {
		softFactLines = softFactLines[:5]
		trimmed = append(trimmed, "facts→5")
	}
	if estimate() > contextTokenBudget && len(draftLines) > 2 {
		draftLines = draftLines[len(draftLines)-2:]
		trimmed = append(trimmed, "drafts→2")
	}
	if len(trimmed) > 0 {
		s.logger.Info("context budget trimming applied",
			"tenant_id", event.TenantID,
			"chat_id", event.ChatID,
			"trimmed", strings.Join(trimmed, ","),
			"final_token_estimate", estimate(),
		)
	}

	// Final assembly preserves the historical section order.
	parts := make([]string, 0, len(header)+len(hardFactLines)+len(softFactLines)+len(summaryLines)+len(messageLines)+len(draftLines)+3)
	parts = append(parts, header...)
	parts = append(parts, hardFactLines...)
	parts = append(parts, softFactLines...)
	if len(summaryLines) > 0 {
		parts = append(parts, "--- ЭПИЗОДИЧЕСКАЯ ПАМЯТЬ (сводки прошлых диалогов) ---")
		parts = append(parts, summaryLines...)
	}
	parts = append(parts, "--- ПАМЯТЬ ТЕКУЩЕГО ДНЯ ---")
	parts = append(parts, messageLines...)
	if len(draftLines) > 0 {
		parts = append(parts, "--- НЕОТПРАВЛЕННЫЕ ЧЕРНОВИКИ (ожидают подтверждения владельца) ---")
		parts = append(parts, draftLines...)
	}
	return parts, records
}

// rrfFuse merges two ranked ID lists using Reciprocal Rank Fusion (k=60).
// Returns a deduplicated slice of IDs in descending fused-score order.
// k=60 is the standard constant that balances precision vs recall for short lists.
func rrfFuse(vectorIDs, bm25IDs []int64, k int) []int64 {
	scores := make(map[int64]float64)
	for rank, id := range vectorIDs {
		scores[id] += 1.0 / float64(k+rank+1)
	}
	for rank, id := range bm25IDs {
		scores[id] += 1.0 / float64(k+rank+1)
	}
	// Collect and sort by descending score.
	type scored struct {
		id    int64
		score float64
	}
	list := make([]scored, 0, len(scores))
	for id, sc := range scores {
		list = append(list, scored{id, sc})
	}
	// Tie-break on id so map iteration order cannot make results non-deterministic.
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].id < list[j].id
	})
	result := make([]int64, len(list))
	for i, s := range list {
		result[i] = s.id
	}
	return result
}

// factIDs extracts the IDs from a slice of SemanticFactRecords, preserving order.
func factIDs(facts []store.SemanticFactRecord) []int64 {
	ids := make([]int64, len(facts))
	for i, f := range facts {
		ids[i] = f.ID
	}
	return ids
}

// finalizeFactOrder reduces RRF-fused recall candidates to the top `limit`
// facts for the prompt. Hard-include facts (taboo/health) always survive;
// the rest are reordered by cosine distance to the query vector in SQL when
// possible, with the incoming RRF order as the deterministic fallback for
// facts that have no embedding yet (the async worker fills those later).
// When the candidate set already fits the limit no ranking work is done at all.
func (s *Service) finalizeFactOrder(tenantID string, chatFacts []store.SemanticFactRecord, queryVector []float64, limit int) []store.SemanticFactRecord {
	hard := make([]store.SemanticFactRecord, 0, 2)
	rest := make([]store.SemanticFactRecord, 0, len(chatFacts))
	for _, f := range chatFacts {
		if f.FactType == "taboo" || f.FactType == "health" {
			hard = append(hard, f)
		} else {
			rest = append(rest, f)
		}
	}
	slots := limit - len(hard)
	if slots < 0 {
		slots = 0
	}
	if len(rest) > slots {
		if len(queryVector) > 0 {
			ranked := s.repository.RankFactsByCosine(tenantID, factIDs(rest), queryVector, slots)
			// Backfill facts without embeddings in their RRF order.
			if len(ranked) < slots {
				seen := make(map[int64]bool, len(ranked))
				for _, f := range ranked {
					seen[f.ID] = true
				}
				for _, f := range rest {
					if len(ranked) >= slots {
						break
					}
					if !seen[f.ID] {
						ranked = append(ranked, f)
					}
				}
			}
			if len(ranked) > 0 {
				rest = ranked
			} else {
				rest = rest[:slots]
			}
		} else {
			rest = rest[:slots]
		}
	}
	return append(hard, rest...)
}

// aiRoleFor resolves the chat's assigned AI role into the contract shape.
// Returns nil when no role is assigned so the Python service falls back to the
// default assistant persona.
func (s *Service) aiRoleFor(tenantID, chatID string) *contracts.AIRole {
	role, ok := s.resolveRole(tenantID, chatID)
	if !ok {
		return nil
	}
	return &contracts.AIRole{
		Name:          role.Name,
		Description:   role.Description,
		SpeakingStyle: role.SpeakingStyle,
		Goals:         role.Goals,
		Constraints:   role.Constraints,
		Context:       role.Context,
		CustomFields:  role.CustomFields,
		MCPServers:    s.repository.RoleMCPServers(role.ID),
		SpeakAsOwner:  role.SpeakAsOwner,
		ModelTier:     role.ModelTier,
	}
}

// botPersonaEffectiveSettings makes an open bot persona conversational by
// default. Bot chats are otherwise born in conservative manual mode, which
// would save every generated answer as an owner draft and leave the Telegram
// user without a reply. Explicit disabled/silent/semi-auto/auto settings,
// owner-speaking roles, business chats and all risk/privacy gates are preserved.
func botPersonaEffectiveSettings(
	event contracts.NormalizedTelegramEvent,
	settings contracts.ChatSettings,
	role *contracts.AIRole,
) (contracts.ChatSettings, bool) {
	if event.Transport != contracts.TransportBot || role == nil || role.SpeakAsOwner ||
		settings.Mode != contracts.ChatModeManual {
		return settings, false
	}
	settings.Mode = contracts.ChatModeAuto
	return settings, true
}

// resolveRole picks the persona for a chat. Bot-transport chats answer in the
// bot's role and nothing else — the bot role overrides any per-chat assignment
// ("и никак иначе"); a bot without a role falls through to the default persona.
// Non-bot chats keep the legacy per-chat role assignment.
func (s *Service) resolveRole(tenantID, chatID string) (store.AIRoleRecord, bool) {
	if s.repository.ChatTransport(tenantID, chatID) == contracts.TransportBot {
		// The chat id is namespaced by bot, so the governing bot — and its role —
		// is recoverable from the chat alone (multi-bot: each bot its own role).
		bot, ok := s.botForChat(tenantID, chatID)
		if !ok || bot.AIRoleID == 0 {
			return store.AIRoleRecord{}, false
		}
		return s.repository.AIRoleByID(tenantID, bot.AIRoleID)
	}
	return s.repository.AIRoleForChat(tenantID, chatID)
}

// botForChat resolves the bot that owns a bot-transport chat from the bot id
// embedded in its namespaced chat id.
func (s *Service) botForChat(tenantID, chatID string) (store.UserBotRecord, bool) {
	botIDStr, _, ok := contracts.ParseBotChatID(chatID)
	if !ok {
		return store.UserBotRecord{}, false
	}
	botID, err := strconv.ParseInt(botIDStr, 10, 64)
	if err != nil {
		return store.UserBotRecord{}, false
	}
	return s.repository.UserBotByID(tenantID, botID)
}

// feedbackContext returns recent draft feedback for this chat to send to the AI.
func (s *Service) feedbackContext(tenantID, chatID string, limit int) []contracts.FeedbackItem {
	feedbacks := s.repository.RecentFeedback(tenantID, chatID, limit)
	items := make([]contracts.FeedbackItem, 0, len(feedbacks))
	for _, fb := range feedbacks {
		items = append(items, contracts.FeedbackItem{
			Rating:       fb.Rating,
			ResponseText: fb.ResponseText,
			FeedbackText: fb.FeedbackText,
		})
	}
	return items
}

// contextMarkerPrefixes are strings that identify assembled prompt context lines injected
// by the Go backend (e.g. date headers, retrieved facts, conversation history).
// If any of these appear in memory_used they are context contamination — not extracted facts —
// and must be rejected to prevent the self-poisoning loop described in the Bug Backlog.
var contextMarkerPrefixes = []string{
	"[CURRENT DATE",
	"[ФАКТ",
	"[FACT",
	"[EXTRACTED",
	"[OWNER STYLE",
	"[OWNER INSTRUCTION",
	"[CURRENT DRAFT",
	"[ЧЕРНОВИК",
	"[RUNTIME",
	"[2026", "[2025", "[2024", "[2023", // timestamped conversation lines
}

// isContextContamination returns true when the string looks like an assembled prompt line
// rather than a newly extracted fact.  This is a defence-in-depth guard; the primary fix
// is in the Python service which must not return input context in memory_used.
func isContextContamination(s string) bool {
	for _, prefix := range contextMarkerPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// embedCandidates synchronously embeds fact texts for write-time dedup (MEM-1).
// One batched call with a 5-second budget; on any failure it returns nil and
// dedup degrades to hash-only (the async embedding worker fills vectors later).
func (s *Service) embedCandidates(texts []string) [][]float64 {
	if len(texts) == 0 {
		return nil
	}
	embedder, ok := s.ai.(aiclient.TextEmbedder)
	if !ok {
		return nil
	}
	embedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := embedder.EmbedTexts(embedCtx, aiclient.EmbedTextsRequest{Texts: texts})
	if err != nil || len(resp.Embeddings) != len(texts) {
		return nil
	}
	return resp.Embeddings
}

// embeddingAt safely indexes a possibly-nil batch embedding result.
func embeddingAt(embeddings [][]float64, i int) []float64 {
	if i < len(embeddings) {
		return embeddings[i]
	}
	return nil
}

// saveExtractedFacts persists facts returned by the AI service into the Semantic Layer.
// Only genuinely new facts extracted from the incoming message are stored; context marker
// strings that indicate assembled prompt lines are rejected as contamination.
// Facts are stored as chat-scoped preference facts — conservative defaults that work
// without typed JSON output from the AI (Phase 4 extractor upgrade will add full typing).
func (s *Service) saveExtractedFacts(event contracts.NormalizedTelegramEvent, facts []string) {
	if len(facts) == 0 || s.repository == nil {
		return
	}
	valid := make([]string, 0, len(facts))
	for _, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" || len(fact) < 5 || len(fact) > 200 {
			continue
		}
		if isContextContamination(fact) {
			s.logger.Warn("saveExtractedFacts: rejected context-contaminated string",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"fact_preview", truncateForLog(fact, 80),
			)
			continue
		}
		valid = append(valid, fact)
	}
	embeddings := s.embedCandidates(valid)
	for i, fact := range valid {
		s.repository.UpsertSemanticFact(store.SemanticFactRecord{
			TenantID:        event.TenantID,
			ScopeKind:       "chat",
			ChatID:          event.ChatID,
			FactText:        fact,
			FactType:        "preference",
			Subject:         "contact",
			Confidence:      0.75,
			Source:          "llm_extracted",
			SourceMessageID: event.SourceMessageID,
		}, embeddingAt(embeddings, i))
	}
}

// processPassive runs asynchronous fact extraction for unclaimed chats.
// It opens/updates the active episode and fires the extractor in a goroutine so it never
// blocks the webhook path. No response is generated and no draft is created.
func (s *Service) processPassive(ctx context.Context, event contracts.NormalizedTelegramEvent) {
	// Maintain an active episode so episode-based context works after claim.
	now := s.now()
	ep := store.ActiveEpisodeRecord{
		TenantID:      event.TenantID,
		ChatID:        event.ChatID,
		StartedAt:     now,
		LastMessageAt: now,
		MessageCount:  1,
	}
	if existing, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
		ep.StartedAt = existing.StartedAt
		ep.MessageCount = existing.MessageCount + 1
	}
	s.repository.UpsertActiveEpisode(ep)

	// Fire lightweight extraction in the background — no response generation.
	// Short fact-free messages are skipped entirely (MEM-13).
	if !shouldExtractFacts(event.MessageText) {
		return
	}
	if typedExtractor, ok := s.ai.(aiclient.TypedMemoryExtractor); ok {
		s.runAsyncLLMTask(event.TenantID, func() {
			extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := s.now()
			resp, err := typedExtractor.ExtractFactsTyped(extractCtx, aiclient.TypedExtractionRequest{
				MessageText: event.MessageText,
				TenantID:    event.TenantID,
				ChatID:      event.ChatID,
			})
			durationMS := int(s.now().Sub(start).Milliseconds())
			if err != nil {
				s.logger.Warn("passive typed extraction failed",
					"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
				s.repository.LogExtraction(store.ExtractionLogRecord{
					TenantID:        event.TenantID,
					ChatID:          event.ChatID,
					InputMessageIDs: []string{event.SourceMessageID},
					Error:           err.Error(),
					DurationMS:      durationMS,
				})
				return
			}
			if resp.BlockedByPrivacy {
				return
			}
			outcome := s.saveTypedFacts(event, resp.Facts)
			s.repository.LogExtraction(store.ExtractionLogRecord{
				TenantID:        event.TenantID,
				ChatID:          event.ChatID,
				InputMessageIDs: []string{event.SourceMessageID},
				ModelUsed:       resp.ModelUsed,
				RawOutput:       redactHealthFacts(resp.RawOutput, resp.Facts, outcome.HealthFactIDs),
				FactsCreated:    outcome.CreatedIDs,
				FactsSkipped:    outcome.Skipped,
				FactsMerged:     resp.DeterministicFactsCount,
				DurationMS:      durationMS,
			})
		})
	} else if extractor, ok := s.ai.(aiclient.MemoryExtractor); ok {
		s.runAsyncLLMTask(event.TenantID, func() {
			extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := extractor.ExtractMemory(extractCtx, aiclient.MemoryExtractionRequest{
				MessageText: event.MessageText,
				TenantID:    event.TenantID,
				ChatID:      event.ChatID,
			})
			if err != nil {
				s.logger.Warn("passive memory extraction failed",
					"tenant_id", event.TenantID, "chat_id", event.ChatID, "error", err)
				return
			}
			if !resp.BlockedByPrivacy {
				s.saveExtractedFacts(event, resp.Memories)
			}
		})
	}
}

// saveTypedFacts persists typed facts returned by the Python AI typed-extraction endpoint.
// Every fact is chat-scoped: memory is strictly per-chat (design v5), so a fact
// observed in one chat never leaks into another — including subject=owner facts.
// Any scope hint from Python is ignored.
//
// extractionOutcome summarises what saveTypedFacts did with a batch of facts, so
// the caller can persist an extraction_log row for observability.
type extractionOutcome struct {
	CreatedIDs []int64
	Skipped    int
	// HealthFactIDs maps persisted health-fact text → fact ID so the caller
	// can redact the plain text from extraction_log.raw_output (§10.3, MEM-8).
	HealthFactIDs map[string]int64
}

// saveProfileSuggestions stages AI-proposed profile updates for owner review
// (§4.5, MEM-4). The AI never writes contact_profiles directly: every value
// goes through contact_profile_suggestions and an explicit accept. Duplicate
// pending suggestions for the same field are dropped by the store.
func (s *Service) saveProfileSuggestions(ctx context.Context, event contracts.NormalizedTelegramEvent, suggestions []aiclient.ProfileSuggestion) {
	for _, sg := range suggestions {
		id := s.repository.SaveProfileSuggestion(store.ProfileSuggestionRecord{
			TenantID:       event.TenantID,
			ChatID:         event.ChatID,
			FieldName:      sg.FieldName,
			SuggestedValue: sg.SuggestedValue,
			Rationale:      sg.Rationale,
		})
		if id == 0 {
			continue // invalid field or duplicate pending suggestion
		}
		s.logger.Info("profile suggestion staged",
			"tenant_id", event.TenantID, "chat_id", event.ChatID,
			"suggestion_id", id, "field", sg.FieldName)
		if s.notify != nil {
			s.notify(event.TenantID, "profile_suggestion", map[string]any{
				"chat_id":         event.ChatID,
				"suggestion_id":   id,
				"field_name":      sg.FieldName,
				"suggested_value": sg.SuggestedValue,
			})
			continue
		}
		ownerChatID := s.ownerChatIDForEvent(event)
		if ownerChatID == "" {
			continue
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID:    event.TenantID,
			ChatID:      ownerChatID,
			Text:        bot.SuggestionCardText(event.ChatDisplayName, bot.SuggestionFieldLabels[sg.FieldName], sg.SuggestedValue, sg.Rationale),
			ReplyMarkup: bot.SuggestionKeyboard(id),
		})
	}
}

// classifyQuarantine reports whether text is an exact duplicate of one of the
// last ~5 contact messages in the chat. Normalisation: lower, collapse whitespace.
// Cheap and local — no model call. Task ③ adds the rate-limit layer on top.
func classifyQuarantine(text string, recent []store.MessageRecord) bool {
	norm := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	if norm == "" {
		return false
	}
	for _, m := range recent {
		if strings.Join(strings.Fields(strings.ToLower(m.Event.MessageText)), " ") == norm {
			return true
		}
	}
	return false
}

// shouldExtractFacts is the cheap pre-filter before the extraction LLM call
// (MEM-13): short acknowledgements («ок», «да», «спасибо») cannot contain a
// durable fact, yet production extraction_log shows they dominate the calls
// and almost always return []. Messages are kept when they are long enough OR
// contain a digit/capitalised name-like token (dates, sums, identifiers).
func shouldExtractFacts(text string) bool {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) >= 15 {
		return true
	}
	for i, r := range runes {
		if r >= '0' && r <= '9' {
			return true
		}
		// A capitalised token after the start hints at a proper name.
		if i > 0 && runes[i-1] == ' ' && r != ' ' && strings.ToLower(string(r)) != string(r) {
			return true
		}
	}
	return false
}

// redactHealthFacts replaces health-fact texts inside the raw LLM output with
// `[health fact #id]` markers before the output is persisted to extraction_log
// (§10.3, MEM-8). Health is the only redacted type: taboo facts are owner-set
// conversation boundaries ("не обсуждать цену") whose text is diagnostically
// useful and not medically sensitive — documented decision per the backlog.
// Unsaved health candidates are redacted without an ID.
func redactHealthFacts(raw string, facts []aiclient.TypedFact, healthIDs map[string]int64) string {
	for _, f := range facts {
		if f.FactType != "health" {
			continue
		}
		text := strings.TrimSpace(f.FactText)
		if text == "" {
			continue
		}
		marker := "[health fact]"
		if id, ok := healthIDs[text]; ok && id > 0 {
			marker = fmt.Sprintf("[health fact #%d]", id)
		}
		raw = strings.ReplaceAll(raw, text, marker)
	}
	return raw
}

func (s *Service) saveTypedFacts(event contracts.NormalizedTelegramEvent, facts []aiclient.TypedFact) extractionOutcome {
	outcome := extractionOutcome{}
	if len(facts) == 0 || s.repository == nil {
		return outcome
	}
	valid := make([]aiclient.TypedFact, 0, len(facts))
	texts := make([]string, 0, len(facts))
	for _, f := range facts {
		text := strings.TrimSpace(f.FactText)
		if text == "" || len(text) < 5 || len(text) > 200 {
			outcome.Skipped++
			continue
		}
		if isContextContamination(text) {
			s.logger.Warn("saveTypedFacts: rejected context-contaminated string",
				"tenant_id", event.TenantID, "fact_preview", truncateForLog(text, 80))
			outcome.Skipped++
			continue
		}
		f.FactText = text
		valid = append(valid, f)
		texts = append(texts, text)
	}
	// Candidate embeddings power write-time semantic dedup (MEM-1). On embed
	// failure dedup degrades to hash-only; the async worker embeds later.
	embeddings := s.embedCandidates(texts)
	for i, f := range valid {
		result := s.repository.UpsertSemanticFact(store.SemanticFactRecord{
			TenantID:        event.TenantID,
			ScopeKind:       "chat",
			ChatID:          event.ChatID,
			FactText:        f.FactText,
			FactType:        f.FactType,
			Subject:         f.Subject,
			Confidence:      f.Confidence,
			Source:          "llm_extracted",
			SourceMessageID: event.SourceMessageID,
		}, embeddingAt(embeddings, i))
		if result.FactID != 0 {
			outcome.CreatedIDs = append(outcome.CreatedIDs, result.FactID)
			if f.FactType == "health" {
				if outcome.HealthFactIDs == nil {
					outcome.HealthFactIDs = make(map[string]int64)
				}
				outcome.HealthFactIDs[f.FactText] = result.FactID
			}
			// Temporal knowledge graph (v4): a supersede creates a directed edge so
			// the graph read-path can resolve "current truth" across time.
			if result.SupersededFactID > 0 {
				s.repository.SaveFactRelation(store.FactRelationRecord{
					TenantID:     event.TenantID,
					FactIDFrom:   result.SupersededFactID,
					FactIDTo:     result.FactID,
					RelationType: "supersedes",
					Confidence:   f.Confidence,
				})
			}
		}
	}
	if len(outcome.CreatedIDs) > 0 {
		s.nudgeEmbeddingWorker()
	}
	return outcome
}

func isOwnerAuthoredMessage(event contracts.NormalizedTelegramEvent, ownerID string) bool {
	return ownerID != "" && event.SenderID == ownerID
}

// notifyAIError pushes a real-time error notice to connected Mini App clients so
// a failed AI turn surfaces as a retryable message rather than the thinking
// indicator disappearing with nothing in its place.
func (s *Service) notifyAIError(event contracts.NormalizedTelegramEvent) {
	if s.notify == nil {
		return
	}
	s.notify(event.TenantID, "ai_error", map[string]any{
		"chat_id": event.ChatID,
		"message": "ИИ не смог сформировать ответ. Попробуйте отправить сообщение ещё раз.",
	})
}

// isOwnerSideMessage reports whether a message belongs to the owner's side of
// the conversation — i.e. it should be treated as written on the owner's behalf
// (suppress an AI reply, label OWNER in the AI context, render "outgoing" in the
// Mini App). The rule depends on transport:
//
//   - business: the bot acts AS the owner's own Telegram account, so anything
//     the owner typed (SenderID == ownerID) is owner-side.
//   - bot: the bot is a SEPARATE identity. A human DMing the bot — including the
//     owner testing their own bot — is an incoming contact the AI must answer.
//     Only system-emitted messages (synthetic "out:" events: auto-replies,
//     approved drafts, app/manual sends) are owner-side.
func isOwnerSideMessage(event contracts.NormalizedTelegramEvent, ownerID, transport string) bool {
	if transport == contracts.TransportBot {
		return strings.HasPrefix(event.EventID, "out:")
	}
	return isOwnerAuthoredMessage(event, ownerID)
}

func anonymousChatName(chatID string) string {
	sum := sha256.Sum256([]byte(chatID))
	return fmt.Sprintf("Собеседник %x", sum[:3])
}

func (s *Service) handleBusinessConnection(ctx context.Context, update telegram.BusinessConnectionUpdate) InboundResult {
	if duplicate := s.repository.MarkReceive(update.ReceiveIdempotencyKey); duplicate {
		event := onboardingEvent(update.UserID, update.UserChatID, "business_connection:"+update.ID, update.UpdateID)
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "duplicate_business_connection"))
		s.logger.Info("duplicate business connection update ignored", "business_connection_id", update.ID, "user_id", update.UserID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_business_connection", Duplicate: true}
	}
	tenantID := tenantForUser(update.UserID)
	s.repository.SaveBusinessConnection(store.BusinessConnectionRecord{
		ConnectionID: update.ID,
		TenantID:     tenantID,
		UserID:       update.UserID,
		UserChatID:   update.UserChatID,
		CanReply:     update.CanReply,
		IsEnabled:    update.IsEnabled,
		UpdatedAt:    s.now(),
	})
	event := onboardingEvent(tenantID, update.UserChatID, "business_connection:"+update.ID, update.UpdateID)
	reason := "business_connection_enabled"
	if !update.IsEnabled {
		reason = "business_connection_disabled"
	}
	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, reason))
	s.logger.Info("telegram business connection updated", "tenant_id", tenantID, "business_connection_id", update.ID, "can_reply", update.CanReply, "is_enabled", update.IsEnabled)
	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID:        tenantID,
		ChatID:          update.UserChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      event.EventID,
		Text:            bot.BusinessConnectionText(update),
	})
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: reason}
}

// Sentinel errors returned by SummarizeChat so both the bot command and the
// Mini App endpoint can map the same failure to their own user-facing message.
var (
	// ErrSummaryBlockedByPrivacy means the chat carries the no_summary flag.
	ErrSummaryBlockedByPrivacy = errors.New("summary blocked by no_summary privacy flag")
	// ErrSummaryNoMessages means the chat has no text messages to compress.
	ErrSummaryNoMessages = errors.New("no messages to summarize")
)

// SummarizeChat builds an on-demand summary of the last 50 messages of a chat.
// The no_summary privacy contract is enforced here so every caller honours it.
// Returns the message count alongside the response so callers can render a
// fallback ("N сообщений, сводка недоступна") when the AI service fails.
func (s *Service) SummarizeChat(ctx context.Context, tenantID, chatID string) (contracts.ChatSummaryResponse, int, error) {
	settings := s.repository.ChatSettings(tenantID, chatID)
	if settings.PrivacyFlags.Contains(contracts.PrivacyNoSummary) {
		return contracts.ChatSummaryResponse{}, 0, ErrSummaryBlockedByPrivacy
	}
	messages := s.repository.RecentMessages(tenantID, chatID, 50)
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		if t := strings.TrimSpace(m.Event.MessageText); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return contracts.ChatSummaryResponse{}, 0, ErrSummaryNoMessages
	}

	// Serve a cached summary when the timeline is unchanged since it was made.
	cacheKey := tenantID + ":" + chatID
	marker := summaryMarker(messages)
	if v, ok := s.summaryCache.Load(cacheKey); ok {
		if entry := v.(summaryCacheEntry); entry.marker == marker {
			return entry.resp, entry.msgCount, nil
		}
	}

	summarizer, ok := s.ai.(aiclient.ChatSummarizer)
	if !ok {
		return contracts.ChatSummaryResponse{}, len(texts), fmt.Errorf("summarizer not available")
	}
	summ, err := summarizer.SummarizeChat(ctx, contracts.ChatSummaryRequest{
		Messages:     texts,
		PrivacyFlags: settings.PrivacyFlags,
	})
	if err == nil && summ.Summary != "" {
		s.summaryCache.Store(cacheKey, summaryCacheEntry{marker: marker, resp: summ, msgCount: len(texts)})
	}
	return summ, len(texts), err
}

func (s *Service) handleSummaryCommand(ctx context.Context, command telegram.BotCommandUpdate, tenantID string, event contracts.NormalizedTelegramEvent) InboundResult {
	selector := strings.TrimSpace(command.Arguments)
	if selector == "" {
		selector = "1"
	}
	chat, ok := s.repository.ChatBySelector(tenantID, selector)
	if !ok {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Чат не найден. Используй /chats чтобы увидеть список.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_chat_not_found"}
	}

	summ, msgCount, err := s.SummarizeChat(ctx, tenantID, chat.ChatID)
	if errors.Is(err, ErrSummaryBlockedByPrivacy) {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Для этого чата отключены сводки (флаг no_summary).",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_blocked_by_privacy"}
	}
	if errors.Is(err, ErrSummaryNoMessages) {
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   command.ChatID,
			Text:     "Нет сообщений для суммаризации в этом чате.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_no_messages"}
	}

	replyText := ""
	if err != nil || summ.Summary == "" {
		replyText = fmt.Sprintf("📊 Чат «%s»: %d сообщений. Сводка недоступна.", chatDisplayName(chat), msgCount)
	} else {
		replyText = fmt.Sprintf("📊 Сводка чата «%s»:\n\n%s", chatDisplayName(chat), summ.Summary)
		if len(summ.ActionItems) > 0 {
			replyText += "\n\n📋 Действия:\n"
			for _, item := range summ.ActionItems {
				replyText += "• " + item + "\n"
			}
		}
	}

	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID: tenantID,
		ChatID:   command.ChatID,
		Text:     replyText,
	})
	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, "summary_command"))
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "summary_command"}
}

func chatDisplayName(chat store.ChatRecord) string {
	if chat.DisplayName != "" {
		return chat.DisplayName
	}
	return "chat " + chat.ChatID
}

func (s *Service) handleBotCommand(ctx context.Context, command telegram.BotCommandUpdate) InboundResult {
	tenantID := tenantForUser(command.SenderID)
	event := onboardingEvent(tenantID, command.ChatID, command.MessageID, command.UpdateID)
	if duplicate := s.repository.MarkReceive(command.ReceiveIdempotencyKey); duplicate {
		s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionIgnore, "duplicate_bot_command"))
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_bot_command", Duplicate: true}
	}

	response := s.botLayer.HandleCommand(command, tenantID)

	if response.Reason == "summary_needs_ai" {
		return s.handleSummaryCommand(ctx, command, tenantID, event)
	}

	s.repository.AppendAudit(s.auditEvent(event, contracts.DecisionObserve, response.Reason))
	s.logger.Info("telegram bot command handled",
		"tenant_id", tenantID,
		"chat_id", command.ChatID,
		"command", command.Command,
		"reason", response.Reason,
		"visible_chats_count", len(s.repository.ChatsForTenant(tenantID)),
		"response_preview", truncateForLog(response.Text, 1000),
	)
	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID:        tenantID,
		ChatID:          command.ChatID,
		SourceMessageID: command.MessageID,
		DecisionID:      event.EventID,
		Text:            response.Text,
		ReplyMarkup:     response.ReplyMarkup,
	})
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: response.Reason}
}

// HandleApprovalCallback applies an owner approval callback.
func (s *Service) HandleApprovalCallback(ctx context.Context, payload contracts.ApprovalCallbackPayload) (ApprovalResult, error) {
	if payload.IdempotencyKey == "" {
		return ApprovalResult{}, fmt.Errorf("missing idempotency_key")
	}
	if !payload.Action.IsValid() {
		return ApprovalResult{}, fmt.Errorf("invalid action %q", payload.Action)
	}
	if duplicate := s.repository.MarkCallback(payload.IdempotencyKey); duplicate {
		return ApprovalResult{Action: payload.Action, Duplicate: true, DraftID: payload.DraftID, Status: "duplicate"}, nil
	}
	if payload.Action == contracts.ApprovalNoMemory {
		settings := s.repository.ChatSettings(payload.TenantID, payload.ChatID)
		if settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) {
			settings.PrivacyFlags = settings.PrivacyFlags.Remove(contracts.PrivacyNoMemory)
			s.repository.SaveChatSettings(settings)
			return ApprovalResult{Action: payload.Action, Status: "no_memory_disabled"}, nil
		}
		settings.PrivacyFlags = settings.PrivacyFlags.Add(contracts.PrivacyNoMemory)
		s.repository.SaveChatSettings(settings)
		return ApprovalResult{Action: payload.Action, Status: "no_memory_enabled"}, nil
	}

	draft, ok := s.repository.Draft(payload.DraftID)
	if !ok {
		return ApprovalResult{}, fmt.Errorf("draft not found")
	}
	// Warning cards carry an owner-facing service note, not a reply. Every
	// mutating/sending action is blocked so no callback path can deliver the
	// note (or an edit of it) to the contact — only dismissal works (ARCH-1).
	if draft.DraftType == "warning" && payload.Action != contracts.ApprovalReject {
		return ApprovalResult{}, fmt.Errorf("warning card supports only reject")
	}
	now := s.now()
	switch payload.Action {
	case contracts.ApprovalApprove:
		if strings.TrimSpace(draft.ResponseText) == "" {
			return ApprovalResult{}, fmt.Errorf("draft has no sendable text")
		}
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateApprovedByOwner)
		approvalEvent := contracts.NormalizedTelegramEvent{
			TenantID:             draft.TenantID,
			ChatID:               draft.ChatID,
			BusinessConnectionID: draft.BusinessConnectionID,
			SourceMessageID:      draft.SourceMessageID,
		}
		sentMsgID, err := s.sendRecipient(ctx, approvalEvent, draft.DecisionID, draft.ResponseText, draft.TextFormat, false, s.deliverySettingsFor(approvalEvent))
		if err != nil {
			s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
			return ApprovalResult{}, err
		}
		draft.Status = store.DraftSent
		draft.SentMessageID = sentMsgID
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.saveOutgoingMessageFromDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateSentToRecipient)
		// The approved reply answers the open question; owner action completed (MEM-5).
		s.repository.ClearEpisodeRuntimeState(draft.TenantID, draft.ChatID)
		if s.notify != nil {
			s.notify(draft.TenantID, "new_message", map[string]any{"chat_id": draft.ChatID})
		}
		// Sending = implicit positive feedback: the owner approved this exact text.
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "up",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "up", "")
		}
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalReject:
		draft.Status = store.DraftRejected
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateIgnored)
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
		}
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalEdit:
		if strings.TrimSpace(payload.EditedText) == "" {
			return ApprovalResult{}, fmt.Errorf("edited_text is required for edit action")
		}
		// Edit-delta capture: the original AI text the owner chose to rewrite is the
		// single richest negative signal. Record it once — on the FIRST edit, before
		// ResponseText is overwritten — as a "down". The eventual approve records the
		// sent text as "up", yielding a clean original→final pair under the
		// UNIQUE(draft_id, rating) constraint that future drafts learn from.
		alreadyEdited := false
		for _, a := range draft.ActionHistory {
			if a == "edit" {
				alreadyEdited = true
				break
			}
		}
		if !alreadyEdited {
			s.repository.SaveDraftFeedback(contracts.DraftFeedback{
				DraftID:      draft.DraftID,
				TenantID:     draft.TenantID,
				ChatID:       draft.ChatID,
				Rating:       "down",
				ResponseText: draft.ResponseText,
				CreatedAt:    now,
			})
			if s.scoreSender != nil {
				s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
			}
		}
		draft.ResponseText = payload.EditedText
		draft.ActionHistory = append(draft.ActionHistory, "edit")
		draft.UpdatedAt = now
		s.repository.UpdateDraft(draft)
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalRegenerate:
		draft.ActionHistory = append(draft.ActionHistory, "regenerate")
		s.repository.UpdateDraft(draft)
		updated, err := s.regenerateDraft(ctx, draft)
		if err != nil {
			return ApprovalResult{}, err
		}
		return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
	case contracts.ApprovalMakeShorter:
		draft.ActionHistory = append(draft.ActionHistory, "shorter")
		s.repository.UpdateDraft(draft)
		updated := s.reviseDraft(ctx, draft, payload.Action)
		return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
	case contracts.ApprovalMakeMoreFormal:
		draft.ActionHistory = append(draft.ActionHistory, "formal")
		s.repository.UpdateDraft(draft)
		updated := s.reviseDraft(ctx, draft, payload.Action)
		return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
	case contracts.ApprovalAIRefine:
		if strings.TrimSpace(payload.EditedText) != "" {
			// Mini App flow: owner supplied a refinement hint inline.
			updated, err := s.regenerateDraftWithHint(ctx, draft, payload.EditedText)
			if err != nil {
				return ApprovalResult{}, err
			}
			s.notifyDraftStatus(updated)
			return ApprovalResult{Action: payload.Action, DraftID: updated.DraftID, Status: string(updated.Status)}, nil
		}
		// Bot flow: no-op, pending input handled by handleCallbackQuery.
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalEditRequest:
		// Pending input is set in handleCallbackQuery — no-op here.
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: string(draft.Status)}, nil
	case contracts.ApprovalThumbUp:
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "up",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "up", "")
		}
		s.stripRatingButtons(ctx, draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: "feedback_saved"}, nil
	case contracts.ApprovalThumbDown:
		s.repository.SaveDraftFeedback(contracts.DraftFeedback{
			DraftID:      draft.DraftID,
			TenantID:     draft.TenantID,
			ChatID:       draft.ChatID,
			Rating:       "down",
			ResponseText: draft.ResponseText,
			CreatedAt:    now,
		})
		if s.scoreSender != nil {
			s.scoreSender.SendOwnerFeedback(ctx, draft.TraceID, "down", "")
		}
		s.stripRatingButtons(ctx, draft)
		return ApprovalResult{Action: payload.Action, DraftID: draft.DraftID, Status: "feedback_saved"}, nil
	default:
		return ApprovalResult{}, fmt.Errorf("unsupported action %q", payload.Action)
	}
}

// removeDraftCard deletes the draft card from the owner's bot chat after it's been acted upon.
func (s *Service) removeDraftCard(ctx context.Context, draft store.DraftRecord) {
	if draft.OwnerChatID == "" || draft.OwnerMessageID == "" {
		return
	}
	_ = s.sender.DeleteMessage(ctx, draft.OwnerChatID, draft.OwnerMessageID)
}

// stripRatingButtons edits the draft card to remove 👍👎 buttons after the owner has rated it.
func (s *Service) stripRatingButtons(ctx context.Context, draft store.DraftRecord) {
	if draft.OwnerChatID == "" || draft.OwnerMessageID == "" {
		return
	}
	keyboard := bot.DraftApprovalKeyboardNoRating(draft.DraftID, s.draftNoMemory(draft))
	_ = s.sender.EditMessageReplyMarkup(ctx, draft.OwnerChatID, draft.OwnerMessageID, keyboard)
}

// draftNoMemory reports whether the no_memory flag is active for the draft's chat.
func (s *Service) draftNoMemory(draft store.DraftRecord) bool {
	settings := s.repository.ChatSettings(draft.TenantID, draft.ChatID)
	return settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory)
}

// Repository returns the service repository.
func (s *Service) Repository() *store.Repository {
	return s.repository
}

func (s *Service) regenerateDraft(ctx context.Context, draft store.DraftRecord) (store.DraftRecord, error) {
	event := s.resolveSourceEvent(draft)
	settings := s.repository.ChatSettings(draft.TenantID, draft.ChatID)
	ownerID := ""
	if connection, ok := s.repository.BusinessConnection(draft.BusinessConnectionID); ok {
		ownerID = connection.UserID
	}
	regenRequest := contracts.AIProcessMessageRequest{
		RequestID:     draft.DecisionID + ":regenerate",
		TraceID:       tracepkg.DeriveTraceID(event.EventID),
		Event:         event,
		ChatSettings:  settings,
		MemoryContext: s.conversationContext(ctx, event, ownerID, 20),
	}
	if profile, ok := s.repository.ContactProfile(draft.TenantID, draft.ChatID); ok {
		regenRequest.ContactProfile = &profile
	}
	regenRequest.AIRole = s.aiRoleFor(draft.TenantID, draft.ChatID)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIRequested)
	proposal, err := s.ai.ProcessMessage(ctx, regenRequest)
	if err != nil {
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
		return store.DraftRecord{}, err
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIGenerated)
	draft.ResponseText = proposal.ResponseText
	draft.TextFormat = proposal.TextFormat.Normalize()
	draft.UpdatedAt = s.now()
	s.repository.UpdateDraft(draft)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
	return draft, nil
}

// regenerateDraftWithHint regenerates a draft using an owner-supplied instruction hint.
// The hint is injected into the memory context so the AI applies it ("make it shorter", "be more formal", etc.).
func (s *Service) regenerateDraftWithHint(ctx context.Context, draft store.DraftRecord, hint string) (store.DraftRecord, error) {
	event := s.resolveSourceEvent(draft)
	settings := s.repository.ChatSettings(draft.TenantID, draft.ChatID)
	ownerID := ""
	if connection, ok := s.repository.BusinessConnection(draft.BusinessConnectionID); ok {
		ownerID = connection.UserID
	}
	baseContext := s.conversationContext(ctx, event, ownerID, 20)
	// Inject the owner's hint and the current draft as context so the AI knows what to change.
	hintContext := append(baseContext,
		"[CURRENT DRAFT] "+draft.ResponseText,
		"[OWNER INSTRUCTION] "+hint,
	)
	hintRequest := contracts.AIProcessMessageRequest{
		RequestID:       draft.DecisionID + ":hint",
		TraceID:         tracepkg.DeriveTraceID(event.EventID),
		Event:           event,
		ChatSettings:    settings,
		MemoryContext:   hintContext,
		FeedbackContext: s.feedbackContext(draft.TenantID, draft.ChatID, 5),
	}
	if profile, ok := s.repository.ContactProfile(draft.TenantID, draft.ChatID); ok {
		hintRequest.ContactProfile = &profile
	}
	hintRequest.AIRole = s.aiRoleFor(draft.TenantID, draft.ChatID)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIRequested)
	proposal, err := s.ai.ProcessMessage(ctx, hintRequest)
	if err != nil {
		s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateFailed)
		return store.DraftRecord{}, err
	}
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateAIGenerated)
	draft.ActionHistory = append(draft.ActionHistory, "ai_refine")
	draft.ResponseText = proposal.ResponseText
	draft.TextFormat = proposal.TextFormat.Normalize()
	draft.UpdatedAt = s.now()
	s.repository.UpdateDraft(draft)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
	return draft, nil
}

func (s *Service) reviseDraft(ctx context.Context, draft store.DraftRecord, action contracts.ApprovalAction) store.DraftRecord {
	if reviser, ok := s.ai.(aiclient.DraftReviser); ok {
		if revised, err := reviser.ReviseDraft(ctx, aiclient.DraftRevisionRequest{Action: action, Text: draft.ResponseText}); err == nil && strings.TrimSpace(revised.Text) != "" {
			draft.ResponseText = revised.Text
		}
	} else {
		draft.ResponseText = localDraftRevision(draft.ResponseText, action)
	}
	draft.UpdatedAt = s.now()
	s.repository.UpdateDraft(draft)
	s.repository.AppendState(draft.TenantID, draft.ChatID, draft.SourceMessageID, contracts.StateDraftSentToOwner)
	return draft
}

// resolveSourceEvent returns the NormalizedTelegramEvent for the draft's source message.
// It first tries the message repository (available when no_memory is off), then falls back
// to reconstructing a minimal event from the draft's own fields so that regen and hint
// operations work even when the message was never persisted (no_memory flag).
func (s *Service) resolveSourceEvent(draft store.DraftRecord) contracts.NormalizedTelegramEvent {
	if message, ok := s.repository.Message(draft.TenantID, draft.ChatID, draft.SourceMessageID); ok {
		return message.Event
	}
	return contracts.NormalizedTelegramEvent{
		EventID:              draft.DecisionID,
		TenantID:             draft.TenantID,
		ChatID:               draft.ChatID,
		BusinessConnectionID: draft.BusinessConnectionID,
		SourceMessageID:      draft.SourceMessageID,
		MessageText:          draft.SourceMessageText,
		MessageType:          "text",
		CreatedAt:            draft.CreatedAt,
	}
}

func (s *Service) createDraft(event contracts.NormalizedTelegramEvent, decisionID, text string, textFormat contracts.TextFormat) store.DraftRecord {
	now := s.now()
	return store.DraftRecord{
		DraftID:              "draft:" + decisionID,
		DecisionID:           decisionID,
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      event.SourceMessageID,
		SourceMessageText:    event.MessageText,
		ResponseText:         text,
		TextFormat:           textFormat.Normalize(),
		Status:               store.DraftPending,
		// Persisting trace_id lets owner feedback (approve / thumb) attach a
		// Langfuse score back to the Phase 4 generation span.
		TraceID:   tracepkg.DeriveTraceID(event.EventID),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (s *Service) notifyDraftStatus(draft store.DraftRecord) {
	if s.notify == nil {
		return
	}
	s.notify(draft.TenantID, "draft_status", map[string]string{
		"draft_id": draft.DraftID,
		"chat_id":  draft.ChatID,
		"status":   string(draft.Status),
	})
}

// sendRecipient sends a reply to the contact and returns the Telegram message_id of the sent message.
// delivery holds the pre-computed rich-formatting and streaming toggles; callers on the hot path
// supply deliverySettingsFromUser (no extra repository read) while others use deliverySettingsFor.
func (s *Service) sendRecipient(ctx context.Context, event contracts.NormalizedTelegramEvent, decisionID, text string, textFormat contracts.TextFormat, allowStreaming bool, delivery deliverySettings) (string, error) {
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
		if allowStreaming && delivery.StreamingEnabled && s.richDraftStreamingAllowedFor(event) {
			return s.sendStreamingRichRecipient(ctx, sender, msg)
		}
		return s.sendRichRecipient(ctx, sender, msg)
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

func (s *Service) richOwnerDraftCardEnabled(event contracts.NormalizedTelegramEvent, draft store.DraftRecord) bool {
	return draft.TextFormat.Normalize() == contracts.TextFormatRich && s.deliverySettingsFor(event).RichFormattingEnabled
}

func (s *Service) sendOwnerCard(ctx context.Context, msg telegram.OutboundMessage, rich bool) (string, error) {
	if rich {
		if richSender, ok := s.sender.(interface {
			SendRichMessage(context.Context, telegram.OutboundMessage) (string, error)
		}); ok {
			msgID, err := richSender.SendRichMessage(ctx, msg)
			if err == nil {
				return msgID, nil
			}
			s.logger.Warn("telegram owner rich card send failed; falling back to plain",
				"tenant_id", msg.TenantID,
				"chat_id", msg.ChatID,
				"decision_id", msg.DecisionID,
				"error", err,
			)
		}
	}
	if idSender, ok := s.sender.(interface {
		SendMessageGetID(context.Context, telegram.OutboundMessage) (string, error)
	}); ok {
		return idSender.SendMessageGetID(ctx, msg)
	}
	return "", s.sender.SendMessage(ctx, msg)
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

func (s *Service) auditEvent(event contracts.NormalizedTelegramEvent, decision contracts.Decision, reason string) contracts.AuditEvent {
	return contracts.AuditEvent{
		AuditID:         fmt.Sprintf("audit:%s:%d", valueOrDefault(event.EventID, event.SourceMessageID), s.now().UnixNano()),
		TenantID:        valueOrDefault(event.TenantID, "unknown"),
		ChatID:          valueOrDefault(event.ChatID, "unknown"),
		SourceMessageID: valueOrDefault(event.SourceMessageID, "unknown"),
		Decision:        decision,
		Reason:          reason,
		// trace_id is derived from event_id so all audit/state rows produced for
		// the same webhook share a trace (idempotent on retries).
		TraceID:   tracepkg.DeriveTraceID(event.EventID),
		CreatedAt: s.now(),
	}
}

func localDraftRevision(text string, action contracts.ApprovalAction) string {
	switch action {
	case contracts.ApprovalMakeShorter:
		text = strings.TrimSpace(text)
		for _, sep := range []string{". ", "! ", "? ", "\n"} {
			if idx := strings.Index(text, sep); idx > 0 {
				return strings.TrimSpace(text[:idx+1])
			}
		}
		return text
	case contracts.ApprovalMakeMoreFormal:
		return "Thank you for your message. " + strings.TrimSpace(text)
	default:
		return text
	}
}

func onboardingEvent(tenantID, chatID, sourceMessageID string, updateID int64) contracts.NormalizedTelegramEvent {
	eventID := fmt.Sprintf("%d:onboarding:%s", updateID, sourceMessageID)
	return contracts.NormalizedTelegramEvent{
		EventID:              eventID,
		TenantID:             tenantID,
		ChatID:               chatID,
		TelegramUpdateID:     updateID,
		BusinessConnectionID: "",
		SourceMessageID:      sourceMessageID,
		SenderID:             chatID,
		MessageText:          "",
		MessageType:          "command",
		CreatedAt:            time.Now().UTC(),
	}
}

func tenantForUser(userID string) string {
	return "tenant:user:" + userID
}

// updateEpisodeRuntime persists Runtime-State-Layer signals (§6.2 / MEM-5)
// derived from the structured AI response — no extra LLM call:
//   - topic comes from the draft JSON (ARCH-4);
//   - an unanswered contact question (intent question/choice_question that did
//     not get an auto-reply) sets open_question_message_id;
//   - draft/ask_owner decisions record what the owner is expected to do.
//
// The matching reset paths: an owner-authored message and a successfully sent
// reply both call ClearEpisodeRuntimeState.
func (s *Service) updateEpisodeRuntime(event contracts.NormalizedTelegramEvent, proposal contracts.AIProcessMessageResponse, decision contracts.Decision) {
	topic := strings.TrimSpace(proposal.Topic)
	openQuestion := ""
	pendingAction := ""
	awaitingOwner := decision == contracts.DecisionDraft || decision == contracts.DecisionAskOwner
	if awaitingOwner && (proposal.Intent == "question" || proposal.Intent == "choice_question") {
		openQuestion = event.SourceMessageID
	}
	switch {
	case decision == contracts.DecisionAskOwner:
		pendingAction = "ответить вручную (критичное сообщение)"
	case decision == contracts.DecisionDraft:
		pendingAction = "подтвердить черновик ответа"
	}
	if topic == "" && openQuestion == "" && pendingAction == "" {
		return
	}
	s.repository.UpdateEpisodeRuntime(event.TenantID, event.ChatID, topic, openQuestion, pendingAction)
}

// ownerNoteFor extracts the owner-facing warning from an AI proposal.
// OwnerNote is the contract field (ARCH-1); for legacy AI services that still
// put the warning into ResponseText on critical risk, that text is treated as
// the note — it is never allowed to become a sendable draft.
func (s *Service) ownerNoteFor(proposal contracts.AIProcessMessageResponse) string {
	if note := strings.TrimSpace(proposal.OwnerNote); note != "" {
		return note
	}
	if proposal.RiskLevel == contracts.RiskCritical {
		return strings.TrimSpace(proposal.ResponseText)
	}
	return ""
}

// sendWarningToOwner delivers a critical-risk warning card to the owner's bot chat.
// The card has no Approve/Edit controls — the owner can only dismiss it and reply
// manually from their own Telegram (ARCH-1).
func (s *Service) sendWarningToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerNote string) {
	ownerChatID := s.ownerChatIDForEvent(event)
	if ownerChatID == "" {
		s.logger.Warn("cannot deliver warning card to owner: chat ID not found",
			"business_connection_id", event.BusinessConnectionID, "draft_id", draft.DraftID)
		return
	}
	msg := telegram.OutboundMessage{
		TenantID:        event.TenantID,
		ChatID:          ownerChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      draft.DecisionID,
		Text:            bot.WarningCardText(event.MessageText, ownerNote, event.ChatDisplayName),
		ReplyMarkup:     bot.WarningKeyboard(draft.DraftID),
	}
	msgID, err := s.sendOwnerCard(ctx, msg, false)
	if err != nil {
		s.logger.Warn("failed to deliver warning card to owner", "tenant_id", event.TenantID, "draft_id", draft.DraftID, "error", err)
		return
	}
	if msgID != "" {
		draft.OwnerChatID = ownerChatID
		draft.OwnerMessageID = msgID
		s.repository.UpdateDraft(draft)
	}
}

// ownerChatIDForEvent resolves the owner's control-bot chat for any transport.
// Bot-transport tenants are keyed "tenant:user:<id>" where <id> is the owner's
// Telegram user id (which equals their DM chat with the control bot); business
// transport keeps the connection-based lookup with its fallbacks.
func (s *Service) ownerChatIDForEvent(event contracts.NormalizedTelegramEvent) string {
	if event.Transport == contracts.TransportBot || event.BusinessConnectionID == "" {
		if owner := strings.TrimPrefix(event.TenantID, "tenant:user:"); owner != event.TenantID {
			return owner
		}
		if event.BusinessConnectionID == "" {
			return ""
		}
	}
	return s.ownerChatIDFor(event.BusinessConnectionID)
}

// ownerChatIDFor resolves the owner's bot chat for a business connection,
// falling back to inference from claimed chats (shared by draft/question/warning cards).
func (s *Service) ownerChatIDFor(businessConnectionID string) string {
	if connection, ok := s.repository.BusinessConnection(businessConnectionID); ok && connection.UserChatID != "" {
		return connection.UserChatID
	}
	ownerChatID := s.inferOwnerChatID(businessConnectionID)
	if ownerChatID != "" {
		s.repository.SaveBusinessConnection(store.BusinessConnectionRecord{
			ConnectionID: businessConnectionID,
			TenantID:     "tenant:user:" + ownerChatID,
			UserID:       ownerChatID,
			UserChatID:   ownerChatID,
			CanReply:     true,
			IsEnabled:    true,
			UpdatedAt:    s.now(),
		})
	}
	return ownerChatID
}

// sendDraftToOwner delivers a draft card with approval keyboard to the owner's bot chat.
// Stores the Telegram message_id of the card so it can be deleted/edited later.
func (s *Service) sendDraftToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerID string) {
	ownerChatID := s.ownerChatIDForEvent(event)
	if ownerChatID == "" {
		s.logger.Warn("cannot deliver draft to owner: business connection not found and no fallback available",
			"business_connection_id", event.BusinessConnectionID,
			"draft_id", draft.DraftID,
			"hint", "reconnect the bot via Telegram Settings → Chat Automation",
		)
		return
	}
	cardText := bot.DraftCardText(event.MessageText, draft.ResponseText, event.ChatDisplayName)
	keyboard := bot.DraftApprovalKeyboard(draft.DraftID, s.draftNoMemory(draft))
	msg := telegram.OutboundMessage{
		TenantID:        event.TenantID,
		ChatID:          ownerChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      draft.DecisionID,
		Text:            cardText,
		ReplyMarkup:     keyboard,
	}
	msgID, err := s.sendOwnerCard(ctx, msg, s.richOwnerDraftCardEnabled(event, draft))
	if err != nil {
		s.logger.Warn("failed to deliver draft card to owner", "tenant_id", event.TenantID, "draft_id", draft.DraftID, "error", err)
		return
	}
	if msgID != "" {
		draft.OwnerChatID = ownerChatID
		draft.OwnerMessageID = msgID
		s.repository.UpdateDraft(draft)
	}
}

// inferOwnerChatID finds the owner's bot chat ID for a business connection
// by looking at chats that have been claimed by a tenant:user:* owner.
func (s *Service) inferOwnerChatID(businessConnectionID string) string {
	tenantID := s.repository.OwnerTenantForBusinessConnection(businessConnectionID)
	if tenantID == "" {
		return ""
	}
	userID := strings.TrimPrefix(tenantID, "tenant:user:")
	if userID == tenantID {
		return "" // not the expected "tenant:user:USERID" format
	}
	return userID
}

// handleCallbackQuery processes an inline button press from the owner.
func (s *Service) handleCallbackQuery(ctx context.Context, cbq telegram.CallbackQueryUpdate) InboundResult {
	tenantID := tenantForUser(cbq.SenderID)
	event := onboardingEvent(tenantID, cbq.ChatID, cbq.OriginalMsgID, cbq.UpdateID)

	if duplicate := s.repository.MarkReceive(cbq.ReceiveIdempotencyKey); duplicate {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "duplicate_callback_query", Duplicate: true}
	}

	// Handle question-card option selection (format "co:N:bareID") before the regular decode.
	if optionIdx, draftID, idempotencyKey, choiceOK := contracts.CallbackDataDecodeChoice(cbq.Data); choiceOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return s.handleChoiceOption(ctx, event, tenantID, cbq, optionIdx, draftID, idempotencyKey)
	}

	// Forget-chat confirmation flow (§10.2, MEM-2).
	if forgetChatID, forgetOK := contracts.CallbackDataDecodeForget(cbq.Data); forgetOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		text := "🗑 Память чата удалена безвозвратно."
		if err := s.repository.ForgetChat(tenantID, forgetChatID); err != nil {
			s.logger.Error("forget chat failed", "tenant_id", tenantID, "chat_id", forgetChatID, "error", err)
			text = "Не удалось удалить память чата: " + err.Error()
		} else {
			s.logger.Info("chat memory forgotten", "tenant_id", tenantID, "chat_id", forgetChatID)
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: cbq.ChatID, Text: text})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_chat_confirmed"}
	}
	if cbq.Data == contracts.ForgetCancelCallbackData {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: cbq.ChatID, Text: "Удаление отменено."})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_chat_cancelled"}
	}

	// Profile-suggestion review (§4.5, MEM-4).
	if suggestionID, accept, suggestionOK := contracts.CallbackDataDecodeSuggestion(cbq.Data); suggestionOK {
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		text := "✅ Предложение принято — профиль обновлён."
		if !accept {
			text = "Предложение отклонено."
		}
		if err := s.repository.ResolveSuggestion(tenantID, suggestionID, accept); err != nil {
			s.logger.Warn("suggestion resolution failed", "tenant_id", tenantID, "suggestion_id", suggestionID, "error", err)
			text = "Предложение уже обработано или не найдено."
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: cbq.ChatID, Text: text})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "suggestion_resolved"}
	}

	action, draftID, idempotencyKey, ok := contracts.CallbackDataDecode(cbq.Data)
	if !ok {
		s.logger.Warn("callback query: unrecognised callback data", "data", cbq.Data)
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		return InboundResult{Event: event, Decision: contracts.DecisionIgnore, Reason: "unknown_callback_data"}
	}

	draft, hasDraft := s.repository.Draft(draftID)
	chatID := cbq.ChatID
	if hasDraft {
		// Use the draft's chat/tenant as canonical source of truth.
		event.TenantID = draft.TenantID
		event.ChatID = draft.ChatID
		chatID = draft.ChatID
		tenantID = draft.TenantID
	}

	payload := contracts.ApprovalCallbackPayload{
		Action:         action,
		TenantID:       tenantID,
		ChatID:         chatID,
		DraftID:        draftID,
		IdempotencyKey: idempotencyKey,
	}
	if hasDraft {
		payload.SourceMessageID = draft.SourceMessageID
		payload.DecisionID = draft.DecisionID
	}

	// edit_request and ai_refine bypass idempotency — owner can always re-open these prompts.
	switch action {
	case contracts.ApprovalEditRequest:
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		s.repository.SetPendingInput(tenantID, cbq.ChatID, contracts.PendingInput{
			TenantID:  tenantID,
			ChatID:    cbq.ChatID,
			Type:      contracts.PendingInputEdit,
			DraftID:   draftID,
			ExpiresAt: s.now().Add(5 * 60 * 1e9),
		})
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   cbq.ChatID,
			Text:     bot.DraftEditPromptText(),
		})
		s.logger.Info("callback query handled", "action", action, "draft_id", draftID, "status", "edit_prompt_sent")
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "callback_edit_request", DraftID: draftID}

	case contracts.ApprovalAIRefine:
		_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
		s.repository.SetPendingInput(tenantID, cbq.ChatID, contracts.PendingInput{
			TenantID:  tenantID,
			ChatID:    cbq.ChatID,
			Type:      contracts.PendingInputAIRefine,
			DraftID:   draftID,
			ExpiresAt: s.now().Add(5 * 60 * 1e9),
		})
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   cbq.ChatID,
			Text:     bot.AIRefinePromptText(),
		})
		s.logger.Info("callback query handled", "action", action, "draft_id", draftID, "status", "ai_refine_prompt_sent")
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "callback_ai_refine", DraftID: draftID}
	}

	result, err := s.HandleApprovalCallback(ctx, payload)
	_ = s.sender.AnswerCallbackQuery(ctx, cbq.QueryID)
	if err != nil {
		s.logger.Error("approval callback failed", "error", err, "draft_id", draftID, "action", action)
		return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "callback_approval_error:" + err.Error()}
	}

	// After thumb_down or reject: offer text feedback (only on first press, not duplicates).
	if !result.Duplicate {
		switch action {
		case contracts.ApprovalThumbDown, contracts.ApprovalReject:
			s.repository.SetPendingInput(tenantID, cbq.ChatID, contracts.PendingInput{
				TenantID:  tenantID,
				ChatID:    cbq.ChatID,
				Type:      contracts.PendingInputFeedback,
				DraftID:   draftID,
				ExpiresAt: s.now().Add(5 * 60 * 1e9),
			})
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   cbq.ChatID,
				Text:     bot.DraftFeedbackPromptText(),
			})
		case contracts.ApprovalNoMemory:
			noMemory := result.Status == "no_memory_enabled"
			// Update the draft card keyboard so the memory button reflects the new state.
			if draft, ok := s.repository.Draft(draftID); ok && draft.OwnerChatID != "" && draft.OwnerMessageID != "" {
				keyboard := bot.DraftApprovalKeyboard(draft.DraftID, noMemory)
				_ = s.sender.EditMessageReplyMarkup(ctx, draft.OwnerChatID, draft.OwnerMessageID, keyboard)
			}
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   cbq.ChatID,
				Text:     bot.NoMemoryStatusText(noMemory),
			})
		}
	}

	s.logger.Info("callback query handled", "action", result.Action, "draft_id", result.DraftID, "status", result.Status)
	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "callback_" + string(result.Action), DraftID: result.DraftID}
}

// handleOwnerMessage handles plain (non-command) text from the owner in the bot chat.
// Used for pending-input flows: editing a draft or providing text feedback.
func (s *Service) handleOwnerMessage(ctx context.Context, msg telegram.OwnerMessageUpdate) InboundResult {
	tenantID := tenantForUser(msg.SenderID)
	event := onboardingEvent(tenantID, msg.ChatID, msg.MessageID, msg.UpdateID)

	pending, hasPending := s.repository.PendingInput(tenantID, msg.ChatID)
	if !hasPending {
		// No pending input state — treat as unknown, show help.
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   msg.ChatID,
			Text:     "Используй /help для списка команд.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_no_pending_input"}
	}

	s.repository.ClearPendingInput(tenantID, msg.ChatID)

	switch pending.Type {
	case contracts.PendingInputForgetAll:
		// §10.2: full erasure runs only on the exact confirmation phrase.
		if strings.TrimSpace(msg.Text) != contracts.ForgetAllConfirmationPhrase {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Фраза не совпала — удаление отменено.",
			})
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_all_cancelled"}
		}
		if err := s.repository.ForgetAll(tenantID); err != nil {
			s.logger.Error("forget all failed", "tenant_id", tenantID, "error", err)
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Не удалось удалить данные: " + err.Error(),
			})
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "forget_all_failed"}
		}
		s.logger.Info("tenant memory forgotten", "tenant_id", tenantID)
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   msg.ChatID,
			Text:     "🗑 Все данные удалены безвозвратно. Подключи бота заново, чтобы начать с чистого листа.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "forget_all_confirmed"}

	case contracts.PendingInputFeedback:
		if msg.Text != "/skip" {
			s.repository.SaveDraftFeedback(contracts.DraftFeedback{
				DraftID:      pending.DraftID,
				TenantID:     tenantID,
				ChatID:       msg.ChatID,
				Rating:       "down",
				FeedbackText: msg.Text,
				CreatedAt:    s.now(),
			})
		}
		_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
			TenantID: tenantID,
			ChatID:   msg.ChatID,
			Text:     "Спасибо, учту в следующий раз.",
		})
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_feedback_saved"}

	case contracts.PendingInputEdit:
		// Direct text replacement — exactly what the owner typed becomes the new draft.
		draft, ok := s.repository.Draft(pending.DraftID)
		if !ok {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Черновик не найден.",
			})
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_edit_draft_not_found"}
		}
		_, err := s.HandleApprovalCallback(ctx, contracts.ApprovalCallbackPayload{
			Action:          contracts.ApprovalEdit,
			TenantID:        draft.TenantID,
			ChatID:          draft.ChatID,
			SourceMessageID: draft.SourceMessageID,
			DraftID:         draft.DraftID,
			DecisionID:      draft.DecisionID,
			IdempotencyKey:  draft.DraftID + ":edit:" + msg.MessageID,
			EditedText:      msg.Text,
		})
		if err != nil {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Не удалось обновить черновик: " + err.Error(),
			})
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "owner_edit_failed"}
		}
		updatedDraft, _ := s.repository.Draft(pending.DraftID)
		keyboard := bot.DraftApprovalKeyboard(updatedDraft.DraftID, s.draftNoMemory(updatedDraft))
		cardText := bot.DraftCardText(updatedDraft.SourceMessageText, updatedDraft.ResponseText, anonymousChatName(updatedDraft.ChatID))
		if updatedDraft.OwnerChatID != "" && updatedDraft.OwnerMessageID != "" {
			_ = s.sender.EditMessageText(ctx, updatedDraft.OwnerChatID, updatedDraft.OwnerMessageID, cardText, keyboard)
		} else {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: msg.ChatID, Text: cardText, ReplyMarkup: keyboard})
		}
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_edit_applied"}

	case contracts.PendingInputAIRefine:
		// AI refinement — the owner's text is an instruction for the AI to improve the current draft.
		draft, ok := s.repository.Draft(pending.DraftID)
		if !ok {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "Черновик не найден.",
			})
			return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_ai_refine_draft_not_found"}
		}
		updatedDraft, err := s.regenerateDraftWithHint(ctx, draft, msg.Text)
		if err != nil {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
				TenantID: tenantID,
				ChatID:   msg.ChatID,
				Text:     "ИИ не смог применить правку: " + err.Error(),
			})
			return InboundResult{Event: event, Decision: contracts.DecisionErrorFallback, Reason: "owner_ai_refine_failed"}
		}
		keyboard := bot.DraftApprovalKeyboard(updatedDraft.DraftID, s.draftNoMemory(updatedDraft))
		cardText := bot.DraftCardText(updatedDraft.SourceMessageText, updatedDraft.ResponseText, anonymousChatName(updatedDraft.ChatID))
		if updatedDraft.OwnerChatID != "" && updatedDraft.OwnerMessageID != "" {
			_ = s.sender.EditMessageText(ctx, updatedDraft.OwnerChatID, updatedDraft.OwnerMessageID, cardText, keyboard)
		} else {
			_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{TenantID: tenantID, ChatID: msg.ChatID, Text: cardText, ReplyMarkup: keyboard})
		}
		return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_ai_refine_applied"}
	}

	return InboundResult{Event: event, Decision: contracts.DecisionObserve, Reason: "owner_message_handled"}
}

// outgoingUpdateID derives a unique negative int64 from a decisionID for synthetic outgoing
// messages. Negative values never collide with real Telegram update IDs (always positive).
func outgoingUpdateID(decisionID string) int64 {
	h := sha256.Sum256([]byte(decisionID))
	// Take first 8 bytes as uint64, shift to stay in int64 range, then negate.
	v := int64(uint64(h[0])<<56 | uint64(h[1])<<48 | uint64(h[2])<<40 | uint64(h[3])<<32 |
		uint64(h[4])<<24 | uint64(h[5])<<16 | uint64(h[6])<<8 | uint64(h[7]))
	if v == 0 {
		v = -1
	}
	if v > 0 {
		v = -v
	}
	return v
}

// saveOutgoingMessage persists a synthetic outgoing message and a virtual sent draft so the
// Mini App can display the AI auto-reply in chat history and label it as "DoubleU".
func (s *Service) saveOutgoingMessage(event contracts.NormalizedTelegramEvent, ownerID, sentMsgID, text, decisionID string, textFormat contracts.TextFormat) {
	if ownerID == "" || sentMsgID == "" {
		return
	}
	s.repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:              "out:" + decisionID,
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      sentMsgID,
		SenderID:             ownerID,
		MessageText:          text,
		MessageType:          "text",
		TelegramUpdateID:     outgoingUpdateID(decisionID),
		CreatedAt:            s.now(),
	})
	// Virtual draft lets DraftBySentMessageID recognise this as a DoubleU message.
	s.repository.SaveDraft(store.DraftRecord{
		DraftID:              "draft:" + decisionID,
		DecisionID:           decisionID,
		TenantID:             event.TenantID,
		ChatID:               event.ChatID,
		BusinessConnectionID: event.BusinessConnectionID,
		SourceMessageID:      event.SourceMessageID,
		ResponseText:         text,
		TextFormat:           textFormat.Normalize(),
		Status:               store.DraftSent,
		SentMessageID:        sentMsgID,
		CreatedAt:            s.now(),
		UpdatedAt:            s.now(),
	})
}

// saveOutgoingMessageFromDraft persists a synthetic outgoing message for an approved draft so
// the Mini App can display it in chat history.
func (s *Service) saveOutgoingMessageFromDraft(draft store.DraftRecord) {
	if draft.SentMessageID == "" {
		return
	}
	ownerID := ""
	if conn, ok := s.repository.BusinessConnection(draft.BusinessConnectionID); ok {
		ownerID = conn.UserID
	}
	if ownerID == "" {
		if strings.HasPrefix(draft.TenantID, "tenant:user:") {
			ownerID = strings.TrimPrefix(draft.TenantID, "tenant:user:")
		} else if ownerTenant := s.repository.OwnerTenantForBusinessConnection(draft.BusinessConnectionID); strings.HasPrefix(ownerTenant, "tenant:user:") {
			ownerID = strings.TrimPrefix(ownerTenant, "tenant:user:")
		}
	}
	if ownerID == "" {
		return
	}
	s.repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:              "out:" + draft.DecisionID,
		TenantID:             draft.TenantID,
		ChatID:               draft.ChatID,
		BusinessConnectionID: draft.BusinessConnectionID,
		SourceMessageID:      draft.SentMessageID,
		SenderID:             ownerID,
		MessageText:          draft.ResponseText,
		MessageType:          "text",
		TelegramUpdateID:     outgoingUpdateID(draft.DecisionID),
		CreatedAt:            s.now(),
	})
}

// ManualSend delivers an owner-authored reply through the chat's own channel
// (the tenant's bot for bot chats) exactly as if the owner had typed it in
// Telegram. The reply is stored as an outgoing owner message so it becomes
// context the AI sees on later replies, and any pending AI drafts in the chat
// are discarded — the owner has answered, so the suggestions are now obsolete.
func (s *Service) ManualSend(ctx context.Context, tenantID, chatID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("message text is required")
	}
	ownerID := strings.TrimPrefix(tenantID, "tenant:user:")
	decisionID := fmt.Sprintf("manual:%s:%d", chatID, s.now().UnixNano())
	manualEvent := contracts.NormalizedTelegramEvent{TenantID: tenantID, ChatID: chatID}
	sentMsgID, err := s.sendRecipient(ctx, manualEvent, decisionID, text, contracts.TextFormatPlain, false, s.deliverySettingsFor(manualEvent))
	if err != nil {
		return err
	}
	if sentMsgID == "" {
		// Senders without a message-id capability (or idempotent replays) return
		// an empty id; fall back to the decisionID so the stored row stays unique.
		sentMsgID = decisionID
	}
	// Owner-authored outgoing message with no virtual DoubleU draft, so the Mini
	// App labels it "Ты"; lands in RecentMessages → the AI accounts for it next.
	s.repository.SaveMessage(contracts.NormalizedTelegramEvent{
		EventID:          "out:" + decisionID,
		TenantID:         tenantID,
		ChatID:           chatID,
		SourceMessageID:  sentMsgID,
		SenderID:         ownerID,
		MessageText:      text,
		MessageType:      "text",
		TelegramUpdateID: outgoingUpdateID(decisionID),
		CreatedAt:        s.now(),
	})
	s.discardPendingDrafts(ctx, tenantID, chatID)
	if s.notify != nil {
		s.notify(tenantID, "new_message", map[string]any{"chat_id": chatID})
	}
	return nil
}

// discardPendingDrafts rejects every pending draft in a chat. Used when the
// owner answers manually so the AI's now-obsolete suggestions disappear from
// the chat and the owner's bot card.
func (s *Service) discardPendingDrafts(ctx context.Context, tenantID, chatID string) {
	for _, draft := range s.repository.DraftsForChat(tenantID, chatID) {
		if draft.Status != store.DraftPending {
			continue
		}
		draft.Status = store.DraftRejected
		draft.UpdatedAt = s.now()
		s.repository.UpdateDraft(draft)
		s.removeDraftCard(ctx, draft)
		s.notifyDraftStatus(draft)
	}
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func truncateForLog(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "...[truncated]"
}

// episodeMessageLimit is the maximum number of messages allowed in a single episode.
// When a chat accumulates this many messages the episode is closed immediately
// (design §6.2 — episode lifecycle trigger: message_limit).
const episodeMessageLimit = 40

// minEpisodeSpan is the minimum wall-clock duration between ep.StartedAt and
// ep.LastMessageAt required to trigger a summary on message-limit close. A
// 40-message burst arriving within this window is a flood, not a conversation;
// real content (if any) is captured later by the time-based daily fallback.
const minEpisodeSpan = 2 * time.Minute

// summarizeEpisode compresses the messages of one closed episode — real
// episode bounds, not calendar days (MEM-6) — into an episode summary with
// full provenance (source_message_ids). Idempotency is period-overlap based:
// a second close of the same episode, or a daily fallback over an already
// summarised window, is a no-op. Safe to call concurrently.
func (s *Service) summarizeEpisode(ctx context.Context, tenantID, chatID string, startedAt, closedAt time.Time, closureReason string) {
	settings := s.repository.ChatSettings(tenantID, chatID)
	if settings.PrivacyFlags.Contains(contracts.PrivacyNoSummary) {
		return
	}
	if !closedAt.After(startedAt) {
		return
	}
	// Half-open fetch window: nudge the end by a second so the message that
	// closed the episode (received_at == closedAt) is included.
	fetchEnd := closedAt.Add(time.Second)
	if s.repository.EpisodeSummaryOverlaps(tenantID, chatID, startedAt, fetchEnd) {
		return
	}
	messages := s.repository.MessagesByDateRange(tenantID, chatID, startedAt, fetchEnd)
	if len(messages) < 3 {
		return
	}
	texts := make([]string, 0, len(messages))
	sourceIDs := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.Quarantined {
			continue
		}
		if t := strings.TrimSpace(m.Event.MessageText); t != "" {
			texts = append(texts, t)
			sourceIDs = append(sourceIDs, m.Event.SourceMessageID)
		}
	}
	if len(texts) == 0 {
		return
	}
	summarizer, ok := s.ai.(aiclient.ChatSummarizer)
	if !ok {
		return
	}
	summ, err := summarizer.SummarizeChat(ctx, contracts.ChatSummaryRequest{
		Messages:     texts,
		PrivacyFlags: settings.PrivacyFlags,
	})
	if err != nil || summ.BlockedByPrivacy {
		return
	}
	content := strings.TrimSpace(summ.Summary)
	// episode_summaries.content carries CHECK (50..1500 chars): a too-short
	// summary previously failed the INSERT silently (MEM-6/MEM-10).
	if runes := []rune(content); len(runes) < 50 {
		s.logger.Warn("episode summary too short, skipped",
			"tenant_id", tenantID, "chat_id", chatID, "length", len(runes))
		return
	} else if len(runes) > 1500 {
		content = string(runes[:1500])
	}
	id := s.repository.SaveEpisodeSummary(store.EpisodeSummaryRecord{
		TenantID:         tenantID,
		ChatID:           chatID,
		PeriodStart:      startedAt,
		PeriodEnd:        closedAt,
		Content:          content,
		MessageCount:     len(messages),
		SourceMessageIDs: sourceIDs,
		ClosureReason:    closureReason,
	})
	s.logger.Info("episode summary saved",
		"tenant_id", tenantID, "chat_id", chatID,
		"summary_id", id,
		"period_start", startedAt.UTC().Format(time.RFC3339),
		"period_end", closedAt.UTC().Format(time.RFC3339),
		"closure_reason", closureReason,
	)
	s.nudgeEmbeddingWorker()
	if len(summ.Recommendations) > 0 {
		s.sendRecommendationsToOwner(ctx, tenantID, chatID, summ.Recommendations)
	}
}

// summarizeChatDay is the daily fallback grid (MEM-6): it summarises only the
// part of the day that no episode summary already covers — closing the "chat
// active past midnight without an episode close" gap without duplicating
// episode-close summaries.
func (s *Service) summarizeChatDay(ctx context.Context, tenantID, chatID string, date time.Time, closureReason string) {
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := startOfDay.Add(24 * time.Hour)
	from := startOfDay
	if covered := s.repository.LatestCoveredEnd(tenantID, chatID, startOfDay, endOfDay); covered.After(from) {
		from = covered
	}
	if !endOfDay.After(from) {
		return // the whole day is already covered by episode summaries
	}
	s.summarizeEpisode(ctx, tenantID, chatID, from, endOfDay.Add(-time.Second), closureReason)
}

// maxSummaryConcurrency caps simultaneous LLM summarisation calls across all tenants.
// Each call blocks on a remote LLM endpoint; uncapped fan-out at scale causes a request storm.
const maxSummaryConcurrency = 8

// runDailySummariesForDate triggers summarizeChatDay for every active chat on the given date.
// Concurrency is bounded by maxSummaryConcurrency to prevent an LLM request storm.
func (s *Service) runDailySummariesForDate(ctx context.Context, date time.Time) {
	tenantIDs := s.repository.AllActiveTenantIDs()
	s.logger.Info("daily summary run started", "date", date.UTC().Format("2006-01-02"), "tenant_count", len(tenantIDs))
	sem := make(chan struct{}, maxSummaryConcurrency)
	for _, tenantID := range tenantIDs {
		for _, chat := range s.repository.ChatsForTenant(tenantID) {
			tenantID, chatID := tenantID, chat.ChatID // capture loop vars
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				s.summarizeChatDay(ctx, tenantID, chatID, date, "daily_summary")
			}()
		}
	}
	// Drain the semaphore to wait for all goroutines before returning.
	for i := 0; i < maxSummaryConcurrency; i++ {
		sem <- struct{}{}
	}
}

// closeStaleEpisodes materialises episode summaries for any chat whose active episode
// has had no messages for > 6 hours, then removes the active_episodes row.
// Concurrency is bounded by maxSummaryConcurrency to prevent an LLM request storm.
// Safe to call concurrently; episode closure is idempotent via EpisodeSummaryExistsForDate.
func (s *Service) closeStaleEpisodes(ctx context.Context) {
	tenantIDs := s.repository.AllActiveTenantIDs()
	sem := make(chan struct{}, maxSummaryConcurrency)
	for _, tenantID := range tenantIDs {
		for _, chat := range s.repository.ChatsForTenant(tenantID) {
			ep, ok := s.repository.ActiveEpisodeByChat(tenantID, chat.ChatID)
			if !ok {
				continue
			}
			if s.now().Sub(ep.LastMessageAt) < 6*time.Hour {
				continue
			}
			tenantID, chatID, startedAt, lastMsg := tenantID, chat.ChatID, ep.StartedAt, ep.LastMessageAt
			// Close the active episode record before launching the goroutine so a
			// second closeStaleEpisodes call for the same chat cannot race.
			s.repository.CloseActiveEpisode(tenantID, chatID)
			s.logger.Info("stale episode closed",
				"tenant_id", tenantID,
				"chat_id", chatID,
				"idle_hours", s.now().Sub(ep.LastMessageAt).Hours(),
			)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				s.summarizeEpisode(ctx, tenantID, chatID, startedAt, lastMsg, "silence_gap")
			}()
		}
	}
	// Drain the semaphore so callers can observe all goroutines completing.
	for i := 0; i < maxSummaryConcurrency; i++ {
		sem <- struct{}{}
	}
}

// StartEpisodeCloser runs every 15 minutes and closes active episodes that have been
// idle for more than 6 hours (§6.2 — episode lifecycle trigger: silence_gap).
func (s *Service) StartEpisodeCloser(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.closeStaleEpisodes(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// StartDailySummaryScheduler blocks until ctx is cancelled, waking at midnight UTC each day
// to compress the previous day's conversations into long-term memory.
func (s *Service) StartDailySummaryScheduler(ctx context.Context) {
	for {
		now := s.now().UTC()
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		select {
		case <-time.After(time.Until(nextMidnight)):
			yesterday := nextMidnight.Add(-24 * time.Hour)
			s.runDailySummariesForDate(ctx, yesterday)
		case <-ctx.Done():
			return
		}
	}
}

// StartConsolidationWorker runs nightly at 02:00 UTC and calls the consolidate_facts
// stored procedure for every active tenant to clean up expired and superseded facts
// and collapse deep supersession chains (design: sleep-time memory consolidation).
func (s *Service) StartConsolidationWorker(ctx context.Context) {
	for {
		now := s.now().UTC()
		// Schedule for 02:00 UTC the following calendar day.
		nextRun := time.Date(now.Year(), now.Month(), now.Day()+1, 2, 0, 0, 0, time.UTC)
		select {
		case <-time.After(time.Until(nextRun)):
			for _, tenantID := range s.repository.AllActiveTenantIDs() {
				cleaned := s.repository.ConsolidateFacts(tenantID)
				if cleaned > 0 {
					s.logger.Info("fact consolidation completed",
						"tenant_id", tenantID,
						"facts_cleaned", cleaned,
					)
				}
				// Profile suggestions the owner ignored for 14 days expire (§4.5, MEM-4).
				if expired := s.repository.ExpireOldSuggestions(tenantID, 14*24*time.Hour); expired > 0 {
					s.logger.Info("stale profile suggestions expired",
						"tenant_id", tenantID, "count", expired)
				}
				// Sleep-time reflection: after cleanup, synthesise high-level
				// behaviour patterns from clusters of recent facts (#5). Off by
				// default; bounded by candidate selection.
				s.runReflection(ctx, tenantID)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Reflection (sleep-time insight generation, #5) tunables.
const (
	reflectionWindow      = 30 * 24 * time.Hour // only cluster facts this recent
	reflectionMinFacts    = 3                   // min recent facts for a chat to qualify
	reflectionMinCluster  = 3                   // min facts in a cluster to synthesise from
	reflectionMaxSim      = 0.85                // cosine similarity threshold for an edge
	reflectionConfidence  = 0.5                 // modest confidence for an inferred pattern
	reflectionTTL         = 60 * 24 * time.Hour // unreinforced patterns decay
	reflectionMaxClusters = 3                   // cap synthesis calls per chat per run
)

// runReflection generates behaviour patterns for a tenant's eligible chats.
// No-op unless REFLECTION_MODE is shadow|enabled and the AI client can synthesise.
func (s *Service) runReflection(ctx context.Context, tenantID string) {
	if s.reflectionMode != "shadow" && s.reflectionMode != "enabled" {
		return
	}
	if _, ok := s.ai.(aiclient.PatternSynthesizer); !ok {
		return
	}
	since := s.now().Add(-reflectionWindow)
	for _, chatID := range s.repository.ReflectionCandidateChats(tenantID, since, reflectionMinFacts) {
		s.reflectChat(ctx, tenantID, chatID)
	}
}

// clusterByPairs groups fact ids into connected components from similarity edges
// (union-find). Only multi-member components are returned, in deterministic order.
func clusterByPairs(pairs []store.FactPair) [][]int64 {
	parent := make(map[int64]int64)
	var find func(int64) int64
	find = func(x int64) int64 {
		p, ok := parent[x]
		if !ok {
			parent[x] = x
			return x
		}
		if p != x {
			parent[x] = find(p)
		}
		return parent[x]
	}
	for _, pr := range pairs {
		ra, rb := find(pr.IDA), find(pr.IDB)
		if ra != rb {
			parent[ra] = rb
		}
	}
	groups := make(map[int64][]int64)
	for id := range parent {
		root := find(id)
		groups[root] = append(groups[root], id)
	}
	var clusters [][]int64
	for _, members := range groups {
		if len(members) >= 2 {
			sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
			clusters = append(clusters, members)
		}
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i][0] < clusters[j][0] })
	return clusters
}

// reflectChat clusters a chat's recent facts and, for each cluster of at least
// reflectionMinCluster facts, asks the AI service to synthesise one durable
// behaviour pattern. In "shadow" mode the pattern is only logged; in "enabled"
// mode it is embedded and written as a behavior_pattern fact (#5). Idempotent
// across runs: upsert dedup (hash + cosine) collapses a repeated pattern.
func (s *Service) reflectChat(ctx context.Context, tenantID, chatID string) {
	synth, ok := s.ai.(aiclient.PatternSynthesizer)
	if !ok {
		return
	}
	since := s.now().Add(-reflectionWindow)
	clusters := clusterByPairs(s.repository.SimilarFactPairs(tenantID, chatID, since, reflectionMaxSim))
	processed := 0
	for _, cluster := range clusters {
		if len(cluster) < reflectionMinCluster || processed >= reflectionMaxClusters {
			continue
		}
		texts := s.repository.SemanticFactTextsByIDs(tenantID, cluster)
		facts := make([]string, 0, len(cluster))
		for _, id := range cluster {
			if t := texts[id]; t != "" {
				facts = append(facts, t)
			}
		}
		if len(facts) < reflectionMinCluster {
			continue
		}
		processed++
		resp, err := synth.SynthesizePattern(ctx, contracts.SynthesizePatternRequest{
			ClusterFacts: facts,
			TenantID:     tenantID,
			ChatID:       chatID,
		})
		if err != nil {
			s.logger.Warn("pattern synthesis failed",
				"tenant_id", tenantID, "chat_id", chatID, "error", err)
			continue
		}
		pattern := strings.TrimSpace(resp.Pattern)
		if pattern == "" || resp.BlockedByPrivacy {
			continue
		}
		if s.reflectionMode != "enabled" {
			// shadow: record the candidate for eval, write nothing.
			s.logger.Info("reflection pattern (shadow)",
				"tenant_id", tenantID, "chat_id", chatID,
				"pattern", pattern, "cluster_size", len(facts))
			continue
		}
		subject := resp.Subject
		if subject == "" {
			subject = "contact"
		}
		expiresAt := s.now().Add(reflectionTTL)
		embedding := embeddingAt(s.embedCandidates([]string{pattern}), 0)
		result := s.repository.UpsertSemanticFact(store.SemanticFactRecord{
			TenantID:   tenantID,
			ScopeKind:  "chat",
			ChatID:     chatID,
			FactText:   pattern,
			FactType:   "behavior_pattern",
			Subject:    subject,
			Confidence: reflectionConfidence,
			Source:     "llm_reflected",
			ExpiresAt:  &expiresAt,
		}, embedding)
		s.logger.Info("reflection pattern written",
			"tenant_id", tenantID, "chat_id", chatID,
			"action", result.Action, "pattern", pattern)
	}
}

// nudgeEmbeddingWorker signals the embedding worker that new facts need
// vectors (ARCH-12). Non-blocking: a pending nudge already covers the batch.
func (s *Service) nudgeEmbeddingWorker() {
	select {
	case s.embedNudge <- struct{}{}:
	default:
	}
}

// StartEmbeddingWorker generates embeddings for any semantic facts inserted
// without one. It wakes on a 5-minute ticker OR immediately on a nudge from
// saveTypedFacts (ARCH-12) — previously fresh facts were invisible to vector
// recall for up to 5 minutes. Runs in a separate goroutine; safe to call
// concurrently. Activates only when the AI client implements TextEmbedder.
func (s *Service) StartEmbeddingWorker(ctx context.Context) {
	embedder, ok := s.ai.(aiclient.TextEmbedder)
	if !ok {
		return // AI client does not support embedding generation.
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runEmbeddingBatch(ctx, embedder)
		case <-s.embedNudge:
			s.runEmbeddingBatch(ctx, embedder)
		case <-ctx.Done():
			return
		}
	}
}

// runEmbeddingBatch fetches facts and episode summaries without embeddings (up to 50
// at a time), calls the Python embedding endpoint, and persists vectors.
func (s *Service) runEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder) {
	const batchSize = 50
	s.runFactEmbeddingBatch(ctx, embedder, batchSize)
	s.runEpisodeSummaryEmbeddingBatch(ctx, embedder, batchSize)
}

func (s *Service) runFactEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder, batchSize int) {
	facts := s.repository.FactsWithoutEmbeddings(batchSize)
	if len(facts) == 0 {
		return
	}
	texts := make([]string, len(facts))
	for i, f := range facts {
		texts[i] = f.FactText
	}
	resp, err := embedder.EmbedTexts(ctx, aiclient.EmbedTextsRequest{
		Texts: texts,
		Model: "qwen/qwen3-embedding-8b",
	})
	if err != nil || resp.ModelUsed == "none" || resp.ModelUsed == "error" {
		s.logger.Warn("fact embedding batch failed", "error", err, "model", resp.ModelUsed)
		return
	}
	for i, f := range facts {
		if i >= len(resp.Embeddings) {
			break
		}
		if emb := resp.Embeddings[i]; len(emb) > 0 {
			s.repository.SaveFactEmbedding(f.ID, f.TenantID, f.ScopeKind, f.ChatID, emb, resp.ModelUsed)
		}
	}
	s.logger.Info("fact embedding batch completed", "facts_embedded", len(facts), "model", resp.ModelUsed)
}

func (s *Service) runEpisodeSummaryEmbeddingBatch(ctx context.Context, embedder aiclient.TextEmbedder, batchSize int) {
	summaries := s.repository.EpisodeSummariesWithoutEmbeddings(batchSize)
	if len(summaries) == 0 {
		return
	}
	texts := make([]string, len(summaries))
	for i, sm := range summaries {
		texts[i] = sm.Content
	}
	resp, err := embedder.EmbedTexts(ctx, aiclient.EmbedTextsRequest{
		Texts: texts,
		Model: "qwen/qwen3-embedding-8b",
	})
	if err != nil || resp.ModelUsed == "none" || resp.ModelUsed == "error" {
		s.logger.Warn("episode summary embedding batch failed", "error", err, "model", resp.ModelUsed)
		return
	}
	for i, sm := range summaries {
		if i >= len(resp.Embeddings) {
			break
		}
		if emb := resp.Embeddings[i]; len(emb) > 0 {
			s.repository.SaveEpisodeSummaryEmbedding(sm.ID, sm.TenantID, emb, resp.ModelUsed)
		}
	}
	s.logger.Info("episode summary embedding batch completed", "summaries_embedded", len(summaries), "model", resp.ModelUsed)
}

// sendRecommendationsToOwner delivers proactive recommendations to the owner's bot chat.
// Recommendations are informational and are not persisted as memory facts — they are
// derived from conversation history and will surface again at the next summarization run.
func (s *Service) sendRecommendationsToOwner(ctx context.Context, tenantID, chatID string, recommendations []string) {
	ownerChatID := strings.TrimPrefix(tenantID, "tenant:user:")
	if ownerChatID == tenantID {
		return // not a "tenant:user:USERID" tenant
	}
	chat, ok := s.repository.Chat(tenantID, chatID)
	if !ok {
		return
	}
	lines := make([]string, 0, len(recommendations)+2)
	lines = append(lines, fmt.Sprintf("💡 Рекомендации для чата с %s:", chatDisplayName(chat)))
	lines = append(lines, "")
	for _, rec := range recommendations {
		lines = append(lines, "• "+rec)
	}
	lines = append(lines, "")
	lines = append(lines, "(на основе истории общения)")
	_ = s.sender.SendMessage(ctx, telegram.OutboundMessage{
		TenantID: tenantID,
		ChatID:   ownerChatID,
		Text:     strings.Join(lines, "\n"),
	})
}

// sendQuestionToOwner delivers a question card with AI-suggested option buttons to the owner's bot chat.
func (s *Service) sendQuestionToOwner(ctx context.Context, event contracts.NormalizedTelegramEvent, draft store.DraftRecord, ownerID string) {
	ownerChatID := s.ownerChatIDForEvent(event)
	if ownerChatID == "" {
		s.logger.Warn("cannot deliver question card to owner: chat ID not found", "business_connection_id", event.BusinessConnectionID, "draft_id", draft.DraftID)
		return
	}
	cardText := bot.QuestionCardText(event.MessageText, event.ChatDisplayName)
	keyboard := bot.QuestionOptionsKeyboard(draft.DraftID, draft.SuggestedOptions)
	msg := telegram.OutboundMessage{
		TenantID:        event.TenantID,
		ChatID:          ownerChatID,
		SourceMessageID: event.SourceMessageID,
		DecisionID:      draft.DecisionID,
		Text:            cardText,
		ReplyMarkup:     keyboard,
	}
	msgID, err := s.sendOwnerCard(ctx, msg, false)
	if err != nil {
		s.logger.Warn("failed to deliver question card to owner", "tenant_id", event.TenantID, "draft_id", draft.DraftID, "error", err)
		return
	}
	if msgID != "" {
		draft.OwnerChatID = ownerChatID
		draft.OwnerMessageID = msgID
		s.repository.UpdateDraft(draft)
	}
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
