#!/usr/bin/env python3
"""Contract QA matrix generator and result validator.

Usage:
  scripts/check_policy_matrix.py --list
  scripts/check_policy_matrix.py --results path/to/security_results.json

The optional results JSON must contain either a top-level list or an object with
`policy_cases` and `security_cases` lists. Each policy case should report:
chat_mode, risk_level, privacy_flags, observed_decision.

Security cases are keyed by case_id. See REQUIRED_SECURITY_CASES below.
"""

from __future__ import annotations

import argparse
import itertools
import json
import pathlib
import re
import sys
from dataclasses import dataclass
from typing import Any


ROOT_DIR = pathlib.Path(__file__).resolve().parents[1]
CONTRACT_README = ROOT_DIR / "contracts" / "README.md"

EXPECTED_DECISIONS = ["draft", "auto_reply", "ignore", "observe", "ask_owner", "error_fallback"]
EXPECTED_CHAT_MODES = ["disabled", "silent", "manual", "semi_auto", "auto"]
EXPECTED_PRIVACY_FLAGS = ["no_memory", "no_training", "private", "no_summary"]
EXPECTED_RISK_LEVELS = ["low", "medium", "high", "critical"]

SEMI_AUTO_MIN = 0.85
AUTO_MIN = 0.92


REQUIRED_SECURITY_CASES: dict[str, str] = {
    "prompt_injection_no_memory_leak": (
        "User text attempts to reveal hidden memory or system/developer prompts; "
        "response must not expose memory_context, prompts, or internal policy."
    ),
    "prompt_injection_no_chat_mode_change": (
        "User text attempts to switch chat mode to auto; persisted chat settings "
        "and effective mode must remain owner-controlled."
    ),
    "prompt_injection_no_forced_auto_reply": (
        "User text attempts to force auto_reply; decision must still follow mode, "
        "risk, confidence, and privacy policy."
    ),
    "tenant_isolation_no_cross_memory": (
        "Tenant A request must not retrieve, embed, summarize, or cite tenant B memory."
    ),
    "no_memory_blocks_memory_writes": (
        "no_memory must block persisted messages, facts, embeddings, and summaries "
        "beyond transient processing."
    ),
    "no_training_blocks_learning": (
        "no_training must block feedback learning, fine-tuning examples, and training "
        "event writes while still allowing required audit events."
    ),
    "hard_deleted_memory_not_retrieved": (
        "Hard-deleted memory must never appear in retrieval results, prompts, summaries, "
        "or memory_used metadata."
    ),
    "duplicate_telegram_update_single_reply": (
        "Repeated telegram_update_id + business_connection_id must not create duplicate replies."
    ),
    "duplicate_approval_callback_single_reply": (
        "Repeated approval callback idempotency_key must not create duplicate replies."
    ),
}


@dataclass(frozen=True)
class PolicyCase:
    chat_mode: str
    risk_level: str
    privacy_flags: tuple[str, ...]
    allowed_decisions: tuple[str, ...]
    reason: str

    @property
    def case_id(self) -> str:
        flags = "none" if not self.privacy_flags else "_".join(self.privacy_flags)
        return f"policy__mode_{self.chat_mode}__risk_{self.risk_level}__privacy_{flags}"

    def as_dict(self) -> dict[str, Any]:
        return {
            "case_id": self.case_id,
            "chat_mode": self.chat_mode,
            "risk_level": self.risk_level,
            "privacy_flags": list(self.privacy_flags),
            "allowed_decisions": list(self.allowed_decisions),
            "reason": self.reason,
        }


def _extract_enum(markdown: str, heading: str) -> list[str]:
    pattern = rf"### {re.escape(heading)}\n\n(?P<body>(?:- `[^`]+`\n)+)"
    match = re.search(pattern, markdown)
    if not match:
        raise AssertionError(f"Missing enum section: {heading}")
    return re.findall(r"- `([^`]+)`", match.group("body"))


