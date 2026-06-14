"""Outcome of one input/output safety scanner pass."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class ScanResult:
    """Result of one scanner inside the input/output safety pipeline.

    ``passed=True`` means the scanner approves the payload; ``passed=False``
    means the scanner blocks it.  ``reason`` is a short machine code (e.g.
    ``"prompt_injection_attempt"``); ``scanner`` is the scanner's name
    (e.g. ``"PromptInjection"``).  ``details`` carries scanner-specific
    metadata for observability (no PII).
    """

    scanner: str
    passed: bool
    reason: str = ""
    latency_ms: float = 0.0
    details: dict[str, Any] = field(default_factory=dict)


class SafetyBlocked(Exception):
    """Raised by InputSafety/OutputSafety to abort an LLM call.

    The agent runtime maps this to a canned-reply ``DraftOutcome`` with
    ``model_used="blocked"`` (and degrades gracefully for non-draft endpoints).
    """

    def __init__(self, reason: str, canned_reply: str = "", scanner: str = "") -> None:
        super().__init__(reason)
        self.reason = reason
        self.canned_reply = canned_reply
        self.scanner = scanner
