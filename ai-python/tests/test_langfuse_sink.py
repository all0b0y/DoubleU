"""Phase 4: Langfuse async sink contract tests."""

from __future__ import annotations

import json
import threading
import time
import unittest
from urllib.request import Request

from ai_agent.contract_enums import PrivacyFlag
from ai_agent.observability.langfuse_sink import LangfuseSink, _SinkEvent, _to_batch_item


class _CollectingUploader:
    """Test uploader that records every event."""

    def __init__(self) -> None:
        self.events: list = []
        self.lock = threading.Lock()

    def __call__(self, event) -> None:
        with self.lock:
            self.events.append(event)


class FireAndForgetTests(unittest.TestCase):
    def test_records_trace_and_flushes(self) -> None:
        uploader = _CollectingUploader()
        sink = LangfuseSink(uploader=uploader)
        try:
            sink.record_trace("trace-1", {"tenant_id": "t-1"})
            sink.record_span("trace-1", "input_safety", {"latency_ms": 1.2})
            sink.record_generation("trace-1", "draft-primary", "hi", "hello", cost_usd=0.001)
            # Give the worker thread time to drain the queue.
            for _ in range(50):
                if len(uploader.events) >= 3:
                    break
                time.sleep(0.01)
            self.assertGreaterEqual(len(uploader.events), 3)
        finally:
            sink.stop()

    def test_record_methods_never_raise(self) -> None:
        def boom(_event) -> None:
            raise RuntimeError("upload failed")

        sink = LangfuseSink(uploader=boom)
        try:
            # Even with a broken uploader, record_* must not propagate.
            sink.record_trace("trace-1", {})
            sink.record_span("trace-1", "name", {})
            sink.record_generation("trace-1", "m", "p", "c")
            sink.record_score("trace-1", "name", 1.0)
            time.sleep(0.05)
        finally:
            sink.stop()


class OverflowTests(unittest.TestCase):
    def test_overflow_drops_and_counts(self) -> None:
        # Tiny queue; never start the worker so the queue fills.
        sink = LangfuseSink(uploader=None, queue_size=2)
        sink.record_trace("trace-1", {"k": 1})
        sink.record_trace("trace-2", {"k": 2})
        # Third event should overflow.
        sink.record_trace("trace-3", {"k": 3})
        self.assertEqual(sink.dropped, 1)


class PrivacyHashingTests(unittest.TestCase):
    def test_no_training_replaces_prompt_with_hash(self) -> None:
        uploader = _CollectingUploader()
        sink = LangfuseSink(uploader=uploader)
        try:
            sink.record_generation(
                "trace-1",
                "draft-primary",
                "secret prompt",
                "secret reply",
                privacy_flags=(PrivacyFlag.NO_TRAINING,),
            )
            for _ in range(50):
                if uploader.events:
                    break
                time.sleep(0.01)
            self.assertTrue(uploader.events)
            payload = uploader.events[0].payload
            self.assertTrue(payload["prompt"].startswith("sha256:"))
            self.assertEqual(payload["completion"], "")
            self.assertNotIn("secret prompt", payload["prompt"])
        finally:
            sink.stop()


class ConfiguredFlagTests(unittest.TestCase):
    def test_configured_returns_false_when_no_uploader(self) -> None:
        sink = LangfuseSink(uploader=None)
        self.assertFalse(sink.configured())

    def test_configured_returns_true_when_uploader_set(self) -> None:
        sink = LangfuseSink(uploader=lambda e: None)
        try:
            self.assertTrue(sink.configured())
        finally:
            sink.stop()


class BatchItemTests(unittest.TestCase):
    """Fix 2: _to_batch_item maps every _SinkEvent kind to a Langfuse batch envelope."""

    def test_generation_maps_to_generation_create(self) -> None:
        event = _SinkEvent(kind="generation", payload={
            "trace_id": "t1", "model": "draft-primary",
            "prompt": "hello", "completion": "hi",
            "usage": {"input_tokens": 10, "output_tokens": 5},
            "cost_usd": 0.001,
        })
        item = _to_batch_item(event)
        self.assertIsNotNone(item)
        self.assertEqual(item["type"], "generation-create")
        body = item["body"]
        self.assertEqual(body["traceId"], "t1")
        self.assertEqual(body["model"], "draft-primary")
        self.assertEqual(body["usage"]["input"], 10)
        self.assertAlmostEqual(body["calculatedTotalCost"], 0.001)
        # Langfuse rejects an observation without a body id (400) — this is the
        # field whose absence silently dropped every app generation.
        self.assertEqual(body["id"], item["id"])
        self.assertTrue(body["id"])

    def test_span_maps_to_span_create(self) -> None:
        event = _SinkEvent(kind="span", payload={"trace_id": "t2", "name": "input_safety", "passed": True})
        item = _to_batch_item(event)
        self.assertEqual(item["type"], "span-create")
        self.assertEqual(item["body"]["name"], "input_safety")
        self.assertEqual(item["body"]["traceId"], "t2")
        self.assertEqual(item["body"]["id"], item["id"])
        self.assertTrue(item["body"]["id"])

    def test_trace_maps_to_trace_create_with_body_id(self) -> None:
        event = _SinkEvent(kind="trace", payload={"trace_id": "t3", "name": "draft", "tenant_id": "u1"})
        item = _to_batch_item(event)
        self.assertEqual(item["type"], "trace-create")
        self.assertEqual(item["body"]["id"], "t3")
        self.assertEqual(item["body"]["metadata"]["tenant_id"], "u1")

    def test_score_maps_to_score_create(self) -> None:
        event = _SinkEvent(kind="score", payload={"trace_id": "t4", "name": "owner_approval", "value": 1.0})
        item = _to_batch_item(event)
        self.assertEqual(item["type"], "score-create")
        self.assertEqual(item["body"]["value"], 1.0)
        self.assertEqual(item["body"]["id"], item["id"])
        self.assertTrue(item["body"]["id"])

    def test_unknown_kind_returns_none(self) -> None:
        event = _SinkEvent(kind="unknown", payload={})
        self.assertIsNone(_to_batch_item(event))


class HttpUploaderTests(unittest.TestCase):
    """HttpUploader sends correct Basic auth and POST body to Langfuse ingestion API."""

    def test_posts_to_ingestion_endpoint_with_basic_auth(self) -> None:
        import base64
        from ai_agent.observability.langfuse_sink import HttpUploader

        captured: dict = {}

        def fake_urlopen(req: Request, timeout: float):
            captured["url"] = req.full_url
            captured["auth"] = req.get_header("Authorization")
            captured["body"] = json.loads(req.data)

            class _Resp:
                def read(self): return b""
                def __enter__(self): return self
                def __exit__(self, *_): pass

            return _Resp()

        import ai_agent.observability.langfuse_sink as mod
        orig = mod.urllib.request.urlopen
        mod.urllib.request.urlopen = fake_urlopen
        try:
            uploader = HttpUploader("http://lf:3000", "pk-test", "sk-test")
            event = _SinkEvent(kind="trace", payload={"trace_id": "t99", "name": "r"})
            uploader(event)
        finally:
            mod.urllib.request.urlopen = orig

        expected_auth = "Basic " + base64.b64encode(b"pk-test:sk-test").decode()
        self.assertEqual(captured["url"], "http://lf:3000/api/public/ingestion")
        self.assertEqual(captured["auth"], expected_auth)
        self.assertEqual(len(captured["body"]["batch"]), 1)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
