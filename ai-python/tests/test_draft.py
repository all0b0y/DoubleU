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

from pydantic_ai.messages import ModelResponse, ToolCallPart
from pydantic_ai.models.function import AgentInfo, FunctionModel

from ai_agent.agents.tool_catalog import TOOL_CATALOG
from ai_agent.pipelines import draft as draft_pipeline
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
from ai_agent.security import PromptBuilder

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
    """``_compose_base_instruction`` places profile data in the right sections."""

    def _compose(self, profile: ContactProfileData | None) -> str:
        return draft_pipeline._compose_base_instruction(
            clean_memory=["[CURRENT DATE: 2026-05-30]"],
            owner_style_facts=[],
            contact_profile=profile,
            current_draft=None,
            owner_instruction=None,
            feedback_hint="",
        )

    def test_me_style_is_in_owner_section_not_contact(self) -> None:
        """me_style describes how the OWNER writes — it belongs in the РОЛЬ block."""
        profile = ContactProfileData(relationship_type="friendship", owner_style=OWNER_STYLE)
        role_block, contact_block = _split_sections(self._compose(profile))
        self.assertIn(OWNER_STYLE, role_block)
        self.assertNotIn(OWNER_STYLE, contact_block)

    def test_raw_notes_json_never_appears(self) -> None:
        """The assembled prompt must never contain the raw {"me_style"...} JSON."""
        profile = ContactProfileData(owner_style=OWNER_STYLE, notes='{"me_style":"x"}')
        prompt = self._compose(profile)
        self.assertNotIn('{"me_style"', prompt)
        self.assertNotIn("me_style", prompt)

    def test_structured_contact_fields_render_cleanly(self) -> None:
        """Tone / address form / chat context / taboo appear in the contact block."""
        profile = ContactProfileData(
            address_form="ty",
            tone="casual",
            chat_context=CONTACT_CONTEXT,
            do_not_discuss=(TABOO_TOPIC,),
        )
        _, contact_block = _split_sections(self._compose(profile))
        self.assertIn("на «ты»", contact_block)
        self.assertIn("casual", contact_block)
        self.assertIn(CONTACT_CONTEXT, contact_block)
        self.assertIn(TABOO_TOPIC, contact_block)

    def test_facts_are_framed_as_optional_background(self) -> None:
        """The prompt instructs the model to treat [ФАКТ ...] lines as soft context."""
        prompt = self._compose(None)
        self.assertIn("[ФАКТ", prompt)
        self.assertIn("справочный фон", prompt)

    def test_contact_extras_render_in_contact_block(self) -> None:
        """Topics/language/timezone/preferred-address (sent by Go) reach the prompt."""
        profile = ContactProfileData(
            topics=("topic-sentinel",),
            language="lang-sentinel",
            contact_timezone="tz-sentinel",
            preferred_address="addr-sentinel",
        )
        _, contact_block = _split_sections(self._compose(profile))
        self.assertIn("topic-sentinel", contact_block)
        self.assertIn("lang-sentinel", contact_block)
        self.assertIn("tz-sentinel", contact_block)
        self.assertIn("addr-sentinel", contact_block)

    def test_owner_identity_renders_in_role_block(self) -> None:
        """user_settings identity lands in the OWNER role block, not the contact block."""
        from ai_agent.schemas import UserSettingsData

        prompt = draft_pipeline._compose_base_instruction(
            clean_memory=["[CURRENT DATE: 2026-05-30]"],
            owner_style_facts=[],
            contact_profile=None,
            current_draft=None,
            owner_instruction=None,
            feedback_hint="",
            user_settings=UserSettingsData(display_name="owner-name-sentinel", timezone="tz-x"),
        )
        role_block, contact_block = _split_sections(prompt)
        self.assertIn("owner-name-sentinel", role_block)
        self.assertNotIn("owner-name-sentinel", contact_block)


