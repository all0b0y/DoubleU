"""Phase 5: shadow runner + divergence diff contract tests."""

from __future__ import annotations

import unittest

from ai_agent.config.flags import FeatureFlags
from ai_agent.contract_enums import LLMPurpose
from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.schemas import GatewayRequest, GatewayResponse, ModelTask, Usage
from ai_agent.shadow import metrics
from ai_agent.shadow.diff import gateway_diff
from ai_agent.shadow.runner import ShadowRunner


class _FakeGateway:
    def __init__(self, text: str) -> None:
        self.text = text
        self.calls = 0

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        self.calls += 1
        return GatewayResponse(text=self.text, model_used="fake", usage=Usage())


def _request() -> GatewayRequest:
    return GatewayRequest(
        task=ModelTask.DRAFT_REPLY,
        model_name="m",
        system_prompt="s",
        user_payload="u",
    )


class DiffClassificationTests(unittest.TestCase):
    def test_identical_text_matches(self) -> None:
        d = gateway_diff("hello world", "hello world", LLMPurpose.DRAFT)
        self.assertEqual(d.classification, "match")
        self.assertGreaterEqual(d.score, 0.85)

    def test_very_different_text_diverges(self) -> None:
        d = gateway_diff("hello world", "completely different sentence", LLMPurpose.DRAFT)
        self.assertEqual(d.classification, "divergent")

    def test_extract_uses_jaccard(self) -> None:
        d = gateway_diff(
            '{"facts": ["a", "b"]}',
            '{"facts": ["a", "b"]}',
            LLMPurpose.EXTRACT,
        )
        self.assertEqual(d.classification, "match")

    def test_embed_identity_check(self) -> None:
        d_same = gateway_diff("vector-bytes", "vector-bytes", LLMPurpose.EMBED)
        d_diff = gateway_diff("vector-bytes", "other-bytes", LLMPurpose.EMBED)
        self.assertEqual(d_same.classification, "match")
        self.assertEqual(d_diff.classification, "divergent")


class ShadowRunnerTests(unittest.TestCase):
    def setUp(self) -> None:
        metrics.reset()
        self.primary = _FakeGateway("new primary text")
        self.legacy = _FakeGateway("legacy text")

    def test_shadow_returns_primary_by_default(self) -> None:
        runner = ShadowRunner(self.primary, self.legacy, FeatureFlags())
        with with_context(LLMCallContext(trace_id="t")):
            resp = runner.complete(_request())
        self.assertEqual(resp.text, "new primary text")
        self.assertEqual(self.primary.calls, 1)
        self.assertEqual(self.legacy.calls, 1)

    def test_shadow_mode_only_returns_legacy(self) -> None:
        runner = ShadowRunner(
            self.primary, self.legacy, FeatureFlags(shadow_mode_only=True),
        )
        with with_context(LLMCallContext(trace_id="t")):
            resp = runner.complete(_request())
        self.assertEqual(resp.text, "legacy text")
        self.assertEqual(self.primary.calls, 1)
        self.assertEqual(self.legacy.calls, 1)

    def test_cutover_skips_legacy(self) -> None:
        runner = ShadowRunner(
            self.primary, self.legacy, FeatureFlags(cutover=True),
        )
        with with_context(LLMCallContext(trace_id="t")):
            resp = runner.complete(_request())
        self.assertEqual(resp.text, "new primary text")
        self.assertEqual(self.primary.calls, 1)
        self.assertEqual(self.legacy.calls, 0)

    def test_legacy_failure_returns_primary(self) -> None:
        class _BrokenLegacy:
            def complete(self, _request):
                raise RuntimeError("legacy down")

        runner = ShadowRunner(self.primary, _BrokenLegacy(), FeatureFlags())
        with with_context(LLMCallContext(trace_id="t")):
            resp = runner.complete(_request())
        self.assertEqual(resp.text, "new primary text")

    def test_divergence_metric_is_recorded(self) -> None:
        runner = ShadowRunner(self.primary, self.legacy, FeatureFlags())
        with with_context(LLMCallContext(trace_id="t")):
            runner.complete(_request())
        snap = metrics.snapshot()
        self.assertIn("draft", snap)
        # Either match/close/divergent must have been incremented once.
        self.assertEqual(sum(snap["draft"].values()), 1)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
