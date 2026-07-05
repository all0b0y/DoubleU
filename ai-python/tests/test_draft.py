"""Behavioural tests for draft system-prompt assembly and profile parsing.

Everything in the draft prompt is personalised — driven entirely by the owner's
per-chat settings.  These tests therefore use opaque sentinel strings rather than
plausible names/styles, so each assertion proves the value is passed THROUGH from
the profile into the right section (and nothing is hard-coded): ``me_style`` lands
in the OWNER section, structured contact fields land in the CONTACT section, and
the legacy ``notes`` JSON is never echoed raw.
"""

from __future__ import annotations

import unittest
from datetime import datetime, timezone

import json

from pydantic_ai.messages import ModelResponse, TextPart, ToolCallPart
from pydantic_ai.models.function import AgentInfo, FunctionModel

from ai_agent.agents.tool_catalog import TOOL_CATALOG
from ai_agent.pipelines import draft as draft_pipeline
from ai_agent.prompts import library as prompt_library
from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.renderers import render as render_prompt
from ai_agent.schemas import (
    AIProcessMessageRequest,
    AIRoleData,
    ChatMode,
    ChatSettings,
    ContactProfileData,
    NormalizedTelegramEvent,
    RiskLevel,
    UserSettingsData,
)
from ai_agent.schemas import _parse_ai_role, _parse_contact_profile

from agent_helpers import make_test_runtime, structured

# Opaque markers: if one appears in the prompt it can only have come from the
# matching profile field — never from a hard-coded default.
OWNER_STYLE = "owner-style-sentinel-α"
CONTACT_CONTEXT = "chat-context-sentinel-β"
TABOO_TOPIC = "taboo-sentinel-γ"


def _process_request(
    text: str = "Когда созвонимся?",
    ai_role: AIRoleData | None = None,
    user_settings: UserSettingsData | None = None,
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
            created_at="2026-06-11T10:00:00Z",
        ),
        chat_settings=ChatSettings(
            tenant_id="tenant:user:77",
            chat_id="42",
            mode=ChatMode.MANUAL,
            privacy_flags=(),
        ),
        ai_role=ai_role,
        user_settings=user_settings,
    )


def _draft_request(
    *,
    ai_role: AIRoleData | None = None,
    user_settings: UserSettingsData | None = None,
    contact_profile: ContactProfileData | None = None,
) -> AIProcessMessageRequest:
    """Like ``_process_request`` but also wires ``contact_profile`` for ``draft_spec``."""
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
            tenant_id="tenant:user:77", chat_id="42", mode=ChatMode.MANUAL, privacy_flags=()
        ),
        ai_role=ai_role,
        user_settings=user_settings,
        contact_profile=contact_profile,
    )


def _build_draft_spec(request: AIProcessMessageRequest, **overrides) -> prompt_library.PromptSpec:
    """Build the draft ``PromptSpec`` with sane test defaults — mirrors
    ``test_prompt_library.py``'s helper of the same shape, scoped to this
    file's content-placement assertions."""
    kwargs = dict(
        clean_memory=["[CURRENT DATE: 2026-05-30]"],
        owner_style_facts=[],
        current_draft=None,
        owner_instruction=None,
        feedback_hint="",
        two_stage=False,
    )
    kwargs.update(overrides)
    return prompt_library.draft_spec(request, **kwargs)


def _context_entry(spec: prompt_library.PromptSpec, prefix: str) -> str:
    """Return the single ``spec.context`` entry starting with ``prefix``."""
    for line in spec.context:
        if line.startswith(prefix):
            return line
    raise AssertionError(f"no context entry starts with {prefix!r}: {spec.context!r}")


def _spec_has(items: tuple[str, ...], sentinel: str) -> bool:
    return any(sentinel in item for item in items)


