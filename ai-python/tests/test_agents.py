"""Behavioural tests for the pydantic-ai agent runtime.

Models are FunctionModel/TestModel instances (the pydantic-ai equivalent of
MockLLMGateway): tests assert the contract behaviour — outcome mapping, the
critic gate, privacy enforcement and deterministic-fact merging — never the
prompt internals.
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest
from pydantic_ai.messages import ModelResponse, ToolCallPart
from pydantic_ai.models.function import AgentInfo, FunctionModel

from ai_agent.agents.deps import AgentDeps
from ai_agent.agents.memory_tools import save_fact, search_facts, suggest_profile_update
from ai_agent.agents.runtime import AgentRuntime
from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.schemas import (
    AIProcessMessageRequest,
    ChatMode,
    ChatSettings,
    ChatSummaryRequest,
    NormalizedTelegramEvent,
    PrivacyFlag,
    RerankFactsRequest,
    RiskLevel,
    TopicTaggingRequest,
    TypedExtractionRequest,
)


def structured(tool_args: dict) -> FunctionModel:
    """A model that always answers with the given structured output."""

    def respond(messages, info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args=tool_args)])

    return FunctionModel(respond)


def process_request(text: str = "Во сколько встречаемся завтра?") -> AIProcessMessageRequest:
    return AIProcessMessageRequest(
        request_id="r1",
        event=NormalizedTelegramEvent(
            event_id="e1",
            tenant_id="tenant:user:77",
            chat_id="42",
            telegram_update_id=1,
            business_connection_id="",
            source_message_id="m1",
            sender_id="s1",
            message_text=text,
            message_type="text",
            created_at="2026-06-11T10:00:00Z",
        ),
        chat_settings=ChatSettings(
            tenant_id="tenant:user:77", chat_id="42", mode=ChatMode.MANUAL, privacy_flags=()
        ),
    )


def make_runtime(**models) -> AgentRuntime:
    defaults = {
        "draft_model": structured(
            {"reply": "Завтра в 19:00, как договаривались.", "intent": "scheduling",
             "options": [], "confidence": 0.9, "topic": "время встречи"}
        ),
        "critic_model": structured({"verdict": "approve", "issues": [], "revised_reply": ""}),
        "extract_model": structured({"facts": [], "profile_suggestions": []}),
        "summary_model": structured(
            {"summary": "Обсуждали встречу.", "action_items": ["встреча завтра в 19:00"],
             "recommendations": []}
        ),
    }
    defaults.update(models)
    return AgentRuntime(run_timeout_seconds=10, **defaults)


def test_draft_outcome_maps_structured_output() -> None:
    runtime = make_runtime()
    outcome = runtime.draft_outcome(process_request(), RiskLevel.LOW)
    assert outcome.text == "Завтра в 19:00, как договаривались."
    assert outcome.intent == "scheduling"
    assert outcome.confidence == 0.9
    assert outcome.topic == "время встречи"
    # Low risk + confident draft: critic must NOT run (no extra LLM latency).
    assert "+critic" not in outcome.model_used


def test_critic_reviews_medium_risk_and_revises() -> None:
    runtime = make_runtime(
        critic_model=structured(
            {"verdict": "revise", "issues": ["обещает скидку, которой нет в контексте"],
             "revised_reply": "Уточню детали и вернусь к вам."}
        )
    )
    outcome = runtime.draft_outcome(process_request("про оплату"), RiskLevel.MEDIUM)
    assert outcome.text == "Уточню детали и вернусь к вам."
    assert outcome.model_used.endswith("+critic")
    # A revised draft must not auto-send on its original self-score.
    assert outcome.confidence <= 0.5


def test_critic_triggers_on_low_confidence_even_at_low_risk() -> None:
    runtime = make_runtime(
        draft_model=structured(
            {"reply": "Не уверен, что понял.", "intent": "general_message",
             "options": [], "confidence": 0.3, "topic": ""}
        )
    )
    outcome = runtime.draft_outcome(process_request(), RiskLevel.LOW)
    assert outcome.model_used.endswith("+critic")


def test_preflight_gates_shared_with_legacy_path() -> None:
    runtime = make_runtime()
    request = process_request()
    silent = AIProcessMessageRequest(
        request_id=request.request_id,
        event=request.event,
        chat_settings=ChatSettings(
            tenant_id="tenant:user:77", chat_id="42", mode=ChatMode.SILENT, privacy_flags=()
        ),
    )
    outcome = runtime.draft_outcome(silent, RiskLevel.LOW)
    assert outcome.text == "" and outcome.model_used == "none"
    # Critical risk never reaches the LLM (ARCH-1).
    outcome = runtime.draft_outcome(request, RiskLevel.CRITICAL)
    assert outcome.text == "" and outcome.model_used == "none"


def test_extraction_merges_deterministic_and_blocks_privacy() -> None:
    runtime = make_runtime(
        extract_model=structured(
            {"facts": [{"fact_text": "учится в МГУ на медицинском", "fact_type": "biographical",
                        "subject": "contact", "confidence": 0.9}],
             "profile_suggestions": [{"field_name": "language", "suggested_value": "ru",
                                      "rationale": "пишет по-русски"}]}
        )
    )

    class OneFactExtractor:
        def extract(self, text, privacy_flags):
            return SimpleNamespace(
                facts=(
                    SimpleNamespace(
                        fact_text="день рождения 5 мая", fact_type="event",
                        subject="contact", scope_kind="chat", confidence=0.8,
                    ),
                )
            )

    request = TypedExtractionRequest(
        message_text="Я учусь в МГУ, кстати др у меня 5 мая",
        tenant_id="tenant:user:77",
        chat_id="42",
        privacy_flags=(),
    )
    response = runtime.extract_facts_typed(OneFactExtractor(), request)
    texts = {f.fact_text for f in response.facts}
    assert "учится в МГУ на медицинском" in texts
    assert "день рождения 5 мая" in texts  # deterministic fact fills the gap
    assert response.llm_facts_count == 1 and response.deterministic_facts_count == 1
    assert response.profile_suggestions[0].field_name == "language"

    blocked = runtime.extract_facts_typed(
        OneFactExtractor(),
        TypedExtractionRequest(
            message_text="x", tenant_id="t", chat_id="c",
            privacy_flags=(PrivacyFlag.NO_MEMORY,),
        ),
    )
    assert blocked.blocked_by_privacy and blocked.facts == ()


def test_summarize_respects_no_summary_flag() -> None:
    runtime = make_runtime()
    blocked = runtime.summarize_chat(
        ChatSummaryRequest(messages=("a", "b"), privacy_flags=(PrivacyFlag.NO_SUMMARY,))
    )
    assert blocked.blocked_by_privacy

    response = runtime.summarize_chat(ChatSummaryRequest(messages=("обсуждали встречу",)))
    assert response.summary == "Обсуждали встречу."
    assert response.action_items == ("встреча завтра в 19:00",)


def test_memory_tools_enforce_privacy_and_degrade_without_pool() -> None:
    """Privacy is enforced INSIDE the tools — the second line of defence."""
    no_memory = SimpleNamespace(
        deps=AgentDeps(tenant_id="t", chat_id="c", privacy_flags=("no_memory",))
    )
    stateless = SimpleNamespace(deps=AgentDeps(tenant_id="t", chat_id="c"))

    assert asyncio.run(search_facts(no_memory, "query")) == []
    assert asyncio.run(search_facts(stateless, "query")) == []
    assert "no_memory" in asyncio.run(save_fact(no_memory, "факт о собеседнике", "preference", "contact", 0.9))
    assert "unavailable" in asyncio.run(save_fact(stateless, "факт о собеседнике", "preference", "contact", 0.9))
    assert "no_memory" in asyncio.run(suggest_profile_update(no_memory, "tone", "casual", ""))


def test_invalid_structured_output_falls_back_to_error_contract() -> None:
    """A model that cannot produce a valid DraftResult surfaces as RuntimeError —
    the same contract the legacy gateway path uses for error_fallback."""

    def broken(messages, info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args={"reply": ""})])

    runtime = make_runtime(draft_model=FunctionModel(broken))
    with pytest.raises(RuntimeError):
        runtime.draft_outcome(process_request(), RiskLevel.LOW)


def test_tag_topic_returns_structured_label() -> None:
    runtime = make_runtime(topic_model=structured({"topic": "планирование встречи"}))
    response = runtime.tag_topic(
        TopicTaggingRequest(message_text="во сколько завтра встречаемся?", tenant_id="t", chat_id="c")
    )
    assert response.topic == "планирование встречи"

    # Empty input is skipped without a model call.
    assert runtime.tag_topic(TopicTaggingRequest(message_text="   ")).topic == ""


def test_rerank_filters_to_valid_ids_in_model_order() -> None:
    runtime = make_runtime(rerank_model=structured({"ranked_ids": [3, 99, 1]}))
    facts = ({"id": 1, "text": "a"}, {"id": 2, "text": "b"}, {"id": 3, "text": "c"})
    response = runtime.rerank_facts(RerankFactsRequest(query="x", facts=facts, top_k=5))
    # Hallucinated id 99 is dropped; the rest keep the model's order.
    assert response.ranked_ids == (3, 1)


def test_rerank_falls_back_to_input_order_when_empty() -> None:
    runtime = make_runtime(rerank_model=structured({"ranked_ids": []}))
    facts = ({"id": 7, "text": "a"}, {"id": 8, "text": "b"}, {"id": 9, "text": "c"})
    response = runtime.rerank_facts(RerankFactsRequest(query="x", facts=facts, top_k=2))
    assert response.ranked_ids == (7, 8)


class _FakeSink:
    """Capturing stand-in for LangfuseSink — records generation events."""

    def __init__(self) -> None:
        self.generations: list[dict] = []
        self.traces: list[str] = []

    def record_trace(self, trace_id, payload):
        self.traces.append(trace_id)

    def record_span(self, trace_id, name, payload):
        pass

    def record_generation(self, trace_id, model, prompt, completion, privacy_flags=(), usage=None, cost_usd=0.0):
        self.generations.append({"trace_id": trace_id, "model": model, "usage": usage or {}})


def test_draft_run_records_generation_to_sink() -> None:
    sink = _FakeSink()
    runtime = make_runtime(sink=sink)
    with with_context(LLMCallContext(trace_id="trace-1")):
        runtime.draft_outcome(process_request(), RiskLevel.LOW)
    assert sink.generations, "agent run must record a generation when a sink is wired"
    assert sink.generations[0]["trace_id"] == "trace-1"
    assert "usage" in sink.generations[0]
