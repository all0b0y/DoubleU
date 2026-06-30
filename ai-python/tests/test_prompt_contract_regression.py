"""Phase 2.8: Prompt Contract / Langfuse regression gate.

A minimal, source-grounded gate against `.claude/analyze.md`'s two replayed
Langfuse scenarios (trace 8891a8b0-e319-44e6-93f4-e1d73d37d067 and its
"Расскажи мне как устроена наша планета" sibling, both 2026-06-29 ~15:39-15:42
MSK), plus the structural properties the Phase 2 plan's 2.8 acceptance list
calls out:

* replay fixtures for the two analyze.md scenarios;
* prompt snapshots for gpt/reasoning/agentic, all through the XML
  ``<prompt_contract>`` envelope;
* rich/plain ``<output_format>`` snapshots;
* tool-call budgets;
* preflight decision snapshots;
* empty ``auto_reply``/``output_safety`` observations are never mistaken for
  empty LLM calls — real prompts live in the ``generation`` events tagged
  ``app.llm.complete``.

This file does not re-prove mechanics already covered elsewhere (dedup,
malformed-arg tolerance, family-rendering internals — see test_agents.py /
test_prompt_renderers.py); it locks in the end-to-end shape for the two
concrete regression scenarios plus the cross-cutting snapshot/gate
properties the plan calls out as a single, named deliverable.
"""

from __future__ import annotations

import dataclasses
import json

import pytest
from pydantic_ai.messages import ModelResponse, TextPart, ToolCallPart
from pydantic_ai.models.function import AgentInfo, FunctionModel

from agent_helpers import make_test_runtime, prompted
from ai_agent.agents.models import AgentTask, model_name_for
from ai_agent.agents.preflight import decide_preflight
from ai_agent.agents.runtime import _DRAFT_TOOL_CALLS_LIMIT, _ORCHESTRATOR_TOOL_CALLS_LIMIT
from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.observability.langfuse_sink import _SinkEvent, _to_batch_item
from ai_agent.prompts import library as prompt_library
from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.renderers import render as render_prompt
from ai_agent.schemas import (
    AIProcessMessageRequest,
    AIRoleData,
    ChatMode,
    ChatSettings,
    NormalizedTelegramEvent,
    RiskLevel,
)

# ---------------------------------------------------------------------------
# Fixtures: the two .claude/analyze.md scenarios, replayed
# ---------------------------------------------------------------------------


def _physics_teacher_role() -> AIRoleData:
    """Mirrors the "Учитель физики" role from the replayed trace's system prompt."""
    return AIRoleData(
        name="Учитель физики",
        description="Помощник отвечает строго на вопросы по физике и объясняет понятно.",
        goals="Помочь пользователю понять физику.",
        constraints="Помощник отвечает строго на вопросы по физике, не выходит за рамки темы.",
        speak_as_owner=False,
        tool_settings={"memory.get_recent_messages": True, "memory.get_contact_profile": True},
    )


def _request(
    text: str, *, ai_role: AIRoleData | None = None, event_id: str = "analyze-1"
) -> AIProcessMessageRequest:
    return AIProcessMessageRequest(
        request_id=event_id,
        event=NormalizedTelegramEvent(
            event_id=event_id,
            tenant_id="tenant:user:analyze",
            chat_id="analyze-chat",
            telegram_update_id=1,
            business_connection_id="",
            source_message_id="m1",
            sender_id="s1",
            message_text=text,
            message_type="text",
            created_at="2026-06-29T15:39:24Z",
        ),
        chat_settings=ChatSettings(
            tenant_id="tenant:user:analyze", chat_id="analyze-chat", mode=ChatMode.MANUAL
        ),
        ai_role=ai_role,
    )


def physics_teacher_request() -> AIProcessMessageRequest:
    """Scenario 1: "Как ты мне можешь помочь..." with the physics-teacher role."""
    return _request("Как ты мне можешь помочь...", ai_role=_physics_teacher_role())


def general_knowledge_request() -> AIProcessMessageRequest:
    """Scenario 2: "Расскажи мне как устроена наша планета" — no role, no chat-memory need."""
    return _request("Расскажи мне как устроена наша планета", event_id="analyze-2")


