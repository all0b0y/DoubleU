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

from ai_agent.pipelines import draft as draft_pipeline
from ai_agent.schemas import AIRoleData, ContactProfileData
from ai_agent.schemas import _parse_ai_role, _parse_contact_profile

# Opaque markers: if one appears in the prompt it can only have come from the
# matching profile field — never from a hard-coded default.
OWNER_STYLE = "owner-style-sentinel-α"
CONTACT_CONTEXT = "chat-context-sentinel-β"
TABOO_TOPIC = "taboo-sentinel-γ"


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
        parsed = _parse_ai_role({"name": " n ", "constraints": " c "})
        assert parsed is not None
        self.assertEqual(parsed.name, "n")
        self.assertEqual(parsed.constraints, "c")
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


def _split_sections(prompt: str) -> tuple[str, str]:
    """Return (persona_block, contact_block) substrings of the system prompt."""
    role_start = prompt.index("КТО ТЫ В ЭТОМ ЧАТЕ:")
    contact_start = prompt.index("СОБЕСЕДНИК — КОМУ ПИШЕШЬ:")
    task_start = prompt.index("ЗАДАЧА:")
    return prompt[role_start:contact_start], prompt[contact_start:task_start]


if __name__ == "__main__":
    unittest.main()
