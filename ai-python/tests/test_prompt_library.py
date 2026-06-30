"""Behavioural tests for the :mod:`ai_agent.prompts.library` PromptSpec builders.

Mirrors the sentinel-based style of ``test_draft.py``: opaque markers prove a
value was passed THROUGH from the request into the right ``PromptSpec``
field, never hard-coded. These builders are exercised directly (not through
the renderers) so a test failure points at the mapping, not the rendering.
"""

from __future__ import annotations

from ai_agent.agents.tool_catalog import TOOL_CATALOG
from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.library import (
    critic_spec,
    draft_spec,
    extract_spec,
    orchestrator_spec,
    pattern_spec,
    rerank_spec,
    role_draft_spec,
    summary_spec,
    topic_spec,
)
from ai_agent.prompts.renderers import render
from ai_agent.prompts.spec import OutputFormat
from ai_agent.schemas import (
    AIProcessMessageRequest,
    AIRoleData,
    ChatMode,
    ChatSettings,
    ContactProfileData,
    NormalizedTelegramEvent,
    PrivacyFlag,
    UserSettingsData,
)

OWNER_INSTRUCTION = "owner-instruction-sentinel"
CURRENT_DRAFT = "current-draft-sentinel"
FEEDBACK_HINT = "feedback-hint-sentinel"


def _request(
    *,
    ai_role: AIRoleData | None = None,
    user_settings: UserSettingsData | None = None,
    contact_profile: ContactProfileData | None = None,
    privacy_flags: tuple[PrivacyFlag, ...] = (),
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
            message_text="Когда созвонимся?",
            message_type="text",
            created_at="2026-06-11T10:00:00Z",
        ),
        chat_settings=ChatSettings(
            tenant_id="tenant:user:77", chat_id="42", mode=ChatMode.MANUAL, privacy_flags=privacy_flags
        ),
        ai_role=ai_role,
        user_settings=user_settings,
        contact_profile=contact_profile,
    )


def _draft_spec(request: AIProcessMessageRequest, **overrides) -> object:
    kwargs = dict(
        clean_memory=["[CURRENT DATE: 2026-05-30]"],
        owner_style_facts=[],
        current_draft=None,
        owner_instruction=None,
        feedback_hint="",
        two_stage=False,
    )
    kwargs.update(overrides)
    return draft_spec(request, **kwargs)


class TestDraftSpecPersonaFraming:
    def test_no_role_uses_owner_assistant_frame(self) -> None:
        spec = _draft_spec(_request())
        assert "ОТ ЛИЦА ВЛАДЕЛЬЦА" in spec.role
        assert spec.output_format == OutputFormat(
            text_format="plain_text", rules="Обычный текст без разметки."
        )

    def test_speak_as_owner_role_keeps_owner_frame(self) -> None:
        role = AIRoleData(name="role-name-sentinel", description="role-desc-sentinel")
        spec = _draft_spec(_request(ai_role=role))
        assert "ОТ ЛИЦА ВЛАДЕЛЬЦА" in spec.role
        assert any("role-name-sentinel" in line for line in spec.context)
        assert spec.output_format.text_format == "telegram_rich_markdown"
        assert "GFM" in spec.output_format.rules

    def test_bot_persona_role_uses_bot_frame_and_disclosure_constraint(self) -> None:
        role = AIRoleData(name="bot-role-sentinel", description="d", speak_as_owner=False)
        spec = _draft_spec(_request(ai_role=role))
        assert "чат-бот" in spec.role
        assert "ОТ ЛИЦА ВЛАДЕЛЬЦА" not in spec.role
        assert any("не копируй личность" in c.lower() for c in spec.constraints)

    def test_owner_context_routes_to_context_not_role(self) -> None:
        settings = UserSettingsData(display_name="owner-name-sentinel")
        speak_as_owner_spec = _draft_spec(_request(ai_role=None, user_settings=settings))
        assert any("owner-name-sentinel" in line for line in speak_as_owner_spec.context)
        assert "owner-name-sentinel" not in speak_as_owner_spec.role

        bot_role = AIRoleData(name="bot-role-sentinel", speak_as_owner=False)
        bot_spec = _draft_spec(_request(ai_role=bot_role, user_settings=settings))
        assert any(
            "owner-name-sentinel" in line and "справочный контекст" in line for line in bot_spec.context
        )


