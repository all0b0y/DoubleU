"""Deterministic risk, intent, and memory classifiers."""

from __future__ import annotations

import re

from .schemas import IntentResponse, MemoryExtractionResponse, PrivacyFlag, RiskLevel, RiskResponse
from .security import contains_prompt_injection


class RiskClassifier:
    """Classify message risk using deterministic MVP policy rules."""

    # Profanity patterns (Russian and English common words).
    _PROFANITY_RU = (
        "хуй", "пизд", "ебл", "ёбл", "еблан", "залуп", "пиздец", "бля", "блядь",
        "сука", "пидор", "пидар", "мудак", "ёб", "заеб", "наеб", "выеб", "охуе",
        "ахуе", "ахуел", "ахуела", "охуел", "охуела", "ебать", "уёб",
    )
    _PROFANITY_EN = ("fuck", "shit", "asshole", "bitch", "cunt", "dick", "pussy")

    # Sexual content patterns.
    _SEXUAL = (
        "секс", "трах", "отсос", "минет", "куни", "анал", "эро", "порно",
        "голый", "голая", "раздевай", "раздевись", "покажи грудь", "покажи попу",
        "sex", "nude", "porn", "blowjob", "erotic", "naked", "horny",
    )

    def classify(self, message_text: str) -> RiskResponse:
        """Return a risk level and concise reasons for the supplied message."""
        text = message_text.lower()
        reasons: list[str] = []
        level = RiskLevel.LOW

        if contains_prompt_injection(message_text):
            reasons.append("prompt_injection_attempt")
            level = RiskLevel.HIGH

        # Profanity/obscene language: high risk — owner should decide how to respond.
        if _contains_any(text, self._PROFANITY_RU + self._PROFANITY_EN):
            reasons.append("profanity_or_obscene_language")
            level = _max_risk(level, RiskLevel.HIGH)

        # Sexual content: critical — must be handled manually by owner.
        if _contains_any(text, self._SEXUAL):
            reasons.append("sexual_content")
            level = _max_risk(level, RiskLevel.CRITICAL)

        if _contains_any(text, ("password", "passcode", "2fa", "verification code", "card number", "ssn")):
            reasons.append("sensitive_credentials_or_financial_data")
            level = _max_risk(level, RiskLevel.CRITICAL)

        if _contains_any(text, ("wire transfer", "bank account", "refund", "invoice", "payment", "crypto")):
            reasons.append("financial_commitment")
            level = _max_risk(level, RiskLevel.HIGH)

        if _contains_any(text, ("contract", "lawsuit", "legal", "signature", "nda", "liability")):
            reasons.append("legal_or_contractual_topic")
            level = _max_risk(level, RiskLevel.HIGH)

        if _contains_any(text, ("urgent", "asap", "deadline", "complaint", "angry", "escalate")):
            reasons.append("time_sensitive_or_escalation")
            level = _max_risk(level, RiskLevel.MEDIUM)

        if not reasons:
            reasons.append("routine_message")

        return RiskResponse(risk_level=level, reasons=tuple(reasons))


class IntentClassifier:
    """Classify user-message intent using deterministic MVP rules."""

    def classify(self, message_text: str) -> IntentResponse:
        """Return a best-effort intent label and confidence."""
        text = message_text.lower()
        if "?" in message_text or _contains_any(text, ("what", "when", "where", "how", "can you", "could you")):
            return IntentResponse(intent="question", confidence=0.86)
        if _contains_any(text, ("meeting", "call", "schedule", "tomorrow", "today", "calendar")):
            return IntentResponse(intent="scheduling", confidence=0.88)
        if _contains_any(text, ("price", "quote", "invoice", "payment", "cost", "refund")):
            return IntentResponse(intent="commercial", confidence=0.84)
        if _contains_any(text, ("thanks", "thank you", "ok", "great", "sounds good")):
            return IntentResponse(intent="acknowledgement", confidence=0.82)
        if _contains_any(text, ("please", "send", "share", "need", "make", "prepare")):
            return IntentResponse(intent="request", confidence=0.8)
        return IntentResponse(intent="general_message", confidence=0.72)


class MemoryExtractor:
    """Extract durable memory candidates without persisting private data."""

    def extract(self, message_text: str, privacy_flags: tuple[PrivacyFlag, ...]) -> MemoryExtractionResponse:
        """Return memory candidates unless `no_memory` blocks extraction."""
        if PrivacyFlag.NO_MEMORY in privacy_flags:
            return MemoryExtractionResponse(memories=(), blocked_by_privacy=True)

        candidates: list[str] = []
        for pattern in (
            r"\bmy name is ([A-ZА-ЯЁ][\wА-Яа-яё-]+)",
            r"\bi prefer ([^.!\n]+)",
            r"\bremember that ([^.!\n]+)",
            r"\bmy email is ([^\s]+@[^\s]+)",
        ):
            for match in re.finditer(pattern, message_text, flags=re.IGNORECASE):
                candidates.append(match.group(0).strip())

        return MemoryExtractionResponse(memories=tuple(dict.fromkeys(candidates)), blocked_by_privacy=False)


def _contains_any(text: str, needles: tuple[str, ...]) -> bool:
    return any(needle in text for needle in needles)


def _max_risk(left: RiskLevel, right: RiskLevel) -> RiskLevel:
    order = {
        RiskLevel.LOW: 0,
        RiskLevel.MEDIUM: 1,
        RiskLevel.HIGH: 2,
        RiskLevel.CRITICAL: 3,
    }
    return left if order[left] >= order[right] else right