class ContactProfileParsingTests(unittest.TestCase):
    """``_parse_contact_profile`` must surface structured + legacy profile fields."""

    def test_structured_fields_are_parsed(self) -> None:
        """All structured style/context fields the Go backend sends are read."""
        profile = _parse_contact_profile({
            "style_notes": OWNER_STYLE,
            "tone": "casual",
            "emoji_usage": "rare",
            "address_form": "ty",
            "chat_context": CONTACT_CONTEXT,
            "do_not_discuss": [TABOO_TOPIC, "  ", 5],
        })
        assert profile is not None
        self.assertEqual(profile.owner_style, OWNER_STYLE)
        self.assertEqual(profile.tone, "casual")
        self.assertEqual(profile.address_form, "ty")
        self.assertEqual(profile.chat_context, CONTACT_CONTEXT)
        # Non-strings and blanks are dropped from do_not_discuss.
        self.assertEqual(profile.do_not_discuss, (TABOO_TOPIC,))

    def test_owner_style_falls_back_to_legacy_notes_json(self) -> None:
        """When structured fields are absent, me_style is recovered from notes JSON."""
        profile = _parse_contact_profile({
            "notes": f'{{"me_style":"{OWNER_STYLE}","chat_context":"{CONTACT_CONTEXT}"}}',
        })
        assert profile is not None
        self.assertEqual(profile.owner_style, OWNER_STYLE)
        self.assertEqual(profile.chat_context, CONTACT_CONTEXT)

    def test_structured_fields_win_over_legacy_notes(self) -> None:
        """A populated style_notes column takes precedence over the legacy JSON."""
        profile = _parse_contact_profile({
            "style_notes": OWNER_STYLE,
            "notes": '{"me_style":"stale-value"}',
        })
        assert profile is not None
        self.assertEqual(profile.owner_style, OWNER_STYLE)

    def test_malformed_notes_json_is_ignored(self) -> None:
        """A non-JSON notes string must not raise and must not leak into fields."""
        profile = _parse_contact_profile({"notes": "not json {"})
        assert profile is not None
        self.assertEqual(profile.owner_style, "")
        self.assertEqual(profile.chat_context, "")


class DraftPromptAssemblyTests(unittest.TestCase):
    """``library.draft_spec`` places profile data in the right context entries."""

    def test_me_style_is_in_owner_section_not_contact(self) -> None:
        """me_style describes how the OWNER writes — it belongs in the persona entry."""
        profile = ContactProfileData(relationship_type="friendship", owner_style=OWNER_STYLE)
        spec = _build_draft_spec(_draft_request(contact_profile=profile))
        persona_entry = _context_entry(spec, "КТО ТЫ В ЭТОМ ЧАТЕ:")
        contact_entry = _context_entry(spec, "СОБЕСЕДНИК — кому пишешь:")
        self.assertIn(OWNER_STYLE, persona_entry)
        self.assertNotIn(OWNER_STYLE, contact_entry)

    def test_raw_notes_json_never_appears(self) -> None:
        """The assembled spec must never carry the raw {"me_style"...} JSON."""
        profile = ContactProfileData(owner_style=OWNER_STYLE, notes='{"me_style":"x"}')
        spec = _build_draft_spec(_draft_request(contact_profile=profile))
        joined = "\n".join(spec.context)
        self.assertNotIn('{"me_style"', joined)
        self.assertNotIn("me_style", joined)

    def test_structured_contact_fields_render_cleanly(self) -> None:
        """Tone / address form / chat context / taboo appear in the contact entry."""
        profile = ContactProfileData(
            address_form="ty",
            tone="casual",
            chat_context=CONTACT_CONTEXT,
            do_not_discuss=(TABOO_TOPIC,),
        )
        spec = _build_draft_spec(_draft_request(contact_profile=profile))
        contact_entry = _context_entry(spec, "СОБЕСЕДНИК — кому пишешь:")
        self.assertIn("на «ты»", contact_entry)
        self.assertIn("casual", contact_entry)
        self.assertIn(CONTACT_CONTEXT, contact_entry)
        self.assertIn(TABOO_TOPIC, contact_entry)

    def test_facts_are_framed_as_optional_background(self) -> None:
        """The spec instructs the model to treat [ФАКТ ...] lines as soft context."""
        spec = _build_draft_spec(_draft_request())
        joined = "\n".join(spec.constraints)
        self.assertIn("[ФАКТ", joined)
        self.assertIn("справочный фон", joined)

    def test_contact_extras_render_in_contact_block(self) -> None:
        """Topics/language/timezone/preferred-address (sent by Go) reach the spec."""
        profile = ContactProfileData(
            topics=("topic-sentinel",),
            language="lang-sentinel",
            contact_timezone="tz-sentinel",
            preferred_address="addr-sentinel",
        )
        spec = _build_draft_spec(_draft_request(contact_profile=profile))
        contact_entry = _context_entry(spec, "СОБЕСЕДНИК — кому пишешь:")
        self.assertIn("topic-sentinel", contact_entry)
        self.assertIn("lang-sentinel", contact_entry)
        self.assertIn("tz-sentinel", contact_entry)
        self.assertIn("addr-sentinel", contact_entry)

    def test_owner_identity_renders_in_role_block(self) -> None:
        """user_settings identity lands among the OWNER/role entries, not contact."""
        spec = _build_draft_spec(
            _draft_request(
                user_settings=UserSettingsData(display_name="owner-name-sentinel", timezone="tz-x")
            )
        )
        joined_context = "\n".join(spec.context)
        contact_entry = _context_entry(spec, "СОБЕСЕДНИК — кому пишешь:")
        self.assertIn("owner-name-sentinel", joined_context)
        self.assertNotIn("owner-name-sentinel", contact_entry)


