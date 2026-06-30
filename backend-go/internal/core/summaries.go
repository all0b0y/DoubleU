package core

import (
	"context"
	"strings"
	"time"

	"doubleu/backend-go/internal/aiclient"
	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

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
func (s *Service) summarizeEpisode(ctx context.Context, tenantID, chatID string, startedAt, closedAt time.Time, closureReason string, roleContextID int64) {
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
		RoleContextID:    roleContextID,
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
	s.summarizeEpisode(ctx, tenantID, chatID, from, endOfDay.Add(-time.Second), closureReason, 0)
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
			tenantID, chatID, startedAt, lastMsg, roleContextID := tenantID, chat.ChatID, ep.StartedAt, ep.LastMessageAt, ep.RoleContextID
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
				s.summarizeEpisode(ctx, tenantID, chatID, startedAt, lastMsg, "silence_gap", roleContextID)
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
