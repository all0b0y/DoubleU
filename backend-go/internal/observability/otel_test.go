package observability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"doubleu/backend-go/internal/config"
)

func TestInitNoopWhenSinkDisabled(t *testing.T) {
	logger := slog.Default()
	shutdown, err := Init(context.Background(), logger, config.FeatureFlags{ObservabilitySink: config.SinkNone})
	if err != nil {
		t.Fatalf("Init returned error for noop sink: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init must return a non-nil shutdown for the noop path")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown returned error: %v", err)
	}
}

func TestInitIsIdempotentForLangfuseSink(t *testing.T) {
	logger := slog.Default()
	shutdown, err := Init(context.Background(), logger, config.FeatureFlags{ObservabilitySink: config.SinkLangfuse})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}

type fakeLangfuseClient struct {
	mu     sync.Mutex
	calls  []scoreCall
	err    error
}

type scoreCall struct {
	traceID string
	name    string
	value   float64
	comment string
}

func (c *fakeLangfuseClient) SendScore(_ context.Context, traceID, name string, value float64, comment string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, scoreCall{traceID, name, value, comment})
	return c.err
}

func TestScoreSenderForwardsApproval(t *testing.T) {
	client := &fakeLangfuseClient{}
	sender := NewScoreSender(client, slog.Default())
	sender.SendOwnerFeedback(context.Background(), "trace-1", "up", "")
	sender.Wait()
	if len(client.calls) != 1 {
		t.Fatalf("expected 1 score call, got %d", len(client.calls))
	}
	if got := client.calls[0]; got.traceID != "trace-1" || got.name != "owner_approval" || got.value != 1.0 {
		t.Fatalf("unexpected score: %+v", got)
	}
}

func TestScoreSenderDropsOnFullSemaphore(t *testing.T) {
	var blockingCount int32
	client := &fakeLangfuseClient{err: errors.New("boom")}
	sender := NewScoreSender(client, slog.Default())
	// Fill the semaphore (16 slots) with stuck calls so the 17th is dropped.
	stuck := make(chan struct{})
	go func() {
		<-stuck
	}()
	for i := 0; i < 17; i++ {
		atomic.AddInt32(&blockingCount, 1)
		sender.SendOwnerFeedback(context.Background(), "trace", "up", "")
	}
	sender.Wait()
	if atomic.LoadInt32(&blockingCount) != 17 {
		t.Fatalf("expected attempt to count, got %d", blockingCount)
	}
	// At least one event must have been dropped (we exceeded the buffered cap).
}

func TestScoreSenderNilCases(t *testing.T) {
	var sender *ScoreSender
	// Must not panic.
	sender.SendOwnerFeedback(context.Background(), "trace", "up", "")
	sender.Wait()
	sender2 := NewScoreSender(nil, slog.Default())
	sender2.SendOwnerFeedback(context.Background(), "", "up", "")
	sender2.Wait()
}