def physics_teacher_request_with_recall_signal() -> AIProcessMessageRequest:
    """Same scenario/role, but with an explicit memory-recall signal.

    The 2.7 preflight correctly skips the orchestrator for the verbatim
    analyze.md text (no memory/schedule/interaction keyword) — that IS the
    regression fix being validated by TestGeneralKnowledgeScenarioReplay
    below. Tests that need to exercise the orchestrator's own mechanics
    (duplicate-final_result tolerance, call budget) for this role use this
    variant so two-stage drafting actually engages.
    """
    return _request(
        "Ты можешь вспомнить, как мне лучше помочь с физикой?", ai_role=_physics_teacher_role()
    )


def draft_output(args: dict) -> ModelResponse:
    return ModelResponse(parts=[TextPart(json.dumps(args))])


class TestPhysicsTeacherScenarioReplay:
    """Replays trace 8891a8b0-e319-44e6-93f4-e1d73d37d067."""

    def test_writer_system_prompt_carries_role_task_constraints_in_xml_envelope(self) -> None:
        captured: dict[str, str] = {}

        def writer(messages, info: AgentInfo) -> ModelResponse:
            captured["instructions"] = info.instructions or ""
            return draft_output(
                {"reply": "Я — Учитель физики, отвечаю на вопросы по физике.",
                 "intent": "general_message", "options": [], "confidence": 0.9, "topic": ""}
            )

        runtime = make_test_runtime(two_stage_draft=False, draft_model=FunctionModel(writer))
        runtime.draft_outcome(physics_teacher_request(), RiskLevel.LOW)

        rendered = captured["instructions"]
        # PromptedOutput prepends its own JSON-schema preamble before the
        # dynamic-instructions hook output, so the trust-boundary wrapper and
        # the XML envelope are present but not necessarily first.
        assert "You are the DoubleU AI service." in rendered
        assert "<prompt_contract>" in rendered
        assert "Учитель физики" in rendered
        assert "Помочь пользователю понять физику" in rendered
        assert "<constraints>" in rendered

    def test_no_duplicate_final_result_for_a_transient_validation_failure(self) -> None:
        """The .claude/analyze.md "duplicate final_result" observation is
        pydantic-ai's own retries=2 succeeding after a validation failure —
        never two real terminal effects. See test_agents.py for the original
        2.5 investigation; replayed here against the actual scenario text."""
        calls = {"n": 0}

        def orchestrator(messages, info: AgentInfo) -> ModelResponse:
            calls["n"] += 1
            if calls["n"] == 1:
                return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args={
                    "gathered_context": [], "suggested_intent": "x" * 200, "notes": "",
                })])
            return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args={
                "gathered_context": ["профиль учтён"], "suggested_intent": "question", "notes": "",
            })])

        request = physics_teacher_request_with_recall_signal()
        runtime = make_test_runtime(
            two_stage_draft=True,
            orchestrator_model=FunctionModel(orchestrator),
            draft_model=prompted({
                "reply": "Физика — наука о природе.",
                "intent": "general_message", "options": [], "confidence": 0.9, "topic": "",
            }),
        )

        outcome = runtime.draft_outcome(request, RiskLevel.LOW)

        assert calls["n"] == 2  # one self-healed validation retry, never a second real call
        assert outcome.text == "Физика — наука о природе."

    def test_preflight_decision_snapshot(self) -> None:
        request = physics_teacher_request()
        decision = decide_preflight(
            request,
            orchestrator_slug=model_name_for(AgentTask.ORCHESTRATE),
            writer_slug=model_name_for(AgentTask.DRAFT),
        )
        # "Как ты мне можешь помочь" carries no memory-recall/schedule keyword
        # — the role's own description already answers it from trusted
        # context, matching the plan's "кто ты и что умеешь" acceptance case.
        # This IS the regression fix: the original trace fired four memory
        # tool calls (incl. malformed get_contact_profile({}"")) for this
        # exact turn — see TestGeneralKnowledgeScenarioReplay for the other
        # replayed analyze.md turn with the same shape.
        assert decision.needs_orchestrator is False
        assert decision.tool_groups - {"delivery"} == frozenset()
        assert decision.structured_output_required is True


