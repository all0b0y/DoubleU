"""Phase 2: LiteLLM gateway contract tests.

The LiteLLM gateway forwards every request through the proxy and does NOT
implement retry/fallback itself — that is LiteLLM's job.  These tests pin the
header contract (idempotency, tenant, purpose, traceparent) and the cost
header parsing.
"""

from __future__ import annotations

import json
import unittest
from urllib.request import Request

from ai_agent.contract_enums import LLMPurpose
from ai_agent.llm_litellm import LiteLLMGateway
from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.schemas import GatewayRequest, ModelTask


class _RecordingTransport:
    """Stand-in transport that records the request and returns a canned response."""

    def __init__(self, body: dict, headers: dict[str, str] | None = None) -> None:
        self.body = json.dumps(body).encode("utf-8")
        self.response_headers = headers or {}
        self.last_request: Request | None = None
        self.calls = 0

    def __call__(self, request: Request, timeout: float) -> tuple[bytes, dict[str, str]]:
        self.last_request = request
        self.calls += 1
        return self.body, self.response_headers


def _canonical_completion(text: str = "ok", model: str = "draft-primary") -> dict:
    return {
        "model": model,
        "choices": [{"message": {"role": "assistant", "content": text}}],
        "usage": {"prompt_tokens": 7, "completion_tokens": 5},
    }


def _gateway_request(**overrides) -> GatewayRequest:
    base = dict(
        task=ModelTask.DRAFT_REPLY,
        model_name="draft-primary",
        system_prompt="system",
        user_payload="user",
        trace_id="trace-abc",
    )
    base.update(overrides)
    return GatewayRequest(**base)


class HeaderContractTests(unittest.TestCase):
    def setUp(self) -> None:
        self.transport = _RecordingTransport(_canonical_completion())
        self.gw = LiteLLMGateway(
            master_key="sk-test-master",
            base_url="http://litellm-test:4000",
            transport=self.transport,
        )

    def test_litellm_url_used(self) -> None:
        with with_context(LLMCallContext(event_id="evt-1", tenant_id="t-1", purpose=LLMPurpose.DRAFT)):
            self.gw.complete(_gateway_request())
        self.assertIsNotNone(self.transport.last_request)
        self.assertEqual(
            self.transport.last_request.full_url,
            "http://litellm-test:4000/chat/completions",
        )

    def test_idempotency_header_set(self) -> None:
        with with_context(LLMCallContext(event_id="evt-42", purpose=LLMPurpose.DRAFT)):
            self.gw.complete(_gateway_request())
        # urllib lowercases header lookups via get_header.
        self.assertEqual(self.transport.last_request.get_header("X-idempotency-key"), "evt-42")

    def test_purpose_header_set(self) -> None:
        with with_context(LLMCallContext(event_id="e", purpose=LLMPurpose.EXTRACT)):
            self.gw.complete(_gateway_request())
        self.assertEqual(self.transport.last_request.get_header("X-litellm-purpose"), "extract")

    def test_tenant_header(self) -> None:
        with with_context(LLMCallContext(event_id="e", tenant_id="tenant:user:abc", purpose=LLMPurpose.DRAFT)):
            self.gw.complete(_gateway_request())
        self.assertEqual(
            self.transport.last_request.get_header("X-litellm-tenant-id"),
            "tenant:user:abc",
        )

    def test_traceparent_header_set_when_ctx_has_trace(self) -> None:
        with with_context(LLMCallContext(event_id="e", trace_id="0123456789abcdef0123456789abcdef", purpose=LLMPurpose.DRAFT)):
            self.gw.complete(_gateway_request())
        tp = self.transport.last_request.get_header("Traceparent")
        self.assertTrue(tp.startswith("00-0123456789abcdef0123456789abcdef-"))


class NoPythonRetryTests(unittest.TestCase):
    """LiteLLM owns retries; the Python gateway must not retry on its own."""

    def test_no_python_retry_on_5xx(self) -> None:
        import urllib.error

        def boom(request, timeout):
            raise urllib.error.HTTPError(
                request.full_url, 503, "Service Unavailable", {}, fp=None,
            )

        gw = LiteLLMGateway(master_key="k", transport=boom)
        with with_context(LLMCallContext(event_id="e")):
            with self.assertRaises(RuntimeError) as cm:
                gw.complete(_gateway_request())
        self.assertIn("503", str(cm.exception))


class TraceMetadataTests(unittest.TestCase):
    """Fix 3: trace_id must be forwarded in the request body so LiteLLM uses
    the same id when logging to Langfuse."""

    def _make_gw(self) -> tuple[LiteLLMGateway, _RecordingTransport]:
        transport = _RecordingTransport(_canonical_completion())
        gw = LiteLLMGateway(master_key="k", base_url="http://x:4000", transport=transport)
        return gw, transport

    def test_metadata_trace_id_propagated(self) -> None:
        """trace_id from LLMCallContext must appear in metadata.trace_id in the body."""
        gw, transport = self._make_gw()
        with with_context(LLMCallContext(event_id="e", trace_id="abc123", purpose=LLMPurpose.DRAFT)):
            gw.complete(_gateway_request(trace_id="abc123"))
        body = json.loads(transport.last_request.data)
        self.assertEqual(body["metadata"]["trace_id"], "abc123")

    def test_metadata_purpose_tag_set(self) -> None:
        """Purpose value must appear in metadata.tags."""
        gw, transport = self._make_gw()
        with with_context(LLMCallContext(event_id="e", trace_id="t1", purpose=LLMPurpose.SUMMARIZE)):
            gw.complete(_gateway_request(trace_id="t1"))
        body = json.loads(transport.last_request.data)
        self.assertIn("summarize", body["metadata"]["tags"])

    def test_no_metadata_when_no_trace_id(self) -> None:
        """When trace_id is empty, metadata block must be absent to keep payloads minimal."""
        gw, transport = self._make_gw()
        with with_context(LLMCallContext(event_id="e", trace_id="", purpose=LLMPurpose.DRAFT)):
            gw.complete(_gateway_request(trace_id=""))
        body = json.loads(transport.last_request.data)
        self.assertNotIn("metadata", body)


class CostHeaderTests(unittest.TestCase):
    def test_cost_header_parsed(self) -> None:
        transport = _RecordingTransport(_canonical_completion(), headers={"x-litellm-response-cost": "0.0042"})
        gw = LiteLLMGateway(master_key="k", transport=transport)
        with with_context(LLMCallContext(event_id="e", purpose=LLMPurpose.DRAFT)):
            response = gw.complete(_gateway_request())
        self.assertAlmostEqual(response.cost_usd, 0.0042)

    def test_cost_header_absent_defaults_zero(self) -> None:
        transport = _RecordingTransport(_canonical_completion())
        gw = LiteLLMGateway(master_key="k", transport=transport)
        with with_context(LLMCallContext(event_id="e", purpose=LLMPurpose.DRAFT)):
            response = gw.complete(_gateway_request())
        self.assertEqual(response.cost_usd, 0.0)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
