from __future__ import annotations

import importlib.util
import pathlib
import sys


ROOT_DIR = pathlib.Path(__file__).resolve().parents[2]
CHECK_PATH = ROOT_DIR / "scripts" / "check_policy_matrix.py"

spec = importlib.util.spec_from_file_location("check_policy_matrix", CHECK_PATH)
assert spec and spec.loader
check_policy_matrix = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = check_policy_matrix
spec.loader.exec_module(check_policy_matrix)


def test_contract_enums_and_security_language_are_frozen() -> None:
    check_policy_matrix.validate_contract_text()


def test_policy_matrix_covers_all_modes_risks_and_privacy_sets() -> None:
    cases = check_policy_matrix.generate_policy_cases()

    assert len(cases) == 5 * 4 * 16
    assert {case.chat_mode for case in cases} == set(check_policy_matrix.EXPECTED_CHAT_MODES)
    assert {case.risk_level for case in cases} == set(check_policy_matrix.EXPECTED_RISK_LEVELS)
    assert {case.privacy_flags for case in cases} == set(check_policy_matrix.privacy_flag_sets())


def test_policy_matrix_matches_owner_risk_policy() -> None:
    """The owner risk-tolerance policy (2026-07-04), asserted invariant-by-invariant.

    Modes/chats that must NEVER auto-reply stay locked; the risk tolerances that
    DO auto-send (auto: low/med/high; semi: low/med) are asserted positively so a
    future regression that silently narrows or widens them is caught.
    """
    for case in check_policy_matrix.generate_policy_cases():
        flags = set(case.privacy_flags)
        allowed = set(case.allowed_decisions)

        # Never-auto surfaces stay locked regardless of risk.
        if case.chat_mode in {"disabled", "silent", "manual"} or "private" in flags:
            assert "auto_reply" not in allowed, case.case_id
            continue

        if case.risk_level == "critical":
            # Handled by a neutral auto-reply, with an owner/ignore backstop; never a plain draft.
            assert "auto_reply" in allowed and "draft" not in allowed, case.case_id
        elif case.chat_mode == "auto":
            # AUTO auto-sends every non-critical risk (low/medium/high).
            assert "auto_reply" in allowed, case.case_id
        elif case.chat_mode == "semi_auto":
            if case.risk_level == "high":
                assert allowed == {"draft"}, case.case_id
            else:
                assert "auto_reply" in allowed, case.case_id


def test_required_security_abuse_cases_are_explicit() -> None:
    required = check_policy_matrix.REQUIRED_SECURITY_CASES

    assert "prompt_injection_no_memory_leak" in required
    assert "prompt_injection_no_chat_mode_change" in required
    assert "prompt_injection_no_forced_auto_reply" in required
    assert "tenant_isolation_no_cross_memory" in required
    assert "no_memory_blocks_memory_writes" in required
    assert "no_training_blocks_learning" in required
    assert "hard_deleted_memory_not_retrieved" in required
    assert "duplicate_telegram_update_single_reply" in required
    assert "duplicate_approval_callback_single_reply" in required