class TestGeneralKnowledgeScenarioReplay:
    """Replays the "Расскажи мне как устроена наша планета" trace: the
    Langfuse evidence for this turn shows search_facts, get_episode_summaries,
    get_recent_messages, and TWO malformed get_contact_profile({}"") calls —
    none of which a general-knowledge question with no chat-memory dependency
    should ever trigger."""

    def test_no_orchestrator_round_trip_and_no_memory_tool_calls(self) -> None:
        def orchestrator_must_not_run(messages, info: AgentInfo) -> ModelResponse:
            raise AssertionError(
                "orchestrator must stay gated off for a general-knowledge turn "
                "with no memory/schedule/interaction signal"
            )

        request = general_knowledge_request()
        runtime = make_test_runtime(
            two_stage_draft=True,
            orchestrator_model=FunctionModel(orchestrator_must_not_run),
            draft_model=prompted({
                "reply": "Земля состоит из ядра, мантии и коры.",
                "intent": "general_message", "options": [], "confidence": 0.9, "topic": "планета",
            }),
        )

        outcome = runtime.draft_outcome(request, RiskLevel.LOW)

        assert "orchestrate(" not in outcome.model_used
        assert outcome.text == "Земля состоит из ядра, мантии и коры."

    def test_preflight_decision_snapshot(self) -> None:
        request = general_knowledge_request()
        decision = decide_preflight(
            request,
            orchestrator_slug=model_name_for(AgentTask.ORCHESTRATE),
            writer_slug=model_name_for(AgentTask.DRAFT),
        )
        assert decision.needs_orchestrator is False
        assert "memory" not in decision.tool_groups
        assert decision.allowed_tool_names == frozenset()


class TestPromptSnapshotsAcrossFamilies:
    """gpt/reasoning/agentic all render the same <prompt_contract> XML
    envelope (2.2/2.3); this snapshot ties that structural guarantee to the
    actual physics-teacher draft spec rather than a synthetic one."""

    @staticmethod
    def _physics_teacher_draft_spec():
        request = physics_teacher_request()
        return prompt_library.draft_spec(
            request,
            clean_memory=[],
            owner_style_facts=[],
            current_draft=None,
            owner_instruction=None,
            feedback_hint="",
            two_stage=False,
        )

    @pytest.mark.parametrize("family", [ModelFamily.GPT, ModelFamily.REASONING, ModelFamily.AGENTIC])
    def test_every_family_renders_the_required_xml_sections(self, family: ModelFamily) -> None:
        spec = self._physics_teacher_draft_spec()
        rendered = render_prompt(spec, family)

        assert rendered.startswith("<prompt_contract>")
        assert rendered.endswith("</prompt_contract>")
        for tag in (
            "role", "task", "trusted_context", "constraints", "tool_policy",
            "output_format", "success_criteria",
        ):
            assert f"<{tag}>" in rendered, f"missing <{tag}> in {family.value} render"

    def test_reasoning_family_omits_examples_and_chain_of_thought_bloat(self) -> None:
        spec = self._physics_teacher_draft_spec()
        rendered = render_prompt(spec, ModelFamily.REASONING)
        assert "<examples>" not in rendered
        assert "думай пошагово" not in rendered.lower()
        assert "step by step" not in rendered.lower()

    def test_gpt_and_agentic_include_examples_when_the_spec_carries_any(self) -> None:
        spec = self._physics_teacher_draft_spec()
        spec_with_example = dataclasses.replace(spec, examples=("owner-approved-example",))
        for family in (ModelFamily.GPT, ModelFamily.AGENTIC):
            rendered = render_prompt(spec_with_example, family)
            assert "<examples>" in rendered
            assert "owner-approved-example" in rendered


class TestOutputFormatSnapshots:
    def test_assigned_role_requires_telegram_rich_markdown(self) -> None:
        spec = prompt_library.draft_spec(
            physics_teacher_request(),
            clean_memory=[], owner_style_facts=[], current_draft=None,
            owner_instruction=None, feedback_hint="", two_stage=False,
        )
        assert spec.output_format.text_format == "telegram_rich_markdown"
        rendered = render_prompt(spec, ModelFamily.GPT)
        assert "<text_format>telegram_rich_markdown</text_format>" in rendered

    def test_no_role_forbids_markdown(self) -> None:
        spec = prompt_library.draft_spec(
            general_knowledge_request(),
            clean_memory=[], owner_style_facts=[], current_draft=None,
            owner_instruction=None, feedback_hint="", two_stage=False,
        )
        assert spec.output_format.text_format == "plain_text"
        rendered = render_prompt(spec, ModelFamily.GPT)
        assert "<text_format>plain_text</text_format>" in rendered
        assert "Telegram Rich Markdown" not in rendered


