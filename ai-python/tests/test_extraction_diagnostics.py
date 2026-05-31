"""Observability tests for the typed fact-extraction pipeline.

A returned empty fact set used to be indistinguishable from a silent failure.
``extract_facts_typed`` now surfaces ``model_used`` / ``raw_output`` and the
LLM-vs-deterministic counts so the Go backend can persist an extraction_log row.
"""

from __future__ import annotations

import unittest

from ai_agent.classifiers import TypedMemoryExtractor
from ai_agent.llm import GatewayResponse, ModelRouter
from ai_agent.pipelines import extraction as extraction_pipeline
from ai_agent.schemas import (
    GatewayRequest,
    PrivacyFlag,
    TypedExtractionRequest,
    Usage,
)
from ai_agent.security import PromptBuilder


class _ScriptedGateway:
    """Gateway returning a fixed completion, recording the model name used."""

    def __init__(self, text: str, model: str = "scripted-model") -> None:
        self._text = text
        self._model = model

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        return GatewayResponse(text=self._text, model_used=self._model, usage=Usage())


def _run(gateway_text: str, message: str) -> object:
    router = ModelRouter(_ScriptedGateway(gateway_text))
    return extraction_pipeline.extract_facts_typed(
        TypedMemoryExtractor(),
        PromptBuilder(),
        router,
        TypedExtractionRequest(message_text=message, tenant_id="t", chat_id="c"),
    )


class ExtractionDiagnosticsTests(unittest.TestCase):
    def test_empty_result_still_reports_model_and_raw_output(self) -> None:
        """An empty fact list is distinguishable from a failure: model + raw survive."""
        resp = _run("[]", "просто болтаем ни о чём")
        self.assertEqual(resp.facts, ())
        self.assertEqual(resp.model_used, "scripted-model")
        self.assertEqual(resp.raw_output, "[]")
        self.assertEqual(resp.llm_facts_count, 0)

    def test_valid_facts_are_counted(self) -> None:
        """A well-formed LLM fact is parsed and reflected in llm_facts_count."""
        payload = (
            '[{"fact_text":"studies medicine at university","fact_type":"biographical",'
            '"subject":"owner","scope_kind":"owner_global","confidence":0.9}]'
        )
        resp = _run(payload, "я учусь на медика в универе")
        self.assertEqual(resp.llm_facts_count, 1)
        self.assertEqual(resp.facts[0].fact_text, "studies medicine at university")
        self.assertIn("medicine", resp.raw_output)

    def test_malformed_json_surfaces_error_in_raw_output(self) -> None:
        """Unparseable LLM output is captured as an ERROR raw_output, not lost."""
        resp = _run("not json at all {", "что-то")
        self.assertEqual(resp.llm_facts_count, 0)
        self.assertTrue(resp.raw_output.startswith("ERROR:"))

    def test_no_memory_blocks_and_skips_diagnostics(self) -> None:
        """no_memory short-circuits before any LLM call (blocked_by_privacy=True)."""
        router = ModelRouter(_ScriptedGateway("[]"))
        resp = extraction_pipeline.extract_facts_typed(
            TypedMemoryExtractor(),
            PromptBuilder(),
            router,
            TypedExtractionRequest(
                message_text="секрет",
                privacy_flags=(PrivacyFlag.NO_MEMORY,),
            ),
        )
        self.assertTrue(resp.blocked_by_privacy)
        self.assertEqual(resp.facts, ())


if __name__ == "__main__":
    unittest.main()
