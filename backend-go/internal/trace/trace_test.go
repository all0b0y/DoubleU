package trace

import (
	"context"
	"strings"
	"testing"
)

func TestDeriveTraceIDIsIdempotent(t *testing.T) {
	a := DeriveTraceID("evt-12345")
	b := DeriveTraceID("evt-12345")
	if a != b {
		t.Fatalf("trace id not deterministic: %s vs %s", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-hex trace id, got %d chars: %s", len(a), a)
	}
	if strings.ContainsAny(a, "GHIJKLMNOPQRSTUVWXYZghijklmnopqrstuvwxyz") {
		t.Fatalf("trace id must be lowercase hex: %s", a)
	}
}

func TestDeriveTraceIDIsDistinct(t *testing.T) {
	a := DeriveTraceID("evt-1")
	b := DeriveTraceID("evt-2")
	if a == b {
		t.Fatalf("distinct events should produce distinct trace ids")
	}
}

func TestDeriveTraceIDEmptyReturnsEmpty(t *testing.T) {
	if got := DeriveTraceID(""); got != "" {
		t.Fatalf("empty event_id must yield empty trace id, got %q", got)
	}
}

// TestDeriveTraceIDCrossLanguageFixture pins one known event_id → expected
// trace-id pair so the matching Python test asserts the same hex string.
// If you change the derivation algorithm, update both sides.
func TestDeriveTraceIDCrossLanguageFixture(t *testing.T) {
	const fixtureEvent = "doubleu-trace-fixture-001"
	got := DeriveTraceID(fixtureEvent)
	// Recorded from the deterministic UUIDv5(NAMESPACE_OID, fixtureEvent) computation.
	const want = "0bc93694d9dd5bdb8e30d9cfa994d333"
	if got != want {
		t.Fatalf("cross-language fixture drift: got %s want %s", got, want)
	}
}

func TestFormatAndParseTraceparent(t *testing.T) {
	tid := DeriveTraceID("evt-tp")
	tp := FormatTraceparent(tid)
	if !strings.HasPrefix(tp, "00-"+tid+"-") {
		t.Fatalf("unexpected traceparent: %s", tp)
	}
	if got := ParseTraceparent(tp); got != tid {
		t.Fatalf("parse round-trip failed: got %s want %s", got, tid)
	}
	if got := ParseTraceparent("bogus"); got != "" {
		t.Fatalf("malformed traceparent must yield empty, got %q", got)
	}
	if got := ParseTraceparent(""); got != "" {
		t.Fatalf("empty traceparent must yield empty, got %q", got)
	}
}

func TestWithAndFromContext(t *testing.T) {
	ctx := context.Background()
	if got := FromContext(ctx); got != "" {
		t.Fatalf("empty context must have empty trace id, got %q", got)
	}
	ctx = WithTraceID(ctx, "deadbeef")
	if got := FromContext(ctx); got != "deadbeef" {
		t.Fatalf("context trace id lookup failed, got %q", got)
	}
}
