"""Contract-alignment tests for shared AI service enums and schemas."""

from __future__ import annotations

import json
import pathlib
import unittest

from ai_agent import contract_enums
from ai_agent.schemas import (
    AIProcessMessageResponse,
    ChatMode,
    Decision,
    LLMPurpose,
    PrivacyFlag,
    ResponsePlanData,
    RiskLevel,
    ScheduledJobData,
    Usage,
)

CONTRACTS_JSON = (
    pathlib.Path(__file__).resolve().parent.parent.parent / "contracts" / "contracts.json"
)


def _values(enum_cls: type) -> set[str]:
    return {item.value for item in enum_cls}


def _load_contract() -> dict:
    return json.loads(CONTRACTS_JSON.read_text())


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

    def test_llm_purpose_values_match_contract(self) -> None:
        """LLMPurpose enum values must match contracts.json llm_purposes."""
        self.assertEqual(
            {item.value for item in LLMPurpose},
            {"draft", "extract", "summarize", "embed", "moderate", "classify"},
        )

    def test_llm_purpose_draft_str_value(self) -> None:
        """LLMPurpose("draft").value must equal "draft" (header attribute)."""
        self.assertEqual(LLMPurpose("draft").value, "draft")

    def test_response_plan_serializes_with_options(self) -> None:
        response = AIProcessMessageResponse(
            decision=Decision.DRAFT,
            response_text="Выберите время",
            confidence=0.9,
            risk_level=RiskLevel.LOW,
            intent="choice_question",
            model_used="test",
            usage=Usage(),
            response_plan=ResponsePlanData(shape="buttons", options=("Сегодня", "Завтра")),
        )
        self.assertEqual(response.to_dict()["response_plan"], {
            "shape": "buttons", "options": ["Сегодня", "Завтра"]
        })

    def test_scheduled_jobs_serialize_for_go_persistence(self) -> None:
        response = AIProcessMessageResponse(
            decision=Decision.DRAFT,
            response_text="Напоминание поставлено",
            confidence=0.9,
            risk_level=RiskLevel.LOW,
            intent="scheduling",
            model_used="test",
            usage=Usage(),
            scheduled_jobs=(ScheduledJobData(
                kind="reminder",
                target="owner",
                fire_at="2026-06-22T15:00:00+00:00",
                payload={"text": "Позвонить клиенту"},
                idempotency_key="scheduler:key",
            ),),
        )
        self.assertEqual(response.to_dict()["scheduled_jobs"], [{
            "kind": "reminder",
            "target": "owner",
            "fire_at": "2026-06-22T15:00:00+00:00",
            "payload": {"text": "Позвонить клиенту"},
            "idempotency_key": "scheduler:key",
            "created_by": "ai",
        }])


class GeneratedEnumsMatchContractJSONTests(unittest.TestCase):
    """Guard against drift between contracts.json and the generated Python file.

    If this test fails, regenerate via ``scripts/generate_contracts.py`` and
    commit the new ``ai_agent/contract_enums.py``.
    """

    def setUp(self) -> None:
        self.contract = _load_contract()

    def test_version_matches(self) -> None:
        self.assertEqual(contract_enums.CONTRACT_VERSION, self.contract["version"])

    def test_decisions(self) -> None:
        self.assertEqual(_values(contract_enums.Decision), set(self.contract["decisions"]))

    def test_tool_categories(self) -> None:
        self.assertEqual(
            _values(contract_enums.ToolCategory),
            set(self.contract["tool_categories"]),
        )

    def test_chat_modes(self) -> None:
        self.assertEqual(_values(contract_enums.ChatMode), set(self.contract["chat_modes"]))

    def test_privacy_flags(self) -> None:
        self.assertEqual(_values(contract_enums.PrivacyFlag), set(self.contract["privacy_flags"]))

    def test_risk_levels(self) -> None:
        self.assertEqual(_values(contract_enums.RiskLevel), set(self.contract["risk_levels"]))

    def test_approval_actions(self) -> None:
        self.assertEqual(
            _values(contract_enums.ApprovalAction),
            set(self.contract["approval_actions"]),
        )

    def test_message_states(self) -> None:
        self.assertEqual(
            _values(contract_enums.MessageState),
            set(self.contract["message_states"]),
        )

    def test_llm_purposes(self) -> None:
        self.assertEqual(
            _values(contract_enums.LLMPurpose),
            set(self.contract["llm_purposes"]),
        )

    def test_confidence_thresholds(self) -> None:
        self.assertEqual(
            contract_enums.SEMI_AUTO_MIN_CONFIDENCE,
            self.contract["confidence_thresholds"]["semi_auto_min"],
        )
        self.assertEqual(
            contract_enums.AUTO_MIN_CONFIDENCE,
            self.contract["confidence_thresholds"]["auto_min"],
        )


if __name__ == "__main__":
    unittest.main()
