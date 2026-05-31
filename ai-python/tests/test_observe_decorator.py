"""Phase 4: ``@observe_pipeline`` decorator is a no-op without a sink."""

from __future__ import annotations

import unittest

from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.observability.decorators import (
    get_sink,
    observe_pipeline,
    set_sink,
)


class _RecordingSink:
    def __init__(self) -> None:
        self.spans: list[dict] = []

    def record_span(self, trace_id: str, name: str, payload: dict) -> None:
        self.spans.append({"trace_id": trace_id, "name": name, **payload})


@observe_pipeline("test_function")
def _example(x: int) -> int:
    return x + 1


class ObserveDecoratorTests(unittest.TestCase):
    def setUp(self) -> None:
        # Ensure no sink is bound at the start of each test.
        set_sink(None)

    def tearDown(self) -> None:
        set_sink(None)

    def test_decorator_pass_through_without_sink(self) -> None:
        self.assertIsNone(get_sink())
        self.assertEqual(_example(2), 3)

    def test_decorator_records_span_when_sink_bound(self) -> None:
        sink = _RecordingSink()
        set_sink(sink)
        with with_context(LLMCallContext(trace_id="trace-1")):
            self.assertEqual(_example(2), 3)
        self.assertEqual(len(sink.spans), 1)
        self.assertEqual(sink.spans[0]["trace_id"], "trace-1")
        self.assertEqual(sink.spans[0]["name"], "test_function")
        self.assertGreaterEqual(sink.spans[0]["latency_ms"], 0)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
