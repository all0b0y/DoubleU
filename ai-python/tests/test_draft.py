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
from ai_agent.schemas import ContactProfileData
from ai_agent.schemas import _parse_contact_profile

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
    """Return (owner_role_block, contact_block) substrings of the system prompt."""
    role_start = prompt.index("РОЛЬ — КТО ПИШЕТ:")
    contact_start = prompt.index("СОБЕСЕДНИК — КОМУ ПИШЕМ:")
    task_start = prompt.index("ЗАДАЧА:")
    return prompt[role_start:contact_start], prompt[contact_start:task_start]


if __name__ == "__main__":
    unittest.main()
