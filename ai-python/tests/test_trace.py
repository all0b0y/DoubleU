"""Phase 0: derive_trace_id / traceparent contract tests."""

from __future__ import annotations

import unittest

from ai_agent.observability.trace import (
    derive_trace_id,
    format_traceparent,
    parse_traceparent,
)


class TraceIDDerivationTests(unittest.TestCase):
    """derive_trace_id is the single source of truth for the W3C trace-id."""

    def test_derive_trace_id_is_idempotent(self) -> None:
        self.assertEqual(derive_trace_id("evt-12345"), derive_trace_id("evt-12345"))

    def test_derive_trace_id_is_32_hex(self) -> None:
        tid = derive_trace_id("evt-12345")
        self.assertEqual(len(tid), 32)
        self.assertTrue(all(c in "0123456789abcdef" for c in tid))

    def test_distinct_event_ids_distinct_trace_ids(self) -> None:
        self.assertNotEqual(derive_trace_id("evt-1"), derive_trace_id("evt-2"))

    def test_empty_event_id_returns_empty(self) -> None:
        self.assertEqual(derive_trace_id(""), "")

    def test_cross_language_fixture(self) -> None:
        """Same fixture asserted in backend-go internal/trace/trace_test.go."""
        self.assertEqual(
            derive_trace_id("doubleu-trace-fixture-001"),
            "0bc93694d9dd5bdb8e30d9cfa994d333",
        )


class TraceparentTests(unittest.TestCase):
    """W3C traceparent round-trip + malformed input handling."""

    def test_round_trip(self) -> None:
        tid = derive_trace_id("evt-tp")
        header = format_traceparent(tid)
        self.assertTrue(header.startswith(f"00-{tid}-"))
        self.assertEqual(parse_traceparent(header), tid)

    def test_malformed_returns_empty(self) -> None:
        self.assertEqual(parse_traceparent("bogus"), "")
        self.assertEqual(parse_traceparent(""), "")
        self.assertEqual(parse_traceparent(None), "")

    def test_format_empty_returns_empty(self) -> None:
        self.assertEqual(format_traceparent(""), "")


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
