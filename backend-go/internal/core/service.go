// Package core contains backend business workflows and final policy decisions.
package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	repository       *store.Repository
	ai               aiclient.Client
	sender           telegram.Sender
	botLayer         *bot.Layer
	engine           DecisionEngine
	logger           *slog.Logger
	now              func() time.Time
	debounce         sync.Map      // map[chatKey]string — latest event_id per chat for typing-delay debounce
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
	// chatLimiters bounds inbound message rate per chat+sender to stop a single
	// user from flooding a bot chat through the full persist→episode→reply path.
	// Keyed by "tenant|chat" (business) or "tenant|chat|sender" (bot); built lazily.
	chatLimiters sync.Map
	chatRate     rate.Limit // sustained messages/sec per chat+sender
	chatBurst    int        // initial burst allowance
	// thinkingRefreshInterval is reserved for bot "thinking" keep-alives; tests
	// can set it to zero while the current implementation sends one indicator.
	thinkingRefreshInterval time.Duration
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
	// schedulerStarted prevents duplicate workers when both Server.Start and
	// cmd/backend wire the lifecycle hook.
	schedulerStarted atomic.Bool
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

const (
	// maxAsyncLLMConcurrency caps simultaneous fire-and-forget LLM calls
	// (typed extraction, message-limit summarisation) across all chats.
	maxAsyncLLMConcurrency = 8
	// defaultTenantAsyncRate / defaultTenantAsyncBurst bound a single tenant's
	// share of the async-LLM pool: 5 tasks/sec sustained, bursts up to 8.
	defaultTenantAsyncRate  = rate.Limit(5)
	defaultTenantAsyncBurst = 8
	// defaultChatRate / defaultChatBurst bound inbound messages per chat+sender:
	// 0.5 msg/s sustained (one message per 2 seconds) with an initial burst of 8.
	defaultChatRate  = rate.Limit(0.5)
	defaultChatBurst = 8
)

const contactProviderProblemText = "ИИ не смог сформировать ответ. Попробуйте отправить сообщение ещё раз."

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

