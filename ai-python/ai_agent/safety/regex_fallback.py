"""Regex-only safety scanners used when LLM Guard is unavailable.

LLM Guard's transformer-based ``PromptInjection`` model is ~500 MB and slow to
cold-start; this module provides a deterministic, dependency-free fallback that
handles the common cases (PII anonymization, simple injection heuristics,
toxicity keyword bans, secret detection).  It runs in constant time per input.

The same module is exercised in MOCK_LLM tests so behaviour is identical with
and without LLM Guard installed.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Pattern


# --- PII patterns -------------------------------------------------------
_EMAIL_RE: Pattern[str] = re.compile(r"\b[\w.+-]+@[\w-]+\.[\w.-]+\b")
_PHONE_RE: Pattern[str] = re.compile(r"\+?\d[\d\s().-]{6,}\d")
_IBAN_RE: Pattern[str] = re.compile(r"\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b")
_IPV4_RE: Pattern[str] = re.compile(r"\b(?:\d{1,3}\.){3}\d{1,3}\b")

# --- Secret patterns ----------------------------------------------------
_OPENAI_KEY_RE: Pattern[str] = re.compile(r"\bsk-[A-Za-z0-9]{20,}\b")
_AWS_KEY_RE: Pattern[str] = re.compile(r"\bAKIA[0-9A-Z]{16}\b")
_GITHUB_TOKEN_RE: Pattern[str] = re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b")
_JWT_RE: Pattern[str] = re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b")
_BEARER_RE: Pattern[str] = re.compile(r"(?i)bearer\s+[A-Za-z0-9._\-]{20,}")
# Generic card number with Luhn validation applied below.
_CARD_RE: Pattern[str] = re.compile(r"\b(?:\d[ -]?){13,19}\b")

# --- Prompt injection heuristics ---------------------------------------
# Designed for high precision: hits well-known injection idioms in English /
# Russian.  False positives are acceptable in security context.
_INJECTION_PATTERNS: tuple[Pattern[str], ...] = tuple(
    re.compile(p, re.IGNORECASE)
    for p in (
        r"ignore (the |all |any )?(previous|prior|above|earlier) (instructions|prompts|messages|rules)",
        r"disregard (the |all |any )?(previous|prior|above|earlier) (instructions|prompts|messages|rules)",
        r"forget (everything|all|previous|prior) (you (were told|know)|instructions|prompts)?",
        r"you are now (?:a|an)\b",
        r"(?:act|behave|pretend) as (?:a|an|the )?",
        r"system prompt(?:\s*[:=])",
        r"jailbreak",
        r"developer mode (enabled|on)",
        r"do anything now",
        r"\b(role|persona)\s*[:=]\s*\"?(admin|root|system)",
        r"забуд[ьте]+ (все|всё) ((что|которое) (тебе|вам))?",
        r"игнорируй (предыдущ|все|всё)",
    )
)

# --- Toxicity / banlist keyword sets -----------------------------------
# Intentionally short; production callers swap LLM Guard's Toxicity model in.
_TOXIC_KEYWORDS: tuple[str, ...] = (
    "fuck off", "kill yourself", "go die", "kys",
    "идиот", "придурок",
)

_TOPIC_BAN_KEYWORDS: tuple[str, ...] = (
    "build a bomb", "synthesize meth", "child porn", "csam",
)


@dataclass
class RegexScanOutcome:
    """Outcome of one regex-fallback pass over an input string."""

    cleaned: str
    redactions: dict[str, str]
    injection_detected: bool
    injection_match: str
    toxicity_detected: bool
    toxicity_match: str
    banned_topic_detected: bool
    banned_topic_match: str
    secrets_detected: bool
    secret_match: str


def _luhn(digits: str) -> bool:
    """Standard Luhn checksum for card numbers."""
    s = 0
    alt = False
    for ch in reversed(digits):
        if not ch.isdigit():
            continue
        d = int(ch)
        if alt:
            d *= 2
            if d > 9:
                d -= 9
        s += d
        alt = not alt
    return s % 10 == 0 and len(digits) >= 13


def _replace_collect(pattern: Pattern[str], tag: str, text: str, redactions: dict[str, str]) -> str:
    """Replace each ``pattern`` match in ``text`` with ``<TAG_n>`` and record the original."""
    def _sub(match: re.Match[str]) -> str:
        original = match.group(0)
        idx = sum(1 for k in redactions if k.startswith(f"<{tag}_"))
        placeholder = f"<{tag}_{idx + 1}>"
        redactions[placeholder] = original
        return placeholder

    return pattern.sub(_sub, text)


def anonymize(text: str) -> tuple[str, dict[str, str]]:
    """Replace emails / phones / IBANs / IPs with stable placeholders.

    Returns the rewritten text plus a mapping ``placeholder -> original`` so the
    output-safety layer can deanonymize the LLM response in Phase 3.
    """
    redactions: dict[str, str] = {}
    rewritten = _replace_collect(_EMAIL_RE, "EMAIL", text, redactions)
    rewritten = _replace_collect(_PHONE_RE, "PHONE", rewritten, redactions)
    rewritten = _replace_collect(_IBAN_RE, "IBAN", rewritten, redactions)
    rewritten = _replace_collect(_IPV4_RE, "IP", rewritten, redactions)
    # Card numbers: only redact when the Luhn checksum is valid.
    def _maybe_card(match: re.Match[str]) -> str:
        original = match.group(0)
        digits = re.sub(r"\D", "", original)
        if not _luhn(digits):
            return original
        idx = sum(1 for k in redactions if k.startswith("<CARD_"))
        placeholder = f"<CARD_{idx + 1}>"
        redactions[placeholder] = original
        return placeholder

    rewritten = _CARD_RE.sub(_maybe_card, rewritten)
    return rewritten, redactions


def detect_secrets(text: str) -> tuple[str, dict[str, str], str]:
    """Replace secret tokens with placeholders and report the first match."""
    redactions: dict[str, str] = {}
    first_match = ""
    for tag, pattern in (
        ("OPENAI_KEY", _OPENAI_KEY_RE),
        ("AWS_KEY", _AWS_KEY_RE),
        ("GITHUB_TOKEN", _GITHUB_TOKEN_RE),
        ("JWT", _JWT_RE),
        ("BEARER", _BEARER_RE),
    ):
        match = pattern.search(text)
        if match and not first_match:
            first_match = tag
        text = _replace_collect(pattern, tag, text, redactions)
    return text, redactions, first_match


def detect_injection(text: str) -> str:
    """Return the first prompt-injection idiom found, or empty string."""
    for pattern in _INJECTION_PATTERNS:
        m = pattern.search(text)
        if m:
            return m.group(0)
    return ""


def detect_toxicity(text: str) -> str:
    """Return the first toxic keyword found, or empty string."""
    lowered = text.lower()
    for kw in _TOXIC_KEYWORDS:
        if kw in lowered:
            return kw
    return ""


def detect_banned_topic(text: str) -> str:
    """Return the first banned-topic phrase found, or empty string."""
    lowered = text.lower()
    for kw in _TOPIC_BAN_KEYWORDS:
        if kw in lowered:
            return kw
    return ""


def scan_all(text: str) -> RegexScanOutcome:
    """Run every regex scanner against ``text`` and merge redactions."""
    rewritten, redactions = anonymize(text)
    rewritten, secret_redactions, secret_match = detect_secrets(rewritten)
    redactions.update(secret_redactions)
    injection = detect_injection(text)
    toxicity = detect_toxicity(text)
    banned = detect_banned_topic(text)
    return RegexScanOutcome(
        cleaned=rewritten,
        redactions=redactions,
        injection_detected=bool(injection),
        injection_match=injection,
        toxicity_detected=bool(toxicity),
        toxicity_match=toxicity,
        banned_topic_detected=bool(banned),
        banned_topic_match=banned,
        secrets_detected=bool(secret_match),
        secret_match=secret_match,
    )
