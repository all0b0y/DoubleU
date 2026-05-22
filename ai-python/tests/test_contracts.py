"""Contract-alignment tests for shared AI service enums and schemas."""

from __future__ import annotations

import unittest

from ai_agent.schemas import ChatMode, Decision, PrivacyFlag, RiskLevel


class ContractEnumTests(unittest.TestCase):
    """Verify local enums match the frozen cross-service contract values."""

    def test_decision_values_match_contract(self) -> None:
        """Decision enum values must match contracts/README.md."""
        self.assertEqual(
            {item.value for item in Decision},
            {"draft", "auto_reply", "ignore", "observe", "ask_owner", "error_fallback"},
        )

    def test_chat_mode_values_match_contract(self) -> None:
        """ChatMode enum values must match contracts/README.md."""
        self.assertEqual(
            {item.value for item in ChatMode},
            {"disabled", "silent", "manual", "semi_auto", "auto"},
        )

    def test_privacy_flag_values_match_contract(self) -> None:
        """PrivacyFlag enum values must match contracts/README.md."""
        self.assertEqual(
            {item.value for item in PrivacyFlag},
            {"no_memory", "no_training", "private", "no_summary"},
        )

    def test_risk_level_values_match_contract(self) -> None:
        """RiskLevel enum values must match contracts/README.md."""
        self.assertEqual(
            {item.value for item in RiskLevel},
            {"low", "medium", "high", "critical"},
        )


if __name__ == "__main__":
    unittest.main()
