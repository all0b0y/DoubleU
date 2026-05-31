"""W3C trace-id derivation and ``traceparent`` header helpers.

Mirrors the Go ``backend-go/internal/trace`` package so both sides land on the
same 32-hex string when given the same ``event_id``.  This makes Langfuse spans
emitted by the Go backend and the Python AI service join on a single trace.
"""

from __future__ import annotations

import uuid


def derive_trace_id(event_id: str) -> str:
    """Return a deterministic 32-hex W3C trace-id derived from ``event_id``.

    Idempotent webhook replays for the same Telegram update produce the same
    trace_id (UUIDv5 over ``NAMESPACE_OID``) so retries do not start new
    Langfuse traces.  Empty input yields empty output so the synthetic
    normalization-error event can opt out cleanly.
    """
    if not event_id:
        return ""
    return uuid.uuid5(uuid.NAMESPACE_OID, event_id).hex


def format_traceparent(trace_id: str) -> str:
    """Render a trace-id into a W3C ``traceparent`` header value.

    span-id is a 16-hex placeholder; downstream consumers (Langfuse, OTel SDK)
    replace it with a per-span value when they emit child spans.
    """
    if not trace_id:
        return ""
    return f"00-{trace_id}-0000000000000000-01"


def parse_traceparent(header: str | None) -> str:
    """Extract the trace-id from a ``traceparent`` header value.

    Returns empty when the header is missing or malformed; callers may then
    fall back to the legacy ``X-DoubleU-Trace-ID`` header or derive a fresh
    id from the inbound ``event_id``.
    """
    if not header:
        return ""
    header = header.strip()
    parts = header.split("-")
    if len(parts) < 2:
        return ""
    trace_id = parts[1]
    if len(trace_id) != 32:
        return ""
    return trace_id
