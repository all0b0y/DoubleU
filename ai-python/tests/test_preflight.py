"""Behavioural tests for :mod:`ai_agent.agents.preflight` (Phase 2.7).

The first class covers the pure rule-based classifier directly against the
plan's acceptance criteria; the second exercises the actual runtime wiring
(orchestrator gating, tool restriction, ``model_settings``, Langfuse
metadata) through ``AgentRuntime.draft_outcome`` with FunctionModel doubles —
the same pattern ``test_agents.py`` uses.
"""

from __future__ import annotations

import json

from pydantic_ai.messages import ModelResponse, TextPart, ToolCallPart
from pydantic_ai.models.function import AgentInfo, FunctionModel
from pydantic_ai.profiles import ModelProfile

from ai_agent.agents.model_capabilities import ModelCapabilities
from ai_agent.agents.preflight import (
    TaskPreflightDecision,
    decide_preflight,
    model_settings_for,
)
from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.prompts.families import ModelFamily
from ai_agent.schemas import (
    AIProcessMessageRequest,
    AIRoleData,
    ChatMode,
    ChatSettings,
    MCPServerSpecData,
    NormalizedTelegramEvent,
    PrivacyFlag,
    RiskLevel,
)

from agent_helpers import make_test_runtime, prompted


def process_request(
    text: str, *, ai_role: AIRoleData | None = None, privacy_flags: tuple[PrivacyFlag, ...] = ()
) -> AIProcessMessageRequest:
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
            created_at="2026-06-30T10:00:00Z",
        ),
        chat_settings=ChatSettings(
            tenant_id="tenant:user:77", chat_id="42", mode=ChatMode.MANUAL, privacy_flags=privacy_flags
        ),
        ai_role=ai_role,
    )


class TestDecidePreflightAcceptanceCriteria:
    """Mirrors the plan's six acceptance-criteria sentences directly."""

    def test_identity_smalltalk_needs_no_reasoning_and_no_tools(self) -> None:
        request = process_request("Кто ты и что ты умеешь?")
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert decision.needs_orchestrator is False
        assert decision.tool_groups - {"delivery"} == frozenset()
        assert decision.allowed_tool_names == frozenset()
        assert decision.needs_reasoning is False
        assert decision.reasoning_effort in ("none", "minimal")

    def test_reminder_request_gets_schedule_tools_only(self, monkeypatch) -> None:
        # Pin the writer family to "reasoning" — the only family whose default
        # capability profile supports_reasoning — so the desired effort
        # ("low"/"minimal") is observable rather than capability-clamped to
        # "none" (see test_writer_without_reasoning_support_forces_effort_none).
        monkeypatch.setenv("MODEL_FAMILY_OVERRIDES", "writer=reasoning")
        role = AIRoleData(
            name="Assistant",
            tool_settings={"scheduler.create_reminder": True, "scheduler.list_reminders": True},
        )
        request = process_request("Поставь напоминание завтра", ai_role=role)
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert decision.needs_orchestrator is True
        assert decision.tool_groups - {"delivery"} == frozenset({"schedule"})
        assert decision.allowed_tool_names == frozenset({"create_reminder", "list_reminders"})
        assert decision.reasoning_effort in ("minimal", "low")

    def test_memory_recall_plus_planning_gets_memory_tools_and_medium_reasoning(self, monkeypatch) -> None:
        monkeypatch.setenv("MODEL_FAMILY_OVERRIDES", "writer=reasoning")
        request = process_request("Найди в памяти, что я говорил про X, и составь план")
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert decision.needs_orchestrator is True
        assert "memory" in decision.tool_groups
        assert "schedule" not in decision.tool_groups
        assert decision.needs_reasoning is True
        assert decision.reasoning_effort == "medium"

    def test_general_knowledge_question_gets_no_memory_tools(self) -> None:
        """The exact .claude/analyze.md regression case: a general-knowledge
        question must not trigger search_facts/get_contact_profile/etc."""
        request = process_request("Расскажи мне как устроена наша планета")
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert "memory" not in decision.tool_groups
        assert decision.needs_orchestrator is False
        assert decision.allowed_tool_names == frozenset()

    def test_blocked_json_tool_reliability_disables_tool_heavy_orchestrator(self, monkeypatch) -> None:
        monkeypatch.setenv(
            "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES", "bad-router=blocked"
        )
        request = process_request("Вспомни, что я говорил про отпуск")
        decision = decide_preflight(request, orchestrator_slug="bad-router", writer_slug="writer")
        assert decision.needs_orchestrator is False
        assert decision.allowed_tool_names == frozenset()
        assert "json_tool_reliability=blocked" in decision.reason

    def test_writer_without_reasoning_support_forces_effort_none(self, monkeypatch) -> None:
        monkeypatch.setenv("MODEL_FAMILY_DEFAULT", "gpt")  # GPT family: supports_reasoning=False
        request = process_request("Найди в памяти, что я говорил про X, и составь план")
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert decision.needs_reasoning is False
        assert decision.reasoning_effort == "none"