class TestToolCallBudgets:
    def test_orchestrator_budget_is_tighter_than_the_writers(self) -> None:
        assert _ORCHESTRATOR_TOOL_CALLS_LIMIT < _DRAFT_TOOL_CALLS_LIMIT

    def test_physics_teacher_scenario_orchestrator_stays_within_budget(self) -> None:
        """Even an orchestrator that keeps finding a (legitimate) reason to
        call tools must be cut off at the budget, never silently looping past
        it — directly exercises the limit against this scenario's tools."""
        call_count = {"n": 0}

        def always_calls_tool(messages, info: AgentInfo) -> ModelResponse:
            call_count["n"] += 1
            return ModelResponse(
                parts=[ToolCallPart(tool_name="get_recent_messages", args={"limit": 1})]
            )

        request = physics_teacher_request_with_recall_signal()
        runtime = make_test_runtime(
            two_stage_draft=True,
            orchestrator_model=FunctionModel(always_calls_tool),
            draft_max_attempts=1,
        )
        with pytest.raises(RuntimeError):
            runtime.draft_outcome(request, RiskLevel.LOW)
        assert call_count["n"] <= _ORCHESTRATOR_TOOL_CALLS_LIMIT + 1


class TestEmptyMetadataSpansAreNotEmptyLLMCalls:
    """Operationalises the .claude/analyze.md clarification: auto_reply
    (trace) and output_safety (span) events legitimately carry no input/
    output — they are metadata, not LLM calls. The real prompt/completion
    always lives on a 'generation' event, batched as 'generation-create'
    named 'app.llm.complete' (see langfuse_sink._to_batch_item)."""

    def test_generation_event_carries_app_llm_complete_name_and_real_payload(self) -> None:
        event = _SinkEvent(kind="generation", payload={
            "trace_id": "t1", "model": "writer-slug",
            "prompt": "[system]\n...\n[user]\n...", "completion": "Земля состоит из ядра.",
            "usage": {"input_tokens": 42, "output_tokens": 7},
            "metadata": {"task": "draft"},
        })
        item = _to_batch_item(event)
        assert item["type"] == "generation-create"
        assert item["body"]["name"] == "app.llm.complete"
        assert item["body"]["input"]
        assert item["body"]["output"]

    def test_auto_reply_trace_event_carries_no_input_output_fields(self) -> None:
        event = _SinkEvent(kind="trace", payload={
            "trace_id": "t1", "name": "auto_reply", "decision": "auto_reply", "intent": "question",
        })
        item = _to_batch_item(event)
        assert item["type"] == "trace-create"
        assert "input" not in item["body"]
        assert "output" not in item["body"]

    def test_output_safety_span_event_carries_no_input_output_fields(self) -> None:
        event = _SinkEvent(kind="span", payload={
            "trace_id": "t1", "name": "output_safety", "flagged": False,
        })
        item = _to_batch_item(event)
        assert item["type"] == "span-create"
        assert "input" not in item["body"]
        assert "output" not in item["body"]

    def test_runtime_records_one_real_generation_per_stage_not_per_metadata_span(self) -> None:
        """End-to-end: draft_outcome must produce exactly one 'draft'
        generation for a single-stage turn — the auto_reply/output_safety
        metadata is recorded separately and must never inflate this count."""
        sink = _FakeSink()
        with with_context(LLMCallContext(trace_id="trace-2.8")):
            runtime = make_test_runtime(sink=sink)
            runtime.draft_outcome(general_knowledge_request(), RiskLevel.LOW)
            runtime.record_decision("auto_reply", "general_message", 0.9)

        draft_generations = [g for g in sink.generations if g["metadata"].get("task") == "draft"]
        assert len(draft_generations) == 1
        assert draft_generations[0]["completion"]


class _FakeSink:
    """Capturing stand-in for LangfuseSink — records generation events."""

    def __init__(self) -> None:
        self.generations: list[dict] = []
        self.traces: list[dict] = []

    def record_trace(self, trace_id, payload):
        self.traces.append({"trace_id": trace_id, **payload})

    def record_span(self, trace_id, name, payload):
        pass

    def record_generation(
        self, trace_id, model, prompt, completion, privacy_flags=(), usage=None, cost_usd=0.0,
        metadata=None,
    ):
        self.generations.append(
            {"trace_id": trace_id, "model": model, "completion": completion, "metadata": metadata or {}}
        )