class TestDraftSpecMemoryAndFeedback:
    def test_owner_instruction_becomes_a_high_priority_constraint(self) -> None:
        spec = _draft_spec(_request(), owner_instruction=OWNER_INSTRUCTION)
        assert any(OWNER_INSTRUCTION in c and "высший приоритет" in c for c in spec.constraints)

    def test_current_draft_becomes_context(self) -> None:
        spec = _draft_spec(_request(), current_draft=CURRENT_DRAFT)
        assert any(CURRENT_DRAFT in line for line in spec.context)

    def test_feedback_hint_becomes_example_only_when_speaking_as_owner(self) -> None:
        owner_spec = _draft_spec(_request(), feedback_hint=FEEDBACK_HINT)
        assert spec_has(owner_spec.examples, FEEDBACK_HINT)

        bot_role = AIRoleData(name="bot-role-sentinel", speak_as_owner=False)
        bot_spec = _draft_spec(_request(ai_role=bot_role), feedback_hint=FEEDBACK_HINT)
        assert not spec_has(bot_spec.examples, FEEDBACK_HINT)


def spec_has(items: tuple[str, ...], sentinel: str) -> bool:
    return any(sentinel in item for item in items)


class TestDraftSpecToolGating:
    def test_interaction_instruction_present_only_when_tool_enabled(self) -> None:
        role_off = AIRoleData(name="r", tool_settings={"interaction.ask_questions": False})
        role_on = AIRoleData(name="r", tool_settings={"interaction.ask_questions": True})
        off_spec = _draft_spec(_request(ai_role=role_off))
        on_spec = _draft_spec(_request(ai_role=role_on))
        assert not spec_has(off_spec.constraints, "ask_questions(questions[1..6])")
        assert spec_has(on_spec.constraints, "ask_questions(questions[1..6])")
        # tool_policy is never fully empty — default-enabled memory tools and
        # the always-on send_message get applicability lines either way — but
        # ask_questions's own line (send_message's policy mentions it by name
        # too, hence the colon to match only the entry header) only appears
        # once the role enables it.
        assert "ask_questions:" not in off_spec.tool_policy
        assert "ask_questions:" in on_spec.tool_policy

    def test_scheduler_instruction_gated_by_privacy_flags(self) -> None:
        role = AIRoleData(name="r", tool_settings={"scheduler.create_reminder": True})
        # Known timezone so this test exercises privacy-flag gating only, not
        # the A1 "unknown timezone" marker (covered separately in test_draft.py).
        settings = UserSettingsData(timezone="Europe/Moscow")
        open_spec = _draft_spec(_request(ai_role=role, user_settings=settings))
        assert spec_has(open_spec.context, "ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА")

        private_spec = _draft_spec(
            _request(ai_role=role, user_settings=settings, privacy_flags=(PrivacyFlag.PRIVATE,))
        )
        assert not spec_has(private_spec.context, "ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА")

        no_memory_spec = _draft_spec(
            _request(ai_role=role, user_settings=settings, privacy_flags=(PrivacyFlag.NO_MEMORY,))
        )
        assert not spec_has(no_memory_spec.context, "ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА")

    def test_two_stage_directive_only_when_two_stage(self) -> None:
        single = _draft_spec(_request(), two_stage=False)
        bimodal = _draft_spec(_request(), two_stage=True)
        assert not spec_has(single.constraints, "ЗАПАСНОЙ путь")
        assert spec_has(bimodal.constraints, "ЗАПАСНОЙ путь")
        assert bimodal.tool_policy != ""


class TestOrchestratorSpec:
    def test_no_role_notes_role_is_unassigned(self) -> None:
        spec = orchestrator_spec(_request(ai_role=None))
        assert spec_has(spec.context, "Роль не назначена.")

    def test_role_fields_reach_context(self) -> None:
        role = AIRoleData(
            name="role-name-sentinel",
            description="role-desc-sentinel",
            goals="role-goals-sentinel",
            constraints="role-constraints-sentinel",
        )
        spec = orchestrator_spec(_request(ai_role=role))
        joined = "\n".join(spec.context)
        for sentinel in ("role-name-sentinel", "role-desc-sentinel", "role-goals-sentinel",
                         "role-constraints-sentinel"):
            assert sentinel in joined

    def test_interaction_instruction_gated_by_tool_setting(self) -> None:
        role_on = AIRoleData(name="r", tool_settings={"interaction.ask_questions": True})
        role_off = AIRoleData(name="r", tool_settings={"interaction.ask_questions": False})
        on_spec = orchestrator_spec(_request(ai_role=role_on))
        off_spec = orchestrator_spec(_request(ai_role=role_off))
        assert spec_has(on_spec.constraints, "ask_questions(questions[1..6])")
        assert not spec_has(off_spec.constraints, "ask_questions(questions[1..6])")

    def test_never_writes_a_contact_reply(self) -> None:
        spec = orchestrator_spec(_request())
        assert "финальный ответ ты НЕ пишешь" in spec.role

    def test_tool_policy_carries_applicability_not_just_short_labels(self) -> None:
        """2.4 acceptance: the orchestrator's tool_policy must say WHEN to use
        a tool, not just that it exists — TOOL_CATALOG's short label/description
        alone would not satisfy this."""
        spec = orchestrator_spec(_request())
        assert "Используй когда:" in spec.tool_policy
        assert "НЕ используй когда:" in spec.tool_policy
        # search_facts is default-enabled, so its applicability reaches the
        # prompt even with no role assigned.
        assert "search_facts:" in spec.tool_policy

    def test_tool_policy_only_describes_tools_actually_enabled_this_turn(self) -> None:
        role_scheduler_on = AIRoleData(
            name="r",
            tool_settings={**{e.key: False for e in TOOL_CATALOG}, "scheduler.create_reminder": True},
        )
        role_scheduler_off = AIRoleData(
            name="r", tool_settings={e.key: False for e in TOOL_CATALOG}
        )
        on_spec = orchestrator_spec(_request(ai_role=role_scheduler_on))
        off_spec = orchestrator_spec(_request(ai_role=role_scheduler_off))
        assert "create_reminder:" in on_spec.tool_policy
        assert "create_reminder:" not in off_spec.tool_policy
        # Every catalog tool disabled → only the generic anti-fabrication line remains.
        assert "search_facts:" not in off_spec.tool_policy


