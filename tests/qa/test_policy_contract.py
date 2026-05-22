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


def test_policy_matrix_has_no_unsafe_auto_reply_paths() -> None:
    for case in check_policy_matrix.generate_policy_cases():
        flags = set(case.privacy_flags)

        if case.chat_mode in {"disabled", "silent", "manual"}:
            assert "auto_reply" not in case.allowed_decisions, case.case_id

        if case.risk_level in {"medium", "high", "critical"}:
            assert "auto_reply" not in case.allowed_decisions, case.case_id

        if "private" in flags:
            assert "auto_reply" not in case.allowed_decisions, case.case_id


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
