package core

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
	"doubleu/backend-go/internal/telegram"
	tracepkg "doubleu/backend-go/internal/trace"
)

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
)

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
