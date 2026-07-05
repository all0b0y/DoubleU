package core

import (
	"context"
	"errors"
	"testing"

	"doubleu/backend-go/internal/contracts"
	"doubleu/backend-go/internal/store"
)

// sequencedAI returns one queued error per call (nil = success), so a test can
// script "fail once then succeed" and assert the retry count.
type sequencedAI struct {
	fakeAI
	errs  []error
	calls int
}

func (s *sequencedAI) ProcessMessage(ctx context.Context, req contracts.AIProcessMessageRequest) (contracts.AIProcessMessageResponse, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return contracts.AIProcessMessageResponse{}, s.errs[i]
	}
	return contracts.AIProcessMessageResponse{
		ResponseText: "ok", Decision: contracts.DecisionAutoReply, RiskLevel: contracts.RiskLow,
	}, nil
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string { return "i/o timeout" }
func (fakeTimeout) Timeout() bool { return true }

func TestIsTimeoutError(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must be a timeout")
	}
	if !isTimeoutError(fakeTimeout{}) {
		t.Fatal("an error whose Timeout() is true must be a timeout")
	}
	if isTimeoutError(errors.New("connection reset by peer")) {
		t.Fatal("a plain transient error must not be classified as a timeout")
	}
}

func TestProcessMessageRetriesTransientError(t *testing.T) {
	ai := &sequencedAI{errs: []error{errors.New("connection reset by peer")}} // fail once, then ok
	svc := newTestService(store.NewRepository(), ai, &fakeSender{})

	resp, err := svc.processMessageWithRetry(context.Background(),
		contracts.NormalizedTelegramEvent{TenantID: "t", ChatID: "c"},
		contracts.AIProcessMessageRequest{})

	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if ai.calls != 2 {
		t.Fatalf("expected 2 attempts (one retry), got %d", ai.calls)
	}
	if resp.ResponseText != "ok" {
		t.Fatalf("expected recovered response, got %q", resp.ResponseText)
	}
}

func TestProcessMessageDoesNotRetryTimeout(t *testing.T) {
	ai := &sequencedAI{errs: []error{fakeTimeout{}, fakeTimeout{}}}
	svc := newTestService(store.NewRepository(), ai, &fakeSender{})

	_, err := svc.processMessageWithRetry(context.Background(),
		contracts.NormalizedTelegramEvent{TenantID: "t", ChatID: "c"},
		contracts.AIProcessMessageRequest{})

	if err == nil {
		t.Fatal("expected the timeout error to surface")
	}
	if ai.calls != 1 {
		t.Fatalf("a slow-model timeout must not be retried, got %d attempts", ai.calls)
	}
}
