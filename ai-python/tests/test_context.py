"""Phase 0: LLMCallContext + ContextVar binding tests."""

from __future__ import annotations

import threading
import unittest

from ai_agent.observability.context import (
    LLMCallContext,
    current_context,
    with_context,
)


class CallContextTests(unittest.TestCase):
    def test_default_context_is_empty(self) -> None:
        ctx = current_context()
        self.assertEqual(ctx.trace_id, "")
        self.assertEqual(ctx.redactions, {})

    def test_with_context_round_trip(self) -> None:
        outer = current_context()
        with with_context(LLMCallContext(trace_id="abc")):
            self.assertEqual(current_context().trace_id, "abc")
        self.assertEqual(current_context().trace_id, outer.trace_id)

    def test_nested_with_context_restores_parent(self) -> None:
        with with_context(LLMCallContext(trace_id="outer")):
            with with_context(LLMCallContext(trace_id="inner")):
                self.assertEqual(current_context().trace_id, "inner")
            self.assertEqual(current_context().trace_id, "outer")

    def test_threads_do_not_leak_context(self) -> None:
        """ContextVar binding is per-thread; one thread must not see another's id."""
        seen: list[str] = []
        errors: list[BaseException] = []

        def worker(tid: str) -> None:
            try:
                with with_context(LLMCallContext(trace_id=tid)):
                    seen.append(current_context().trace_id)
            except BaseException as exc:  # pragma: no cover
                errors.append(exc)

        threads = [threading.Thread(target=worker, args=(f"trace-{i}",)) for i in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertFalse(errors)
        self.assertEqual(sorted(seen), [f"trace-{i}" for i in range(4)])


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