class AIRolePromptTests(unittest.TestCase):
    """The assigned AI role drives the persona entry of the draft ``PromptSpec``."""

    ROLE = AIRoleData(
        name="role-name-sentinel",
        description="role-description-sentinel",
        speaking_style="role-style-sentinel",
        goals="role-goals-sentinel",
        constraints="role-constraints-sentinel",
    )

    def test_role_fields_render_in_persona_block(self) -> None:
        """Name, description, style, goals and constraints all reach the persona entry."""
        spec = _build_draft_spec(_draft_request(ai_role=self.ROLE))
        persona_entry = _context_entry(spec, "КТО ТЫ В ЭТОМ ЧАТЕ:")
        contact_entry = _context_entry(spec, "СОБЕСЕДНИК — кому пишешь:")
        for sentinel in (
            "role-name-sentinel",
            "role-description-sentinel",
            "role-style-sentinel",
            "role-goals-sentinel",
            "role-constraints-sentinel",
        ):
            self.assertIn(sentinel, persona_entry)
            self.assertNotIn(sentinel, contact_entry)

    def test_role_style_wins_over_owner_style(self) -> None:
        """With a role speaking_style set, the owner's observed style is not injected."""
        profile = ContactProfileData(owner_style=OWNER_STYLE)
        spec = _build_draft_spec(_draft_request(ai_role=self.ROLE, contact_profile=profile))
        joined = "\n".join(spec.context)
        self.assertIn("role-style-sentinel", joined)
        self.assertNotIn(OWNER_STYLE, joined)

    def test_role_without_style_falls_back_to_owner_style(self) -> None:
        """A role with no speaking_style borrows the owner's per-chat style as a hint."""
        role = AIRoleData(name="role-name-sentinel", description="d")
        profile = ContactProfileData(owner_style=OWNER_STYLE)
        spec = _build_draft_spec(_draft_request(ai_role=role, contact_profile=profile))
        persona_entry = _context_entry(spec, "КТО ТЫ В ЭТОМ ЧАТЕ:")
        self.assertIn(OWNER_STYLE, persona_entry)

    def test_bot_role_uses_owner_bio_as_context_not_personality(self) -> None:
        """Bot mode may reason from owner facts without inheriting the owner's voice."""
        role = AIRoleData(
            name="bot-role-sentinel",
            description="bot-description-sentinel",
            speak_as_owner=False,
        )
        spec = _build_draft_spec(
            _draft_request(
                ai_role=role,
                user_settings=UserSettingsData(
                    display_name="owner-name-sentinel", bio="owner-bio-sentinel"
                ),
                contact_profile=ContactProfileData(owner_style=OWNER_STYLE),
            ),
            owner_style_facts=[OWNER_STYLE],
            feedback_hint="owner-feedback-sentinel",
        )
        text = render_prompt(spec, ModelFamily.GPT)

        self.assertIn("Твоя единственная персона", spec.role)
        self.assertIn("bot-role-sentinel", text)
        self.assertIn("не копируй личность", text.lower())
        self.assertIn("выше остальных инструкций", text)
        self.assertIn("ВЛАДЕЛЕЦ — справочный контекст, НЕ персона бота", text)
        self.assertIn("owner-name-sentinel", text)
        self.assertIn("owner-bio-sentinel", text)
        self.assertIn("точный совет, объяснение или вывод", text)
        self.assertNotIn(OWNER_STYLE, text)
        self.assertNotIn("owner-feedback-sentinel", text)

    def test_no_role_falls_back_to_owner_assistant_persona(self) -> None:
        """Without a role the spec keeps the legacy copy-the-owner behaviour."""
        profile = ContactProfileData(owner_style=OWNER_STYLE)
        spec = _build_draft_spec(_draft_request(contact_profile=profile))
        persona_entry = _context_entry(spec, "КТО ТЫ В ЭТОМ ЧАТЕ:")
        self.assertIn("Роль для чата не задана", persona_entry)
        self.assertIn(OWNER_STYLE, persona_entry)

    def test_prompt_pins_memory_to_this_chat(self) -> None:
        """The spec forbids referencing conversations outside this chat's context."""
        spec = _build_draft_spec(_draft_request(ai_role=self.ROLE))
        self.assertIn("только из этого чата", "\n".join(spec.constraints))

    def test_prompt_frames_markup_as_readability_tool(self) -> None:
        """The output-format rules name the Rich Markdown dialect and useful GFM blocks."""
        spec = _build_draft_spec(_draft_request(ai_role=self.ROLE))
        rules = spec.output_format.rules

        self.assertIn("Telegram Rich Markdown (GFM)", rules)
        self.assertIn("GFM-таблицы", rules)
        self.assertIn("Короткий ответ", rules)

    def test_two_stage_writer_directive_is_opt_in(self) -> None:
        """Only the bimodal writer gets the context-already-gathered directive.

        Hybrid: the writer keeps its tools as a fallback, so the directive frames
        them as a backup path rather than removing them.
        """
        single_stage = _build_draft_spec(_draft_request(ai_role=self.ROLE))
        two_stage = _build_draft_spec(_draft_request(ai_role=self.ROLE), two_stage=True)
        self.assertFalse(_spec_has(single_stage.constraints, "КОНТЕКСТ УЖЕ СОБРАН"))
        self.assertTrue(_spec_has(two_stage.constraints, "КОНТЕКСТ УЖЕ СОБРАН"))
        self.assertTrue(_spec_has(two_stage.constraints, "ЗАПАСНОЙ путь"))

    def test_ask_questions_instruction_present_in_both_stages(self) -> None:
        """Hybrid: the writer is told the ask_questions schema in both stages.

        The bimodal writer keeps the tool as a fallback, so it needs the schema
        even when an orchestrator bundle is present (``two_stage=True`` here
        models the writer's *own* spec once a bundle exists — the orchestrator
        bundle text itself is appended separately by ``AgentRuntime``).
        """
        role = AIRoleData(
            name="role-name-sentinel",
            tool_settings={"interaction.ask_questions": True},
        )
        request = _draft_request(ai_role=role)

        single_stage = _build_draft_spec(request)
        two_stage = _build_draft_spec(request, two_stage=True)
        single_joined = "\n".join(single_stage.constraints)
        two_joined = "\n".join(two_stage.constraints)

        self.assertIn("ask_questions(questions[1..6])", single_joined)
        self.assertIn("своими словами", single_joined)
        self.assertIn("ask_questions(questions[1..6])", two_joined)
        # The question text must not be echoed into the reply (anti-duplicate rule).
        self.assertIn("ТОЛЬКО в ask_questions", single_joined)
        self.assertIn("НИКОГДА не повторяй этот вопрос", two_joined)

    def test_single_stage_writer_ask_questions_reaches_draft_outcome(self) -> None:
        """A writer tool call in single-stage mode becomes the final response_plan."""
        calls = {"writer": 0}

        def writer(messages, info: AgentInfo) -> ModelResponse:
            calls["writer"] += 1
            if calls["writer"] == 1:
                return ModelResponse(parts=[ToolCallPart(
                    tool_name="ask_questions",
                    args={"questions": [
                        {"prompt": "Какой день удобнее?", "options": ["Сегодня", "Завтра"]},
                    ]},
                )])
            # Draft uses PromptedOutput → final result is plain-text JSON.
            return ModelResponse(parts=[TextPart(json.dumps({
                "reply": "",
                "intent": "general_message",
                "options": [],
                "confidence": 0.9,
                "topic": "выбор времени",
            }))])

        request = _process_request(
            text="Когда созвонимся?",
            ai_role=AIRoleData(
                name="Support",
                tool_settings={
                    **{entry.key: False for entry in TOOL_CATALOG},
                    "interaction.ask_questions": True,
                },
            ),
        )
        runtime = make_test_runtime(
            two_stage_draft=False,
            draft_model=FunctionModel(writer),
            critic_model=structured({
                "verdict": "approve",
                "issues": [],
                "revised_reply": "",
            }),
        )

        outcome = runtime.draft_outcome(request, RiskLevel.LOW)

        self.assertEqual(calls["writer"], 2)
        self.assertEqual(outcome.intent, "choice_question")
        self.assertIsNotNone(outcome.response_plan)
        assert outcome.response_plan is not None
        self.assertEqual(outcome.response_plan.shape, "questions")
        self.assertEqual(len(outcome.response_plan.questions), 1)
        self.assertEqual(outcome.response_plan.questions[0].options, ("Сегодня", "Завтра"))

    def test_no_role_prompt_forbids_markup(self) -> None:
        """The legacy owner-copy path remains plain to match its delivery contract."""
        spec = _build_draft_spec(_draft_request())
        self.assertEqual(spec.output_format.text_format, "plain_text")
        self.assertIn("без разметки", spec.output_format.rules)

    def test_decide_text_format_uses_rich_for_role_structured_intents(self) -> None:
        """Assigned roles may return Rich Markdown for structured answer intents.

        choice_question is included: quiz/choice replies carry markdown and must
        render rich rather than leak raw ** / # into the chat.
        """
        for intent in ("question", "scheduling", "commercial", "request",
                       "general_message", "choice_question"):
            self.assertEqual(draft_pipeline.decide_text_format(self.ROLE, intent), "rich")

    def test_decide_text_format_keeps_copy_and_short_replies_plain(self) -> None:
        """Legacy owner-copy replies and terse acknowledgements stay plain."""
        self.assertEqual(draft_pipeline.decide_text_format(None, "question"), "plain")
        self.assertEqual(draft_pipeline.decide_text_format(self.ROLE, "acknowledgement"), "plain")

    def test_parse_ai_role_requires_name(self) -> None:
        """A role payload without a name is treated as no role at all."""
        self.assertIsNone(_parse_ai_role({"description": "x"}))
        self.assertIsNone(_parse_ai_role(None))
        parsed = _parse_ai_role({
            "name": " n ",
            "constraints": " c ",
            "model_name": " model-b ",
        })
        assert parsed is not None
        self.assertEqual(parsed.name, "n")
        self.assertEqual(parsed.constraints, "c")
        self.assertEqual(parsed.model_name, "model-b")
        self.assertTrue(parsed.speak_as_owner)

        bot_role = _parse_ai_role({"name": "bot", "speak_as_owner": False})
        assert bot_role is not None
        self.assertFalse(bot_role.speak_as_owner)

    def test_custom_fields_render_in_persona_block(self) -> None:
        """Owner-defined custom fields reach the persona block, not the contact block."""
        from ai_agent.schemas import AIRoleCustomFieldData

        role = AIRoleData(
            name="role-name-sentinel",
            description="d",
            custom_fields=(
                AIRoleCustomFieldData(
                    title="cf-title-sentinel",
                    description="cf-desc-sentinel",
                    context="cf-context-sentinel",
                ),
            ),
        )
        spec = _build_draft_spec(_draft_request(ai_role=role))
        persona_entry = _context_entry(spec, "КТО ТЫ В ЭТОМ ЧАТЕ:")
        contact_entry = _context_entry(spec, "СОБЕСЕДНИК — кому пишешь:")
        for sentinel in ("cf-title-sentinel", "cf-desc-sentinel", "cf-context-sentinel"):
            self.assertIn(sentinel, persona_entry)
            self.assertNotIn(sentinel, contact_entry)

    def test_parse_ai_role_drops_custom_fields_without_title(self) -> None:
        """Custom fields without a title are dropped on parse."""
        parsed = _parse_ai_role(
            {
                "name": "n",
                "custom_fields": [
                    {"title": " keep ", "description": " why "},
                    {"title": "", "description": "no title"},
                    "not-a-dict",
                ],
            }
        )
        assert parsed is not None
        self.assertEqual(len(parsed.custom_fields), 1)
        self.assertEqual(parsed.custom_fields[0].title, "keep")
        self.assertEqual(parsed.custom_fields[0].description, "why")


