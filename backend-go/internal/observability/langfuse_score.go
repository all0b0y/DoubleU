package observability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// LangfuseClient is the boundary used by the bot/core code to attach owner
// feedback scores to drafts via their persisted trace_id.  The interface is
// satisfied by the concrete HTTP client and by fake implementations in tests.
type LangfuseClient interface {
	SendScore(ctx context.Context, traceID, name string, value float64, comment string) error
}

// ScoreSender wraps a LangfuseClient with a bounded semaphore so the bot
// reply path can fire-and-forget owner feedback scores without blocking on
// the Langfuse API.  The semaphore caps the number of in-flight goroutines.
type ScoreSender struct {
	client    LangfuseClient
	semaphore chan struct{}
	logger    *slog.Logger
	wg        sync.WaitGroup
}

// NewScoreSender returns a fire-and-forget owner-feedback score forwarder.
// ``client == nil`` produces a noop sender so production code can ignore the
// nil case.
func NewScoreSender(client LangfuseClient, logger *slog.Logger) *ScoreSender {
	return &ScoreSender{
		client:    client,
		semaphore: make(chan struct{}, 16),
		logger:    logger,
	}
}

// SendOwnerFeedback forwards an owner approval/reject/thumb-up/down score to
// Langfuse without blocking the caller.  When the semaphore is full the score
// is dropped with a warning log so the bot UI is never delayed.
func (s *ScoreSender) SendOwnerFeedback(ctx context.Context, traceID string, rating string, comment string) {
	if s == nil || s.client == nil || traceID == "" {
		return
	}
	value, name := scoreFor(rating)
	select {
	case s.semaphore <- struct{}{}:
	default:
		if s.logger != nil {
			s.logger.Warn("langfuse score semaphore full; dropping feedback", "trace_id", traceID, "rating", rating)
		}
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.semaphore }()
		if err := s.client.SendScore(ctx, traceID, name, value, comment); err != nil {
			if s.logger != nil {
				s.logger.Warn("langfuse score send failed", "trace_id", traceID, "rating", rating, "error", err)
			}
		}
	}()
}

// Wait blocks until every in-flight score forward completes.  Used by tests.
func (s *ScoreSender) Wait() {
	if s == nil {
		return
	}
	s.wg.Wait()
}

// scoreFor maps a contract rating onto the Langfuse score name + value.
func scoreFor(rating string) (float64, string) {
	switch rating {
	case "up", "approve":
		return 1.0, "owner_approval"
	case "down", "reject":
		return 0.0, "owner_approval"
	case "edit":
		return 0.5, "owner_approval"
	default:
		return 0.0, "owner_approval"
	}
}

// ErrLangfuseNotConfigured is returned by client constructors when no
// credentials are present so callers can fall back to a noop sender.
var ErrLangfuseNotConfigured = errors.New("langfuse not configured")
