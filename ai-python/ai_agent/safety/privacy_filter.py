"""Privacy-flag filter for LLM output (DoubleU-specific, not in LLM Guard).

LLM Guard's ``Sensitive`` scanner does not know about DoubleU's privacy_flag
semantics: ``no_summary`` chats must not surface a summary at all,
``private`` chats must strip personal-identifier-looking content from any
generated text.  This module owns that policy.
"""

from __future__ import annotations

import re

from ai_agent.contract_enums import PrivacyFlag


# Tags that should never appear in output going to the owner when the chat is
# flagged ``private`` (e.g. a leaked credential placeholder, a raw email).
_PRIVATE_SENTINEL_RE = re.compile(
    r"\b(?:[\w.+-]+@[\w-]+\.[\w.-]+|\+?\d[\d\s().-]{7,}\d)\b"
)


def filter_for_privacy(
    text: str,
    privacy_flags: tuple[PrivacyFlag, ...],
) -> tuple[str, list[str]]:
    """Return (filtered, removed_segments) per privacy_flags.

    * ``no_summary`` → empty string (summary suppressed entirely).
    * ``private`` → emails and phone numbers redacted to ``[redacted]``.
    Other flags pass through.
    """
    removed: list[str] = []
    if PrivacyFlag.NO_SUMMARY in privacy_flags:
        if text.strip():
            removed.append("entire_summary_no_summary_flag")
        return "", removed
    if PrivacyFlag.PRIVATE in privacy_flags:
        def _sub(match: re.Match[str]) -> str:
            removed.append(match.group(0))
            return "[redacted]"

        text = _PRIVATE_SENTINEL_RE.sub(_sub, text)
    return text, removed