class TestDecidePreflightToolMinimisation:
    def test_no_signal_no_role_skips_orchestrator_entirely(self) -> None:
        """If trusted context already answers, needs_orchestrator=False, tools empty."""
        request = process_request("Спасибо, договорились!")
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert decision.needs_orchestrator is False
        assert decision.tool_groups - {"delivery"} == frozenset()

    def test_privacy_flag_blocks_schedule_group_even_with_keyword(self) -> None:
        role = AIRoleData(name="Assistant", tool_settings={"scheduler.create_reminder": True})
        request = process_request(
            "Поставь напоминание завтра", ai_role=role, privacy_flags=(PrivacyFlag.NO_MEMORY,)
        )
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert "schedule" not in decision.tool_groups
        assert decision.needs_orchestrator is False

    def test_role_enabled_memory_tools_outside_signal_stay_unselected(self) -> None:
        """A role enabling 6 memory tools must not expose all 6 just because one
        keyword matched — only the tools whose group preflight selected."""
        role = AIRoleData(
            name="Assistant",
            tool_settings={
                "memory.search_facts": True,
                "memory.get_episode_summaries": True,
                "memory.get_recent_messages": True,
                "memory.get_contact_profile": True,
                "memory.save_fact": True,
                "memory.suggest_profile_update": True,
                "scheduler.create_reminder": True,
            },
        )
        request = process_request("Вспомни, что я говорил про отпуск", ai_role=role)
        decision = decide_preflight(request, orchestrator_slug="router", writer_slug="writer")
        assert "schedule" not in decision.tool_groups
        assert "create_reminder" not in decision.allowed_tool_names
        assert "search_facts" in decision.allowed_tool_names

    def test_mcp_only_rides_along_with_an_already_triggered_turn(self) -> None:
        role = AIRoleData(
            name="Assistant",
            mcp_servers=(MCPServerSpecData(name="x", url="https://x", transport="http"),),
        )
        identity_request = process_request("Кто ты и что ты умеешь?", ai_role=role)
        decision = decide_preflight(identity_request, orchestrator_slug="router", writer_slug="writer")
        assert "mcp" not in decision.tool_groups
        assert decision.needs_orchestrator is False