class TestCriticSpec:
    def test_speak_as_owner_persona_rule(self) -> None:
        spec = critic_spec(speak_as_owner=True)
        assert spec_has(spec.context, "от лица владельца")

    def test_bot_persona_rule(self) -> None:
        spec = critic_spec(speak_as_owner=False)
        assert spec_has(spec.context, "открытый чат-бот")
        assert not spec_has(spec.context, "от лица владельца")

    def test_feedback_hint_becomes_example(self) -> None:
        with_hint = critic_spec(speak_as_owner=True, feedback_hint=FEEDBACK_HINT)
        without_hint = critic_spec(speak_as_owner=True)
        assert spec_has(with_hint.examples, FEEDBACK_HINT)
        assert without_hint.examples == ()


# The six auxiliary builders take no arguments: the message/history/draft text
# each task operates on is untrusted and travels as the per-call user payload
# (see AgentRuntime._x_async), never through these builders or PromptSpec.context.
_AUXILIARY_BUILDERS = (extract_spec, summary_spec, topic_spec, rerank_spec, pattern_spec, role_draft_spec)


class TestAuxiliaryBuilderSpecsTakeNoUntrustedInput:
    def test_builders_accept_zero_arguments(self) -> None:
        """Nothing for a caller to pass through — there is no signature to misuse."""
        for builder in _AUXILIARY_BUILDERS:
            spec = builder()
            assert spec.role
            assert spec.task

    def test_builders_render_as_a_valid_prompt_contract_envelope_for_every_family(self) -> None:
        for builder in _AUXILIARY_BUILDERS:
            spec = builder()
            for family in (ModelFamily.GPT, ModelFamily.REASONING, ModelFamily.AGENTIC):
                text = render(spec, family)
                assert text.startswith("<prompt_contract>")
                assert text.endswith("</prompt_contract>")
                assert "<role>" in text and "<task>" in text


class TestExtractSpec:
    def test_extracts_only_durable_facts_not_transient(self) -> None:
        spec = extract_spec()
        assert "durable" in spec.task.lower()
        assert any("transient" in c.lower() for c in spec.constraints)

    def test_uses_empty_lists_rather_than_inventing_facts(self) -> None:
        spec = extract_spec()
        assert any("never invent" in c.lower() for c in spec.constraints)


class TestSummarySpec:
    def test_defines_summary_action_items_and_recommendations(self) -> None:
        spec = summary_spec()
        assert spec_has(spec.constraints, "summary")
        assert spec_has(spec.constraints, "action_items")
        assert spec_has(spec.constraints, "recommendations")


class TestTopicSpec:
    def test_labels_topic_as_short_noun_phrase_in_conversation_language(self) -> None:
        spec = topic_spec()
        assert "3 to 7 words" in spec.task
        assert any("conversation's language" in c for c in spec.constraints)


class TestRerankSpec:
    def test_returns_only_candidate_ids_never_invented(self) -> None:
        spec = rerank_spec()
        assert "ranked_ids" in spec.task
        assert any("never invent" in c.lower() for c in spec.constraints)


class TestPatternSpec:
    def test_empty_pattern_when_no_durable_signal(self) -> None:
        spec = pattern_spec()
        assert any("pattern=''" in c for c in spec.constraints)


class TestRoleDraftSpec:
    def test_does_not_invent_business_facts(self) -> None:
        spec = role_draft_spec()
        assert any("не выдумывай факты" in c.lower() for c in spec.constraints)
