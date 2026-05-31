// Package trace owns the cross-service trace-id derivation and propagation.
//
// trace_id is derived deterministically from the inbound webhook event_id so
// that idempotent replays of the same Telegram update produce the *same*
// trace, not a new one — this lets us join Langfuse spans across retries.
//
// The derivation uses UUIDv5 (SHA-1 over a fixed OID namespace) and takes the
// first 16 bytes (32 hex chars) of the digest, matching the W3C trace-context
// trace-id format consumed by every OTel SDK and Langfuse.  Collision
// probability is cryptographically negligible.
package trace

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// namespaceOID is RFC 4122 NAMESPACE_OID, the same namespace used by the
// Python derive_trace_id so both sides land on identical hex strings.
var namespaceOID = []byte{
	0x6b, 0xa7, 0xb8, 0x12,
	0x9d, 0xad,
	0x11, 0xd1,
	0x80, 0xb4,
	0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// DeriveTraceID returns a 32-hex-character W3C trace-id derived from eventID.
//
// Empty input yields an empty string so callers that handle synthetic events
// without an event_id can opt out of trace propagation cleanly.
func DeriveTraceID(eventID string) string {
	if eventID == "" {
		return ""
	}
	h := sha1.New()
	h.Write(namespaceOID)
	h.Write([]byte(eventID))
	sum := h.Sum(nil)
	// UUIDv5: set version (5) into nibble at byte 6 and variant (RFC 4122) at byte 8.
	sum[6] = (sum[6] & 0x0F) | 0x50
	sum[8] = (sum[8] & 0x3F) | 0x80
	return hex.EncodeToString(sum[:16])
}

// FormatTraceparent renders a trace-id into a W3C traceparent header value.
// span-id is a fresh 16-hex placeholder; downstream services replace it.
func FormatTraceparent(traceID string) string {
	if traceID == "" {
		return ""
	}
	// version-traceid-spanid-flags.  Sampled flag = 01.
	return "00-" + traceID + "-0000000000000000-01"
}

// ParseTraceparent extracts the trace-id from a W3C traceparent header value.
// Returns empty when the header is missing or malformed.
func ParseTraceparent(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	parts := strings.Split(header, "-")
	if len(parts) < 2 {
		return ""
	}
	traceID := parts[1]
	if len(traceID) != 32 {
		return ""
	}
	return traceID
}

type traceCtxKey struct{}

// WithTraceID stores the trace-id on the request context.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, traceID)
}

// FromContext returns the trace-id on the context, or empty if none.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceCtxKey{}).(string)
	return v
}