class CurrentTimeInstructionTests(unittest.TestCase):
    """``_current_time_instruction`` grounds scheduler.create_reminder in the
    owner's local time: bare times in a message must resolve against the
    owner's IANA zone (never UTC), and an already-passed time must roll
    forward or prompt a clarifying question instead of firing in the past."""

    # Fixed instant (not datetime.now()) so the assertions never depend on
    # the date the suite happens to run on.
    _NOON_UTC = datetime(2026, 6, 23, 10, 0, tzinfo=timezone.utc)

    def test_local_time_reflects_owner_timezone(self) -> None:
        """A known UTC instant converts to the owner's zone, not UTC."""
        request = _process_request(user_settings=UserSettingsData(timezone="Europe/Kyiv"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        # Europe/Kyiv observes EEST (UTC+3) in June.
        self.assertIn("2026-06-23T13:00:00+03:00", instruction)
        self.assertIn("Europe/Kyiv", instruction)
        self.assertIn("2026-06-23T10:00:00+00:00", instruction)

    def test_invalid_timezone_is_treated_as_unknown(self) -> None:
        """An unrecognised IANA name must NOT silently degrade to UTC (A1):
        a stored value that fails to resolve cannot be trusted to mean what
        the owner thinks it means, so it is flagged unknown like an empty one."""
        request = _process_request(user_settings=UserSettingsData(timezone="Not/AZone"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("ПОЯС ВЛАДЕЛЬЦА НЕИЗВЕСТЕН", instruction)
        self.assertNotIn("ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА", instruction)

    def test_missing_user_settings_is_treated_as_unknown(self) -> None:
        """No user_settings at all (legacy/incomplete request) is UNKNOWN, not UTC (A1):
        a bot-only owner who never synced a timezone must never get a silent UTC guess —
        the instruction must steer the agent toward ask_questions instead."""
        request = _process_request()
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("ПОЯС ВЛАДЕЛЬЦА НЕИЗВЕСТЕН", instruction)
        self.assertIn("ask_questions", instruction)
        self.assertNotIn("ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА", instruction)

    def test_unknown_timezone_marker_absent_when_timezone_known(self) -> None:
        """The UNKNOWN marker must appear only when the timezone is actually empty/invalid."""
        request = _process_request(user_settings=UserSettingsData(timezone="Europe/Kyiv"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertNotIn("ПОЯС ВЛАДЕЛЬЦА НЕИЗВЕСТЕН", instruction)

    def test_bare_time_is_framed_as_owner_local_never_utc(self) -> None:
        """The strengthened wording forbids reading a bare time as UTC."""
        request = _process_request(user_settings=UserSettingsData(timezone="Europe/Kyiv"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("локальное", instruction)
        self.assertIn("никогда не интерпретируй его как UTC", instruction)

    def test_passed_time_must_roll_forward_or_ask(self) -> None:
        """The strengthened wording requires rolling a passed time forward, or asking."""
        request = _process_request(user_settings=UserSettingsData(timezone="Europe/Kyiv"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("уже прошёл", instruction)
        self.assertIn("ближайшее будущее наступление", instruction)
        self.assertIn("уточняющий вопрос", instruction)

    def test_fire_at_iso_stays_an_absolute_offset_timestamp(self) -> None:
        """fire_at_iso must remain an absolute offset timestamp, not bare local time."""
        request = _process_request(user_settings=UserSettingsData(timezone="Europe/Kyiv"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("fire_at_iso", instruction)
        self.assertIn("смещением", instruction)


class GreetingRuleTests(unittest.TestCase):
    """``_greeting_rule`` trusts the Go-supplied flag as the single source of truth."""

    def test_runtime_flag_overrides_per_line_scan(self) -> None:
        """When the backend says greeting_sent_today=true, do not greet again —
        even though no OWNER line on the current date contains a greeting word."""
        memory = (
            "[CURRENT DATE: 2026-05-30]",
            "[RUNTIME: greeting_sent_today=true]",
            "[2026-05-30 09:00] OWNER: что по задаче?",  # no greeting word present
        )
        rule = draft_pipeline._greeting_rule(memory)
        self.assertIn("do NOT open with a greeting", rule)

    def test_falls_back_to_scan_without_flag(self) -> None:
        """Without the flag, same-day history with no greeting allows a soft open."""
        memory = (
            "[CURRENT DATE: 2026-05-30]",
            "[2026-05-30 09:00] OWNER: что по задаче?",
        )
        rule = draft_pipeline._greeting_rule(memory)
        self.assertIn("ongoing same-day", rule)


class PromptBuilderTests(unittest.TestCase):
    """``PromptBuilder.build`` isolates user data in a single untrusted block."""

    def test_context_wrapped_in_one_block(self) -> None:
        from ai_agent.security import PromptBuilder

        _, payload = PromptBuilder().build("task", "hi", ("line-a", "line-b"))
        # One context envelope, not one per line.
        self.assertEqual(payload.count("<untrusted_context>"), 1)
        self.assertIn("line-a\nline-b", payload)
        self.assertIn("<untrusted_user_message>", payload)

    def test_no_context_emits_no_context_block(self) -> None:
        from ai_agent.security import PromptBuilder

        _, payload = PromptBuilder().build("task", "hi", ())
        self.assertNotIn("<untrusted_context>", payload)
        self.assertIn("<untrusted_user_message>", payload)


class NormalizeRichMarkdownTests(unittest.TestCase):
    """``normalize_rich_markdown`` fixes common model-output heading artefacts."""

    def _norm(self, text: str) -> str:
        return draft_pipeline.normalize_rich_markdown(text)

    def test_inline_heading_after_emoji_gets_paragraph_break(self) -> None:
        """Emoji followed by a heading on the same line must be split."""
        result = self._norm("Hello 😊 ### Заголовок")
        self.assertIn("\n\n### Заголовок", result)
        # The emoji must stay on its own line, not disappear.
        self.assertIn("😊", result)

    def test_heading_after_single_newline_gets_blank_line(self) -> None:
        """A heading immediately after text (single \\n) must get a blank line."""
        result = self._norm("Paragraph.\n## Section")
        self.assertIn("\n\n## Section", result)

    def test_heading_already_separated_is_unchanged(self) -> None:
        """A heading already on its own paragraph must not gain extra newlines."""
        text = "Paragraph.\n\n## Section\n\nContent."
        result = self._norm(text)
        self.assertEqual(result, text)

    def test_plain_response_is_not_modified(self) -> None:
        """Text with no heading markers must be returned as-is."""
        text = "Just a short reply with no markdown."
        self.assertEqual(self._norm(text), text)

    def test_triple_newlines_collapsed(self) -> None:
        """Three or more consecutive newlines are collapsed to exactly two."""
        result = self._norm("A\n\n\n\nB")
        self.assertNotIn("\n\n\n", result)
        self.assertIn("A\n\nB", result)

    def test_idempotent_on_already_normalized_text(self) -> None:
        """A second pass must produce the same output as the first."""
        text = "😊 ### Title\n\nBody text."
        first = self._norm(text)
        second = self._norm(first)
        self.assertEqual(first, second)

    def test_heading_at_start_of_text_unchanged(self) -> None:
        """A heading at the very start of the text needs no preceding blank line."""
        text = "# Title\n\nContent."
        self.assertEqual(self._norm(text), text)

    def test_glued_table_rows_are_split_onto_separate_lines(self) -> None:
        """A table with separator+rows glued onto one line is rebuilt row-per-line.

        This is the real failure mode from production: Telegram's rich parser
        rendered the whole thing as literal ``|`` text because the rows were not
        newline-separated.
        """
        glued = (
            "| Блок | Что сделать | Зачем |\n"
            "|---|---|---| | 1 | Сегменты | Привязать | | 2 | Потоки | Разметить |"
        )
        result = self._norm(glued)
        # Each data row now sits on its own line.
        self.assertIn("| 1 | Сегменты | Привязать |", result)
        self.assertIn("| 2 | Потоки | Разметить |", result)
        # No line still carries two rows' worth of pipes glued together.
        for line in result.splitlines():
            self.assertLessEqual(line.count("|"), 4, line)

    def test_table_width_follows_separator_not_short_header(self) -> None:
        """When the header is missing a column the separator's width wins, so the
        4-value data rows are not mis-split into the header's 3 columns."""
        glued = (
            "| Блок | Что | Зачем |\n"
            "|---|---|---|---| | 1 | A | B | C | | 2 | D | E | F |"
        )
        result = self._norm(glued)
        self.assertIn("| 1 | A | B | C |", result)
        self.assertIn("| 2 | D | E | F |", result)

    def test_wellformed_table_is_untouched_and_idempotent(self) -> None:
        """A correctly formatted table (incl. a blank cell) must pass through as-is."""
        table = "| A | B |\n|---|---|\n| 1 |  |\n|  | 4 |"
        first = self._norm(table)
        self.assertEqual(first, table)
        self.assertEqual(self._norm(first), first)

    def test_markdown_table_validator_flags_inconsistent_widths(self) -> None:
        """The validator reports a table whose separator/rows disagree in width."""
        broken = "| A | B |\n|---|---|---|\n| 1 | 2 | 3 |"
        self.assertTrue(draft_pipeline._markdown_table_issues(broken))
        self.assertFalse(draft_pipeline._markdown_table_issues("| A | B |\n|---|---|\n| 1 | 2 |"))

    def test_repair_then_validate_leaves_no_residual_issues(self) -> None:
        """After normalize repairs a glued table, the validator finds nothing left."""
        glued = "| Блок | Что | Зачем |\n|---|---|---|---| | 1 | A | B | C | | 2 | D | E | F |"
        self.assertFalse(draft_pipeline._markdown_table_issues(self._norm(glued)))

    def test_formatting_line_enforces_heading_rules(self) -> None:
        """The output-format rules forbid # on the same line as text or emoji."""
        role = AIRoleData(name="r", description="d")
        spec = _build_draft_spec(_draft_request(ai_role=role))
        rules = spec.output_format.rules
        self.assertIn("никогда в одной строке", rules)
        self.assertIn("Пустая строка до и после", rules)


class OrchestratorResultCoercionTests(unittest.TestCase):
    """``response_plan`` must never crash a draft into error_fallback (DATA-1).

    The orchestrator's authoritative plan is set from the interaction tool's
    deps after the run, so the model's inline ``response_plan`` value is always
    discarded. Reasoning models nonetheless mis-serialise that nested object —
    most damagingly as a JSON *string* — and a strict object schema turned that
    into a validation error → "Exceeded maximum output retries" → empty reply.
    """

    def test_response_plan_emitted_as_json_string_is_parsed(self) -> None:
        from ai_agent.agents.results import OrchestratorResult, ResponsePlan

        # Exact failure shape observed in production logs.
        plan_str = (
            '{"shape": "questions", "questions": '
            '[{"prompt": "Кто возглавил Октябрьскую революцию?", '
            '"options": ["Ленин", "Керенский"]}]}'
        )
        result = OrchestratorResult(response_plan=plan_str)
        self.assertIsInstance(result.response_plan, ResponsePlan)
        assert result.response_plan is not None
        self.assertEqual(result.response_plan.shape, "questions")
        self.assertEqual(result.response_plan.questions[0].options, ("Ленин", "Керенский"))

    def test_response_plan_null_and_unparseable_strings_become_none(self) -> None:
        from ai_agent.agents.results import OrchestratorResult

        for raw in ("", "null", "None", "not json at all"):
            with self.subTest(raw=raw):
                self.assertIsNone(OrchestratorResult(response_plan=raw).response_plan)


class DraftRetryTests(unittest.TestCase):
    """A transient draft failure must be retried, not surfaced immediately.

    Provider 5xx/rate-limit, a malformed structured output, or the tool-call cap
    fail one attempt; a fresh run usually succeeds. Only after every attempt
    fails does draft_outcome raise so the service returns error_fallback.
    """

    @staticmethod
    def _flaky_writer(fail_times: int) -> tuple[FunctionModel, dict]:
        calls = {"n": 0}

        def respond(messages, info: AgentInfo) -> ModelResponse:
            calls["n"] += 1
            if calls["n"] <= fail_times:
                raise RuntimeError("simulated provider 5xx")
            return ModelResponse(parts=[TextPart(json.dumps({
                "reply": "восстановился после сбоя",
                "intent": "general_message",
                "options": [],
                "confidence": 0.8,
                "topic": "t",
            }))])

        return FunctionModel(respond), calls

    def test_transient_failure_is_retried_then_succeeds(self) -> None:
        writer, calls = self._flaky_writer(fail_times=1)
        runtime = make_test_runtime(draft_model=writer, draft_max_attempts=3)

        outcome = runtime.draft_outcome(_process_request(), RiskLevel.LOW)

        self.assertEqual(outcome.text, "восстановился после сбоя")
        self.assertEqual(calls["n"], 2)  # one failure, one success

    def test_exhausted_attempts_raise_runtime_error(self) -> None:
        writer, calls = self._flaky_writer(fail_times=99)  # never recovers
        runtime = make_test_runtime(draft_model=writer, draft_max_attempts=3)

        with self.assertRaises(RuntimeError):
            runtime.draft_outcome(_process_request(), RiskLevel.LOW)
        self.assertEqual(calls["n"], 3)  # exactly draft_max_attempts tries


if __name__ == "__main__":
    unittest.main()