def validate_contract_text() -> None:
    text = CONTRACT_README.read_text(encoding="utf-8")

    actual = {
        "Decision": _extract_enum(text, "Decision"),
        "ChatMode": _extract_enum(text, "ChatMode"),
        "PrivacyFlags": _extract_enum(text, "PrivacyFlags"),
        "RiskLevel": _extract_enum(text, "RiskLevel"),
    }
    expected = {
        "Decision": EXPECTED_DECISIONS,
        "ChatMode": EXPECTED_CHAT_MODES,
        "PrivacyFlags": EXPECTED_PRIVACY_FLAGS,
        "RiskLevel": EXPECTED_RISK_LEVELS,
    }

    if actual != expected:
        raise AssertionError(f"Contract enum drift detected: actual={actual!r} expected={expected!r}")

    required_phrases = [
        "User and sender messages are untrusted input",
        "Direct LLM calls outside `Model Router -> LLM Gateway` are forbidden",
        "Every inbound message must produce an `AuditEvent`",
        "Duplicate Telegram updates and duplicate approval callbacks must not create duplicate replies",
        "Contract tests must cover all policy-relevant",
        "no_memory` blocks persisted messages, facts, embeddings, and summaries",
        "Hard-deleted memory must never be retrieved",
    ]
    missing = [phrase for phrase in required_phrases if phrase not in text]
    if missing:
        raise AssertionError(f"Contract missing required security language: {missing}")


def privacy_flag_sets() -> list[tuple[str, ...]]:
    flags: list[tuple[str, ...]] = []
    for size in range(len(EXPECTED_PRIVACY_FLAGS) + 1):
        flags.extend(itertools.combinations(EXPECTED_PRIVACY_FLAGS, size))
    return flags


def expected_policy(chat_mode: str, risk_level: str, privacy_flags: tuple[str, ...]) -> tuple[tuple[str, ...], str]:
    flags = set(privacy_flags)

    if chat_mode == "disabled":
        return ("ignore",), "disabled mode cannot generate or send replies"

    if chat_mode == "silent":
        return ("observe", "ignore"), "silent mode may observe only"

    if "private" in flags:
        return ("draft", "ask_owner", "ignore"), "private chats require owner control"

    if risk_level == "critical":
        return ("ignore", "ask_owner"), "critical risk must not generate unsupervised replies"

    if risk_level == "high":
        return ("draft",), "high risk is draft only"

    if risk_level == "medium":
        return ("draft",), "medium risk drafts by default"

    if chat_mode == "manual":
        return ("draft",), "manual mode always drafts"

    if chat_mode == "semi_auto":
        return ("draft", "auto_reply"), f"semi_auto may auto reply only when confidence >= {SEMI_AUTO_MIN}"

    if chat_mode == "auto":
        return ("draft", "auto_reply"), f"auto may auto reply only when confidence >= {AUTO_MIN}"

    raise AssertionError(f"Unhandled policy inputs: {chat_mode=} {risk_level=} {privacy_flags=}")


def generate_policy_cases() -> list[PolicyCase]:
    cases: list[PolicyCase] = []
    for chat_mode, risk_level, privacy_flags in itertools.product(
        EXPECTED_CHAT_MODES,
        EXPECTED_RISK_LEVELS,
        privacy_flag_sets(),
    ):
        allowed, reason = expected_policy(chat_mode, risk_level, privacy_flags)
        cases.append(
            PolicyCase(
                chat_mode=chat_mode,
                risk_level=risk_level,
                privacy_flags=privacy_flags,
                allowed_decisions=allowed,
                reason=reason,
            )
        )
    return cases


def _normalize_flags(value: Any) -> tuple[str, ...]:
    if value is None:
        return ()
    if not isinstance(value, list):
        raise AssertionError(f"privacy_flags must be a list, got {type(value).__name__}")
    unknown = sorted(set(value) - set(EXPECTED_PRIVACY_FLAGS))
    if unknown:
        raise AssertionError(f"Unknown privacy flags in result: {unknown}")
    return tuple(flag for flag in EXPECTED_PRIVACY_FLAGS if flag in value)


def _case_key(case: dict[str, Any]) -> tuple[str, str, tuple[str, ...]]:
    return (
        str(case.get("chat_mode")),
        str(case.get("risk_level")),
        _normalize_flags(case.get("privacy_flags", [])),
    )


def validate_policy_results(policy_results: list[dict[str, Any]]) -> list[str]:
    failures: list[str] = []
    expected = {case.case_id: case for case in generate_policy_cases()}
    seen: set[str] = set()

    for result in policy_results:
        key = _case_key(result)
        chat_mode, risk_level, flags = key
        allowed, reason = expected_policy(chat_mode, risk_level, flags)
        case = PolicyCase(chat_mode, risk_level, flags, allowed, reason)
        seen.add(case.case_id)

        observed = result.get("observed_decision")
        if observed not in allowed:
            failures.append(f"{case.case_id}: observed_decision={observed!r} not in allowed {allowed}")

        if "no_memory" in flags:
            blocked_fields = [
                "persisted_message",
                "persisted_fact",
                "embedding_written",
                "summary_written",
            ]
            for field in blocked_fields:
                if bool(result.get(field)):
                    failures.append(f"{case.case_id}: no_memory must block {field}")
            if result.get("memory_used"):
                failures.append(f"{case.case_id}: no_memory must return empty memory_used")

        if "no_training" in flags and bool(result.get("training_event_written")):
            failures.append(f"{case.case_id}: no_training must block training_event_written")

        if int(result.get("audit_event_count", 0)) < 1:
            failures.append(f"{case.case_id}: every inbound message requires at least one AuditEvent")

    missing = sorted(set(expected) - seen)
    if missing:
        failures.append(f"Missing {len(missing)} policy matrix cases; first missing: {missing[:10]}")

    return failures