// WithChatRateLimit configures the per-chat+sender inbound message rate limit.
// Bot chats are keyed per tenant|chat|sender; business chats per tenant|chat.
// Zero values keep the defaults (0.5 msg/s, burst 8).
func WithChatRateLimit(r rate.Limit, burst int) Option {
	return func(service *Service) {
		if r > 0 {
			service.chatRate = r
		}
		if burst > 0 {
			service.chatBurst = burst
		}
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
		chatRate:             defaultChatRate,
		chatBurst:            defaultChatBurst,
		media:                DefaultMediaConfig(),
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// tenantLimiter returns the token bucket for a tenant, creating it on first use.
func (s *Service) tenantLimiter(tenantID string) *rate.Limiter {
	if v, ok := s.tenantLimiters.Load(tenantID); ok {
		return v.(*rate.Limiter)
	}
	v, _ := s.tenantLimiters.LoadOrStore(tenantID, rate.NewLimiter(s.tenantRate, s.tenantBurst))
	return v.(*rate.Limiter)
}

// chatLimiterFor returns the lazily-built token bucket for the chat+sender key.
// Bot transport scopes by tenant|chat|sender (each person gets their own bucket).
// Business transport scopes by tenant|chat (the owner's conversation unit).
func (s *Service) chatLimiterFor(event contracts.NormalizedTelegramEvent) *rate.Limiter {
	var key string
	if event.Transport == contracts.TransportBot {
		key = event.TenantID + "|" + event.ChatID + "|" + event.SenderID
	} else {
		key = event.TenantID + "|" + event.ChatID
	}
	if v, ok := s.chatLimiters.Load(key); ok {
		return v.(*rate.Limiter)
	}
	v, _ := s.chatLimiters.LoadOrStore(key, rate.NewLimiter(s.chatRate, s.chatBurst))
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

// pythonOwnsMemory reports whether fact persistence is delegated to the
// Python agent runtime (AI_MEMORY_SOURCE=python).
func (s *Service) pythonOwnsMemory() bool {
	return s.aiMemorySource == "python"
}

func (s *Service) saveAIScheduledJobs(event contracts.NormalizedTelegramEvent, settings contracts.ChatSettings, jobs []contracts.ScheduledJob) error {
	if len(jobs) == 0 {
		return nil
	}
	settings = settings.WithDefaults()
	if settings.PrivacyFlags.Contains(contracts.PrivacyNoMemory) ||
		settings.PrivacyFlags.Contains(contracts.PrivacyPrivate) {
		return fmt.Errorf("chat privacy blocks AI-created scheduled job")
	}
	now := s.now()
	for _, job := range jobs {
		payload := make(map[string]any, len(job.Payload))
		for key, value := range job.Payload {
			payload[key] = value
		}
		text, ok := payload["text"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("scheduled job payload text is empty")
		}
		payload["text"] = strings.TrimSpace(text)
		createdBy := strings.TrimSpace(job.CreatedBy)
		if createdBy == "" {
			createdBy = "ai"
		}
		if createdBy != "ai" {
			return fmt.Errorf("invalid AI scheduled job creator %q", job.CreatedBy)
		}
		fireAt := job.FireAt.UTC()
		if !fireAt.After(now) {
			return fmt.Errorf("scheduled job fire_at must be in the future")
		}
		if fireAt.After(now.Add(365 * 24 * time.Hour)) {
			return fmt.Errorf("scheduled job fire_at must be no more than 1 year in the future")
		}
		botID := event.BotID
		if botID == "" {
			if parsed, _, botChat := contracts.ParseBotChatID(event.ChatID); botChat {
				botID = parsed
			}
		}
		if _, err := s.repository.InsertScheduledJob(store.ScheduledJobRecord{
			TenantID:       event.TenantID,
			ChatID:         event.ChatID,
			BotID:          botID,
			Kind:           strings.TrimSpace(job.Kind),
			Target:         strings.TrimSpace(job.Target),
			FireAt:         fireAt,
			Payload:        payload,
			CreatedBy:      createdBy,
			IdempotencyKey: strings.TrimSpace(job.IdempotencyKey),
		}); err != nil {
			return err
		}
	}
	s.logger.Info("scheduled jobs persisted from AI response",
		"tenant_id", event.TenantID,
		"chat_id", event.ChatID,
		"count", len(jobs),
	)
	return nil
}

// ingestionModeFor resolves the tenant's effective ingestion mode
// (explicit per-tenant selection or the configured default).
func (s *Service) ingestionModeFor(tenantID string) contracts.IngestionMode {
	if mode := s.repository.TenantIngestionMode(tenantID); mode != "" {
		return mode
	}
	return s.defaultIngestionMode
}

// contextMarkerPrefixes are strings that identify assembled prompt context lines injected
// by the Go backend (e.g. date headers, retrieved facts, conversation history).
// If any of these appear in memory_used they are context contamination — not extracted facts —
// and must be rejected to prevent the self-poisoning loop described in the Bug Backlog.
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
	s.saveExtractedFactsWithRoleContext(event, facts, s.activeRoleContextIDForEvent(event))
}

func (s *Service) saveExtractedFactsWithRoleContext(event contracts.NormalizedTelegramEvent, facts []string, roleContextID int64) {
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
			RoleContextID:   roleContextID,
		}, embeddingAt(embeddings, i))
	}
}

// processPassive runs asynchronous fact extraction for unclaimed chats.
// It opens/updates the active episode and fires the extractor in a goroutine so it never
// blocks the webhook path. No response is generated and no draft is created.
func (s *Service) processPassive(ctx context.Context, event contracts.NormalizedTelegramEvent) {
	roleContextID := s.activeRoleContextIDForEvent(event)
	// Maintain an active episode so episode-based context works after claim.
	now := s.now()
	ep := store.ActiveEpisodeRecord{
		TenantID:      event.TenantID,
		ChatID:        event.ChatID,
		StartedAt:     now,
		LastMessageAt: now,
		MessageCount:  1,
		RoleContextID: roleContextID,
	}
	if existing, ok := s.repository.ActiveEpisodeByChat(event.TenantID, event.ChatID); ok {
		ep.StartedAt = existing.StartedAt
		ep.MessageCount = existing.MessageCount + 1
		if ep.RoleContextID == 0 {
			ep.RoleContextID = existing.RoleContextID
		}
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
					RoleContextID:   roleContextID,
				})
				return
			}
			if resp.BlockedByPrivacy {
				return
			}
			outcome := s.saveTypedFacts(event, resp.Facts, roleContextID)
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
				RoleContextID:   roleContextID,
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
				s.saveExtractedFactsWithRoleContext(event, resp.Memories, roleContextID)
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

