package core

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// When a shard is full the update spills to the bounded overflow pool; when that
// is full too the enqueue is shed (so the HTTP layer answers 429) instead of
// spawning an unbounded goroutine.
func TestEnqueueTelegramWebhookShedsWhenSaturated(t *testing.T) {
	s := NewService(nil, nil, nil)
	workers := make([]chan webhookJob, webhookWorkerCount)
	for i := range workers {
		workers[i] = make(chan webhookJob) // unbuffered, no drain → always full
	}
	s.webhookWorkers = workers

	// Overflow has room: the update spills rather than being shed.
	s.webhookOverflow = make(chan webhookJob, 1)
	if !s.EnqueueTelegramWebhook([]byte("{}"), "t1") {
		t.Fatal("expected spill to the overflow pool to be accepted")
	}
	// Overflow is now full (cap 1, no drain): the next update is shed.
	if s.EnqueueTelegramWebhook([]byte("{}"), "t1") {
		t.Fatal("expected shed (false) when shard and overflow are both full")
	}
}

// One tenant flooding async-LLM work cannot starve another: each tenant has its
// own token bucket, so t1's burst is capped while t2 still gets serviced.
func TestRunAsyncLLMTaskFairSharePerTenant(t *testing.T) {
	// Negligible refill, burst 1 → each tenant gets exactly one task, then sheds.
	s := NewService(nil, nil, nil, WithTenantAsyncRate(rate.Limit(0.001), 1))

	var ran int32
	done := make(chan struct{}, 8)
	fn := func() { atomic.AddInt32(&ran, 1); done <- struct{}{} }

	s.runAsyncLLMTask("t1", fn) // allowed: t1 burst token
	s.runAsyncLLMTask("t1", fn) // shed: t1 bucket empty
	s.runAsyncLLMTask("t2", fn) // allowed: t2 has its own bucket (fair-share)

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected 2 tasks to run, got %d", atomic.LoadInt32(&ran))
		}
	}
	if got := atomic.LoadInt32(&ran); got != 2 {
		t.Fatalf("expected exactly 2 tasks (one per tenant), got %d", got)
	}
}