class AIRolePromptTests(unittest.TestCase):
    """The assigned AI role drives the persona block of the system prompt."""

    ROLE = AIRoleData(
        name="role-name-sentinel",
        description="role-description-sentinel",
        speaking_style="role-style-sentinel",
        goals="role-goals-sentinel",
        constraints="role-constraints-sentinel",
    )

    def _compose(self, ai_role: AIRoleData | None, **kwargs) -> str:
        return draft_pipeline._compose_base_instruction(
            clean_memory=["[CURRENT DATE: 2026-05-30]"],
            owner_style_facts=kwargs.pop("owner_style_facts", []),
            contact_profile=kwargs.pop("contact_profile", None),
            current_draft=None,
            owner_instruction=None,
            feedback_hint=kwargs.pop("feedback_hint", ""),
            ai_role=ai_role,
            **kwargs,
        )

    def test_role_fields_render_in_persona_block(self) -> None:
        """Name, description, style, goals and constraints all reach the persona block."""
        persona_block, contact_block = _split_sections(self._compose(self.ROLE))
        for sentinel in (
            "role-name-sentinel",
            "role-description-sentinel",
            "role-style-sentinel",
            "role-goals-sentinel",
            "role-constraints-sentinel",
        ):
            self.assertIn(sentinel, persona_block)
            self.assertNotIn(sentinel, contact_block)

    def test_role_style_wins_over_owner_style(self) -> None:
        """With a role speaking_style set, the owner's observed style is not injected."""
        profile = ContactProfileData(owner_style=OWNER_STYLE)
        prompt = self._compose(self.ROLE, contact_profile=profile)
        self.assertIn("role-style-sentinel", prompt)
        self.assertNotIn(OWNER_STYLE, prompt)

    def test_role_without_style_falls_back_to_owner_style(self) -> None:
        """A role with no speaking_style borrows the owner's per-chat style as a hint."""
        role = AIRoleData(name="role-name-sentinel", description="d")
        profile = ContactProfileData(owner_style=OWNER_STYLE)
        persona_block, _ = _split_sections(self._compose(role, contact_profile=profile))
        self.assertIn(OWNER_STYLE, persona_block)

    def test_bot_role_uses_owner_bio_as_context_not_personality(self) -> None:
        """Bot mode may reason from owner facts without inheriting the owner's voice."""
        from ai_agent.schemas import UserSettingsData

        role = AIRoleData(
            name="bot-role-sentinel",
            description="bot-description-sentinel",
            speak_as_owner=False,
        )
        prompt = self._compose(
            role,
            owner_style_facts=[OWNER_STYLE],
            contact_profile=ContactProfileData(owner_style=OWNER_STYLE),
            user_settings=UserSettingsData(
                display_name="owner-name-sentinel",
                bio="owner-bio-sentinel",
            ),
            feedback_hint="owner-feedback-sentinel",
        )

        self.assertIn("Твоя единственная персона", prompt)
        self.assertIn("bot-role-sentinel", prompt)
        self.assertIn("не копируй личность", prompt.lower())
        self.assertIn("выше остальных инструкций", prompt)
        self.assertIn("СПРАВОЧНЫЙ КОНТЕКСТ, НЕ ПЕРСОНА БОТА", prompt)
        self.assertIn("owner-name-sentinel", prompt)
        self.assertIn("owner-bio-sentinel", prompt)
        self.assertIn("точный совет, объяснение или вывод", prompt)
        self.assertNotIn(OWNER_STYLE, prompt)
        self.assertNotIn("owner-feedback-sentinel", prompt)

    def test_no_role_falls_back_to_owner_assistant_persona(self) -> None:
        """Without a role the prompt keeps the legacy copy-the-owner behaviour."""
        profile = ContactProfileData(owner_style=OWNER_STYLE)
        persona_block, _ = _split_sections(self._compose(None, contact_profile=profile))
        self.assertIn("Роль для чата не задана", persona_block)
        self.assertIn(OWNER_STYLE, persona_block)

    def test_prompt_pins_memory_to_this_chat(self) -> None:
        """The prompt forbids referencing conversations outside this chat's context."""
        prompt = self._compose(self.ROLE)
        self.assertIn("только из этого чата", prompt)

    def test_prompt_frames_markup_as_readability_tool(self) -> None:
        """The compact prompt names the Rich Markdown dialect and useful GFM blocks."""
        prompt = self._compose(self.ROLE)

        self.assertIn("Telegram Rich Markdown (GFM)", prompt)
        self.assertIn("GFM-таблицы", prompt)
        self.assertIn("Короткий ответ пиши обычным текстом", prompt)

    def test_two_stage_writer_directive_is_opt_in(self) -> None:
        """Only the bimodal writer is told that tools are unavailable."""
        single_stage = self._compose(self.ROLE)
        two_stage = self._compose(self.ROLE, two_stage=True)
        self.assertNotIn("КОНТЕКСТ УЖЕ СОБРАН", single_stage)
        self.assertIn("КОНТЕКСТ УЖЕ СОБРАН", two_stage)
        self.assertIn("НЕТ инструментов", two_stage)

    def test_present_buttons_instruction_is_single_stage_only(self) -> None:
        """Single-stage writer is told to call present_buttons; bimodal writer is not."""
        role = AIRoleData(
            name="role-name-sentinel",
            tool_settings={"interaction.present_buttons": True},
        )
        request = _process_request(ai_role=role)

        single_stage, _ = draft_pipeline.build_draft_prompt(request, PromptBuilder())
        two_stage, _ = draft_pipeline.build_draft_prompt(
            request, PromptBuilder(), orchestrator_bundle=("context",)
        )

        self.assertIn("present_buttons(prompt, options[2..4])", single_stage)
        self.assertIn("Не описывай кнопки словами", single_stage)
        self.assertNotIn("present_buttons(prompt, options[2..4])", two_stage)

    def test_single_stage_writer_present_buttons_reaches_draft_outcome(self) -> None:
        """A writer tool call in single-stage mode becomes the final response_plan."""
        calls = {"writer": 0}

        def writer(messages, info: AgentInfo) -> ModelResponse:
            calls["writer"] += 1
            if calls["writer"] == 1:
                return ModelResponse(parts=[ToolCallPart(
                    tool_name="present_buttons",
                    args={
                        "prompt": "Какой вариант вам удобнее?",
                        "options": ["Сегодня", "Завтра"],
                    },
                )])
            return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args={
                "reply": "writer placeholder",
                "intent": "general_message",
                "options": [],
                "confidence": 0.9,
                "topic": "выбор времени",
            })])

        request = _process_request(
            text="Когда созвонимся?",
            ai_role=AIRoleData(
                name="Support",
                tool_settings={
                    **{entry.key: False for entry in TOOL_CATALOG},
                    "interaction.present_buttons": True,
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
        self.assertEqual(outcome.text, "Какой вариант вам удобнее?")
        self.assertEqual(outcome.intent, "choice_question")
        self.assertEqual(outcome.options, ("Сегодня", "Завтра"))
        self.assertIsNotNone(outcome.response_plan)
        assert outcome.response_plan is not None
        self.assertEqual(outcome.response_plan.shape, "buttons")
        self.assertEqual(outcome.response_plan.options, ("Сегодня", "Завтра"))

    def test_no_role_prompt_forbids_markup(self) -> None:
        """The legacy owner-copy path remains plain to match its delivery contract."""
        self.assertIn("Пиши обычным текстом без разметки", self._compose(None))

    def test_decide_text_format_uses_rich_for_role_structured_intents(self) -> None:
        """Assigned roles may return Rich Markdown for structured answer intents."""
        for intent in ("question", "scheduling", "commercial", "request", "general_message"):
            self.assertEqual(draft_pipeline.decide_text_format(self.ROLE, intent), "rich")

    def test_decide_text_format_keeps_copy_and_short_replies_plain(self) -> None:
        """Legacy owner-copy replies and short choice/greeting-style intents stay plain."""
        self.assertEqual(draft_pipeline.decide_text_format(None, "question"), "plain")
        for intent in ("choice_question", "acknowledgement"):
            self.assertEqual(draft_pipeline.decide_text_format(self.ROLE, intent), "plain")

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
        persona_block, contact_block = _split_sections(self._compose(role))
        for sentinel in ("cf-title-sentinel", "cf-desc-sentinel", "cf-context-sentinel"):
            self.assertIn(sentinel, persona_block)
            self.assertNotIn(sentinel, contact_block)

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

    def test_invalid_timezone_falls_back_to_utc(self) -> None:
        """An unknown IANA name degrades to UTC instead of raising."""
        request = _process_request(user_settings=UserSettingsData(timezone="Not/AZone"))
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("(UTC)", instruction)

    def test_missing_user_settings_falls_back_to_utc(self) -> None:
        """No user_settings at all (legacy/incomplete request) also defaults to UTC."""
        request = _process_request()
        instruction = draft_pipeline._current_time_instruction(request, self._NOON_UTC)
        self.assertIn("(UTC)", instruction)

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

    def test_formatting_line_enforces_heading_rules(self) -> None:
        """The formatting prompt forbids # on the same line as text or emoji."""
        role = AIRoleData(name="r", description="d")
        prompt = draft_pipeline._compose_base_instruction(
            clean_memory=[],
            owner_style_facts=[],
            contact_profile=None,
            current_draft=None,
            owner_instruction=None,
            feedback_hint="",
            ai_role=role,
        )
        self.assertIn("никогда в одной строке", prompt)
        self.assertIn("пустая строка до и после", prompt)


def _split_sections(prompt: str) -> tuple[str, str]:
    """Return (persona_block, contact_block) substrings of the system prompt."""
    role_start = prompt.index("КТО ТЫ В ЭТОМ ЧАТЕ:")
    contact_start = prompt.index("СОБЕСЕДНИК — КОМУ ПИШЕШЬ:")
    task_start = prompt.index("ЗАДАЧА:")
    return prompt[role_start:contact_start], prompt[contact_start:task_start]


if __name__ == "__main__":
    unittest.main()
