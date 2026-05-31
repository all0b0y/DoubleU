"""Phase 3: output safety scanner contract tests."""

from __future__ import annotations

import json
import unittest

from ai_agent.config.flags import FeatureFlags
from ai_agent.contract_enums import LLMPurpose, PrivacyFlag
from ai_agent.observability.context import LLMCallContext, with_context
from ai_agent.safety.output import OutputSafetyScanner
from ai_agent.safety.privacy_filter import filter_for_privacy
from ai_agent.schemas import ModelTask


ENABLED = FeatureFlags(safety_output_enabled=True)


class JSONValidationTests(unittest.TestCase):
    def test_json_schema_repair_succeeds(self) -> None:
        def repair(broken: str) -> str:
            return '{"facts": []}'

        scanner = OutputSafetyScanner(flags=ENABLED, repair_gateway=repair)
        ctx = LLMCallContext(purpose=LLMPurpose.EXTRACT)
        text, flagged = scanner.scan("not json", ModelTask.EXTRACT_FACTS, ctx)
        self.assertFalse(flagged)
        self.assertEqual(json.loads(text), {"facts": []})

    def test_json_schema_repair_fails_returns_empty(self) -> None:
        def bad_repair(broken: str) -> str:
            return "still not json"

        scanner = OutputSafetyScanner(flags=ENABLED, repair_gateway=bad_repair)
        ctx = LLMCallContext(purpose=LLMPurpose.EXTRACT)
        text, flagged = scanner.scan("not json", ModelTask.EXTRACT_FACTS, ctx)
        self.assertTrue(flagged)
        self.assertEqual(text, "")

    def test_json_valid_pass_through(self) -> None:
        scanner = OutputSafetyScanner(flags=ENABLED)
        ctx = LLMCallContext(purpose=LLMPurpose.EXTRACT)
        text, flagged = scanner.scan('{"facts": ["a"]}', ModelTask.EXTRACT_FACTS, ctx)
        self.assertFalse(flagged)
        self.assertEqual(text, '{"facts": ["a"]}')


class ToxicityFlaggingTests(unittest.TestCase):
    def test_toxicity_in_draft_sets_flagged(self) -> None:
        scanner = OutputSafetyScanner(flags=ENABLED)
        ctx = LLMCallContext(purpose=LLMPurpose.DRAFT)
        text, flagged = scanner.scan("kill yourself, idiot", ModelTask.DRAFT_REPLY, ctx)
        # Draft is still returned, but flagged for the bot UI.
        self.assertTrue(flagged)
        self.assertIn("kill yourself", text)
        self.assertTrue(ctx.output_flagged)


class SensitiveLeakTests(unittest.TestCase):
    def test_secret_leak_triggers_flag(self) -> None:
        scanner = OutputSafetyScanner(flags=ENABLED)
        ctx = LLMCallContext(purpose=LLMPurpose.DRAFT)
        text, flagged = scanner.scan("here is the key: sk-AAAAAAAAAAAAAAAAAAAAAAAA", ModelTask.DRAFT_REPLY, ctx)
        self.assertTrue(flagged)


class DeanonymizeTests(unittest.TestCase):
    def test_deanonymize_round_trip(self) -> None:
        scanner = OutputSafetyScanner(flags=ENABLED)
        ctx = LLMCallContext(
            purpose=LLMPurpose.DRAFT,
            redactions={"<EMAIL_1>": "alice@example.com"},
        )
        text, flagged = scanner.scan("Reply to <EMAIL_1> tomorrow.", ModelTask.DRAFT_REPLY, ctx)
        self.assertFalse(flagged)
        self.assertEqual(text, "Reply to alice@example.com tomorrow.")

    def test_unmapped_placeholder_blocked(self) -> None:
        scanner = OutputSafetyScanner(flags=ENABLED)
        ctx = LLMCallContext(
            purpose=LLMPurpose.DRAFT,
            redactions={"<EMAIL_1>": "alice@example.com"},
        )
        text, flagged = scanner.scan("Reply to <EMAIL_42> tomorrow.", ModelTask.DRAFT_REPLY, ctx)
        self.assertTrue(flagged)
        self.assertEqual(text, "")
        last = ctx.safety_results[-1]
        self.assertEqual(last.scanner, "Deanonymize")
        self.assertEqual(last.reason, "hallucinated_placeholder")


class PrivacyFilterTests(unittest.TestCase):
    def test_no_summary_filter_returns_empty(self) -> None:
        text, removed = filter_for_privacy(
            "Some summary text",
            (PrivacyFlag.NO_SUMMARY,),
        )
        self.assertEqual(text, "")
        self.assertEqual(removed, ["entire_summary_no_summary_flag"])

    def test_private_filter_redacts_email_and_phone(self) -> None:
        text, removed = filter_for_privacy(
            "Contact alice@example.com or call +1 415 555 0000",
            (PrivacyFlag.PRIVATE,),
        )
        self.assertIn("[redacted]", text)
        self.assertNotIn("alice@example.com", text)
        self.assertTrue(any("alice@example.com" in r for r in removed))

    def test_no_flags_pass_through(self) -> None:
        text, removed = filter_for_privacy("Hello", ())
        self.assertEqual(text, "Hello")
        self.assertEqual(removed, [])


class FlagDisabledPassThroughTests(unittest.TestCase):
    def test_disabled_returns_input_unchanged(self) -> None:
        scanner = OutputSafetyScanner(flags=FeatureFlags(safety_output_enabled=False))
        ctx = LLMCallContext(purpose=LLMPurpose.DRAFT, redactions={"<EMAIL_1>": "real@x"})
        text, flagged = scanner.scan("Bad <EMAIL_999>", ModelTask.DRAFT_REPLY, ctx)
        self.assertFalse(flagged)
        # Deanonymize did not run since the layer is disabled.
        self.assertEqual(text, "Bad <EMAIL_999>")


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