class TestModelSettingsFor:
    def _decision(self, **overrides) -> TaskPreflightDecision:
        defaults = dict(
            needs_reasoning=True,
            reasoning_effort="medium",
            needs_orchestrator=True,
            tool_groups=frozenset({"memory", "delivery"}),
            allowed_tool_names=frozenset({"search_facts", "send_message"}),
            structured_output_required=True,
            selected_model_slug="writer",
            prompt_shape_family=ModelFamily.GPT,
            reason="test",
        )
        defaults.update(overrides)
        return TaskPreflightDecision(**defaults)

    def test_reasoning_effort_translates_to_unified_thinking_setting(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.REASONING,
            supports_tools=True,
            supports_tool_choice=False,
            supports_parallel_tool_calls=False,
            supports_structured_outputs=False,
            supports_reasoning=True,
            reasoning_mandatory=False,
            reasoning_default_enabled=True,
            supported_reasoning_efforts=("low", "medium", "high"),
        )
        settings = model_settings_for(self._decision(), caps)
        assert settings["thinking"] == "medium"

    def test_max_effort_clamps_to_high_thinking_value(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.REASONING,
            supports_tools=True,
            supports_tool_choice=False,
            supports_parallel_tool_calls=False,
            supports_structured_outputs=False,
            supports_reasoning=True,
            reasoning_mandatory=False,
            reasoning_default_enabled=True,
            supported_reasoning_efforts=("low", "medium", "high"),
        )
        settings = model_settings_for(self._decision(reasoning_effort="max"), caps)
        assert settings["thinking"] == "high"

    def test_no_reasoning_support_never_sends_thinking_key(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.GPT,
            supports_tools=True,
            supports_tool_choice=True,
            supports_parallel_tool_calls=True,
            supports_structured_outputs=True,
            supports_reasoning=False,
            reasoning_mandatory=False,
            reasoning_default_enabled=False,
        )
        settings = model_settings_for(self._decision(needs_reasoning=False, reasoning_effort="none"), caps)
        assert "thinking" not in settings

    def test_effort_none_disables_reasoning_when_not_mandatory(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.REASONING,
            supports_tools=True,
            supports_tool_choice=False,
            supports_parallel_tool_calls=False,
            supports_structured_outputs=False,
            supports_reasoning=True,
            reasoning_mandatory=False,
            reasoning_default_enabled=True,
        )
        settings = model_settings_for(
            self._decision(needs_reasoning=False, reasoning_effort="none"), caps
        )
        assert settings["thinking"] is False

    def test_effort_none_leaves_mandatory_reasoning_model_untouched(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.REASONING,
            supports_tools=True,
            supports_tool_choice=False,
            supports_parallel_tool_calls=False,
            supports_structured_outputs=False,
            supports_reasoning=True,
            reasoning_mandatory=True,
            reasoning_default_enabled=True,
        )
        settings = model_settings_for(
            self._decision(needs_reasoning=False, reasoning_effort="none"), caps
        )
        assert "thinking" not in settings

    def test_parallel_tool_calls_only_set_when_multiple_groups_active(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.GPT,
            supports_tools=True,
            supports_tool_choice=True,
            supports_parallel_tool_calls=True,
            supports_structured_outputs=True,
            supports_reasoning=False,
            reasoning_mandatory=False,
            reasoning_default_enabled=False,
        )
        single_group = model_settings_for(
            self._decision(tool_groups=frozenset({"memory", "delivery"})), caps
        )
        multi_group = model_settings_for(
            self._decision(tool_groups=frozenset({"memory", "schedule", "delivery"})), caps
        )
        assert single_group["parallel_tool_calls"] is False
        assert multi_group["parallel_tool_calls"] is True

    def test_unsupported_parallel_tool_calls_omits_key(self) -> None:
        caps = ModelCapabilities(
            prompt_shape_family=ModelFamily.GPT,
            supports_tools=True,
            supports_tool_choice=True,
            supports_parallel_tool_calls=False,
            supports_structured_outputs=True,
            supports_reasoning=False,
            reasoning_mandatory=False,
            reasoning_default_enabled=False,
        )
        settings = model_settings_for(self._decision(), caps)
        assert "parallel_tool_calls" not in settings


def draft_output(args: dict) -> ModelResponse:
    return ModelResponse(parts=[TextPart(json.dumps(args))])