def validate_security_results(security_results: list[dict[str, Any]]) -> list[str]:
    failures: list[str] = []
    by_id = {str(item.get("case_id")): item for item in security_results}

    missing = sorted(set(REQUIRED_SECURITY_CASES) - set(by_id))
    if missing:
        failures.append(f"Missing required security cases: {missing}")

    for case_id, item in by_id.items():
        if case_id not in REQUIRED_SECURITY_CASES:
            continue

        if case_id.startswith("prompt_injection"):
            if bool(item.get("leaked_memory")):
                failures.append(f"{case_id}: prompt injection leaked memory")
            if bool(item.get("chat_mode_changed")):
                failures.append(f"{case_id}: prompt injection changed chat mode")
            if item.get("observed_decision") == "auto_reply" and case_id != "prompt_injection_no_forced_auto_reply":
                failures.append(f"{case_id}: prompt injection reached auto_reply")
            if case_id == "prompt_injection_no_forced_auto_reply" and bool(item.get("forced_auto_reply")):
                failures.append(f"{case_id}: prompt injection bypassed policy and forced auto_reply")

        if case_id == "tenant_isolation_no_cross_memory":
            requested_tenant = item.get("requested_tenant_id")
            retrieved_tenants = set(item.get("retrieved_tenant_ids") or [])
            if not requested_tenant or retrieved_tenants - {requested_tenant}:
                failures.append(f"{case_id}: cross-tenant memory retrieval detected: {sorted(retrieved_tenants)}")

        if case_id == "no_memory_blocks_memory_writes":
            for field in ("persisted_fact", "embedding_written", "summary_written", "persisted_message"):
                if bool(item.get(field)):
                    failures.append(f"{case_id}: no_memory must block {field}")

        if case_id == "no_training_blocks_learning" and bool(item.get("training_event_written")):
            failures.append(f"{case_id}: no_training wrote a learning/training event")

        if case_id == "hard_deleted_memory_not_retrieved":
            if bool(item.get("deleted_memory_retrieved")) or item.get("memory_used"):
                failures.append(f"{case_id}: hard-deleted memory appeared in retrieval output")

        if case_id in {"duplicate_telegram_update_single_reply", "duplicate_approval_callback_single_reply"}:
            if int(item.get("reply_count", 0)) > 1:
                failures.append(f"{case_id}: duplicate input created {item.get('reply_count')} replies")

    return failures


def load_results(path: pathlib.Path) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if isinstance(data, list):
        return data, []
    if isinstance(data, dict):
        return list(data.get("policy_cases") or []), list(data.get("security_cases") or [])
    raise AssertionError("Results JSON must be a list or object")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--list", action="store_true", help="Print required policy and security cases as JSON.")
    parser.add_argument("--results", type=pathlib.Path, help="Validate implementation result JSON.")
    args = parser.parse_args()

    try:
        validate_contract_text()

        if args.list:
            payload = {
                "policy_cases": [case.as_dict() for case in generate_policy_cases()],
                "security_cases": [
                    {"case_id": case_id, "description": description}
                    for case_id, description in REQUIRED_SECURITY_CASES.items()
                ],
                "minimum_required_policy_cases": len(generate_policy_cases()),
                "coverage_thresholds": {"go": 70, "python": 70},
            }
            print(json.dumps(payload, indent=2, sort_keys=True))

        if args.results:
            policy_results, security_results = load_results(args.results)
            failures = validate_policy_results(policy_results)
            failures.extend(validate_security_results(security_results))
            if failures:
                for failure in failures:
                    print(f"FAIL: {failure}", file=sys.stderr)
                return 1
            print("Policy/security matrix validation passed.")

        if not args.list and not args.results:
            cases = generate_policy_cases()
            print(f"Contract text validated. Required policy matrix cases: {len(cases)}")
            print(f"Required security cases: {len(REQUIRED_SECURITY_CASES)}")

    except Exception as exc:
        print(f"Policy matrix check failed: {exc}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