func (s *Service) saveTypedFacts(event contracts.NormalizedTelegramEvent, facts []aiclient.TypedFact, roleContextID int64) extractionOutcome {
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
			RoleContextID:   roleContextID,
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

// notifyAIError pushes a real-time error notice to connected Mini App clients so
// a failed AI turn surfaces as a retryable message rather than the thinking
// indicator disappearing with nothing in its place.
func (s *Service) notifyAIError(ctx context.Context, event contracts.NormalizedTelegramEvent) {
	if event.Transport == contracts.TransportBot {
		if _, err := s.sendRecipient(ctx, event, event.EventID+":ai_error", contactProviderProblemText, contracts.TextFormatPlain, false, s.deliverySettingsFor(event)); err != nil {
			s.logger.Warn("telegram contact ai-error fallback failed",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"error", err,
			)
		}
	}
	if s.notify != nil {
		s.notify(event.TenantID, "ai_error", map[string]any{
			"chat_id": event.ChatID,
			"message": contactProviderProblemText,
		})
	}
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

// Repository returns the service repository.
func (s *Service) Repository() *store.Repository {
	return s.repository
}

func (s *Service) createDraft(event contracts.NormalizedTelegramEvent, decisionID, text string, textFormat contracts.TextFormat) store.DraftRecord {
	return s.createDraftWithRoleContext(event, decisionID, text, textFormat, s.activeRoleContextIDForEvent(event))
}

func (s *Service) createDraftWithRoleContext(event contracts.NormalizedTelegramEvent, decisionID, text string, textFormat contracts.TextFormat, roleContextID int64) store.DraftRecord {
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
		DraftType:            "reply",
		Status:               store.DraftPending,
		RoleContextID:        roleContextID,
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

func shouldSurfaceScheduledJobs(role *contracts.AIRole) bool {
	if role == nil {
		return false
	}
	return contracts.ToolEnabled(role.ToolSettings, "scheduler.list_reminders") ||
		contracts.ToolEnabled(role.ToolSettings, "scheduler.cancel_reminder")
}

func existingScheduledJobs(jobs []store.ScheduledJobRecord) []contracts.ExistingScheduledJob {
	existing := make([]contracts.ExistingScheduledJob, 0, len(jobs))
	for _, job := range jobs {
		if job.Status != "pending" {
			continue
		}
		existing = append(existing, contracts.ExistingScheduledJob{
			ID:     job.ID,
			Kind:   job.Kind,
			Target: job.Target,
			FireAt: job.FireAt,
			Text:   scheduledJobText(job),
		})
	}
	return existing
}

func scheduledJobText(job store.ScheduledJobRecord) string {
	if job.Payload == nil {
		return ""
	}
	if text, ok := job.Payload["text"].(string); ok {
		return text
	}
	return ""
}

func (s *Service) cancelAIScheduledJobs(event contracts.NormalizedTelegramEvent, ids []int64) {
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if !s.repository.CancelJob(event.TenantID, event.ChatID, id) {
			s.logger.Warn("scheduled job cancellation ignored",
				"tenant_id", event.TenantID,
				"chat_id", event.ChatID,
				"job_id", id,
			)
		}
	}
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

// inferOwnerChatID finds the owner's bot chat ID for a business connection
// by looking at chats that have been claimed by a tenant:user:* owner.
// outgoingUpdateID derives a unique negative int64 from a decisionID for synthetic outgoing
// messages. Negative values never collide with real Telegram update IDs (always positive).
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
	s.saveOutgoingMessageWithRoleContext(event, ownerID, sentMsgID, text, decisionID, textFormat, s.activeRoleContextIDForEvent(event))
}

func (s *Service) saveOutgoingMessageWithRoleContext(event contracts.NormalizedTelegramEvent, ownerID, sentMsgID, text, decisionID string, textFormat contracts.TextFormat, roleContextID int64) {
	if ownerID == "" || sentMsgID == "" {
		return
	}
	s.repository.SaveMessageWithRoleContext(contracts.NormalizedTelegramEvent{
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
	}, roleContextID)
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
		RoleContextID:        roleContextID,
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
	s.repository.SaveMessageWithRoleContext(contracts.NormalizedTelegramEvent{
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
	}, draft.RoleContextID)
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