class TestRuntimeWiring:
    """Verifies the decision actually constrains AgentRuntime, not just logs."""

    def test_general_knowledge_question_skips_orchestrator_round_trip(self) -> None:
        def orchestrator_must_not_run(messages, info: AgentInfo) -> ModelResponse:
            raise AssertionError("orchestrator must stay gated off for a no-signal turn")

        role = AIRoleData(name="Tutor", tool_settings={"memory.search_facts": True})
        request = process_request("Расскажи мне как устроена наша планета", ai_role=role)
        runtime = make_test_runtime(
            two_stage_draft=True,
            orchestrator_model=FunctionModel(orchestrator_must_not_run),
            draft_model=prompted(
                {"reply": "Земля состоит из ядра, мантии и коры.",
                 "intent": "general_message", "options": [], "confidence": 0.9, "topic": "планета"}
            ),
        )
        outcome = runtime.draft_outcome(request, RiskLevel.LOW)
        assert "orchestrate(" not in outcome.model_used

    def test_orchestrator_tools_are_restricted_to_the_signalled_group(self) -> None:
        captured: dict[str, object] = {}

        def orchestrator(messages, info: AgentInfo) -> ModelResponse:
            captured["tools"] = sorted(tool.name for tool in info.function_tools)
            return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args={
                "gathered_context": ["напоминание учтено"],
                "suggested_intent": "scheduling",
                "notes": "",
            })])

        role = AIRoleData(
            name="Assistant",
            tool_settings={"memory.search_facts": True, "scheduler.create_reminder": True},
        )
        request = process_request("Поставь напоминание завтра", ai_role=role)
        runtime = make_test_runtime(
            two_stage_draft=True,
            orchestrator_model=FunctionModel(orchestrator),
            draft_model=prompted(
                {"reply": "Готово.", "intent": "scheduling", "options": [], "confidence": 0.9, "topic": ""}
            ),
        )
        runtime.draft_outcome(request, RiskLevel.LOW)
        assert captured["tools"] == ["create_reminder"]

    def test_reasoning_effort_reaches_writer_model_settings(self, monkeypatch) -> None:
        # Pin the writer's resolved family to "reasoning" — the only family
        # whose default capability profile supports_reasoning (see
        # model_capabilities._FAMILY_DEFAULTS) — so model_settings_for has
        # something to translate the decision's effort into.
        monkeypatch.setenv("MODEL_DRAFT", "writer-reasoning-slug")
        monkeypatch.setenv("MODEL_FAMILY_OVERRIDES", "writer-reasoning-slug=reasoning")
        captured: dict[str, object] = {}

        def writer(messages, info: AgentInfo) -> ModelResponse:
            # pydantic-ai resolves the unified `model_settings["thinking"]`
            # into `model_request_parameters.thinking` inside
            # `Model.prepare_request` — only when the model's profile
            # declares `supports_thinking` (see ``draft_model`` below) —
            # then strips the raw key out of `model_settings`. That
            # resolved field, not the now-stripped `model_settings` dict,
            # is what a real provider call (OpenAIChatModel/OpenRouter)
            # actually reads — see `_translate_thinking` in
            # `pydantic_ai.models.openai`.
            captured["thinking"] = info.model_request_parameters.thinking
            return draft_output(
                {"reply": "План готов.", "intent": "general_message",
                 "options": [], "confidence": 0.9, "topic": ""}
            )

        request = process_request("Найди в памяти, что я говорил про X, и составь план")
        runtime = make_test_runtime(
            two_stage_draft=False,
            draft_model=FunctionModel(writer, profile=ModelProfile(supports_thinking=True)),
        )
        runtime.draft_outcome(request, RiskLevel.LOW)
        assert captured["thinking"] == "medium"

    def test_langfuse_trace_carries_preflight_decision_and_request_params(self) -> None:
        sink = _FakeSink()
        request = process_request("Поставь напоминание завтра")
        runtime = make_test_runtime(sink=sink)
        with with_context(LLMCallContext(trace_id="trace-preflight")):
            runtime.draft_outcome(request, RiskLevel.LOW)
        draft_generation = next(g for g in sink.generations if g["metadata"].get("task") == "draft")
        meta = draft_generation["metadata"]
        assert meta["preflight_needs_orchestrator"] is False
        assert "preflight_reasoning_effort" in meta
        assert "preflight_reason" in meta
        assert "tool_call_count" in meta


class _FakeSink:
    """Capturing stand-in for LangfuseSink — records generation events."""

    def __init__(self) -> None:
        self.generations: list[dict] = []
        self.traces: list[str] = []

    def record_trace(self, trace_id, payload):
        self.traces.append(trace_id)

    def record_span(self, trace_id, name, payload):
        pass

    def record_generation(
        self, trace_id, model, prompt, completion, privacy_flags=(), usage=None, cost_usd=0.0,
        metadata=None,
    ):
        self.generations.append(
            {"trace_id": trace_id, "model": model, "usage": usage or {}, "metadata": metadata or {}}
        )
