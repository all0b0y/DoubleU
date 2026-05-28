"""Deterministic risk, intent, and memory classifiers."""

from __future__ import annotations

import re
from typing import TYPE_CHECKING

from .schemas import (
    IntentResponse,
    MemoryExtractionResponse,
    PrivacyFlag,
    RiskLevel,
    RiskResponse,
    TypedExtractionResponse,
    TypedFact,
)
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


class TypedMemoryExtractor:
    """Extract typed, scoped facts from a message using deterministic rules.

    Each candidate is classified into a fact_type, subject, and scope_kind per
    the design §5.4 routing rules so the Go backend can persist them without
    hard-coded defaults.

    This extractor is intentionally deterministic and fast (no LLM round-trip).
    LLM-powered extraction is handled by the service._extract_facts_llm_typed
    method which is called asynchronously via /ai/extract-facts-typed.
    """

    # Patterns that hint at health/biographical/preference facts about the contact.
    _HEALTH_PATTERNS = re.compile(
        r"\b(аллергия|диабет|гипертония|астма|инсульт|инфаркт|рак|"
        r"allerg|diabet|asthma|cancer|heart disease|chronic)\b",
        re.IGNORECASE,
    )
    _BIOGRAPHICAL_PATTERNS = re.compile(
        r"\b(my name is|меня зовут|зовут меня|my birthday|день рождения|"
        r"я живу в|i live in|i am from|я из|born in|родился|родилась)\b",
        re.IGNORECASE,
    )
    _PREFERENCE_PATTERNS = re.compile(
        r"\b(i prefer|предпочитаю|prefer|нравится|не нравится|"
        r"like|dislike|люблю|не люблю)\b",
        re.IGNORECASE,
    )
    _COMMITMENT_PATTERNS = re.compile(
        r"\b(i will|я сделаю|я пришлю|i'll send|i'll do|пришлю|отправлю|"
        r"обещаю|promise|deadline|дедлайн|к|by)\b",
        re.IGNORECASE,
    )

    def extract(
        self,
        message_text: str,
        privacy_flags: tuple[PrivacyFlag, ...],
    ) -> TypedExtractionResponse:
        """Return typed fact candidates unless `no_memory` blocks extraction."""
        if PrivacyFlag.NO_MEMORY in privacy_flags:
            return TypedExtractionResponse(facts=(), blocked_by_privacy=True)

        facts: list[TypedFact] = []

        # Health facts about the owner are global-scope and need confirmation.
        for match in self._HEALTH_PATTERNS.finditer(message_text):
            sentence = _surrounding_sentence(message_text, match.start())
            facts.append(TypedFact(
                fact_text=sentence,
                fact_type="health",
                subject="owner",
                scope_kind="owner_global",
                confidence=0.70,
            ))

        # Biographical facts about the contact.
        for match in self._BIOGRAPHICAL_PATTERNS.finditer(message_text):
            sentence = _surrounding_sentence(message_text, match.start())
            facts.append(TypedFact(
                fact_text=sentence,
                fact_type="biographical",
                subject="contact",
                scope_kind="chat",
                confidence=0.80,
            ))

        # Preferences of the contact.
        for match in self._PREFERENCE_PATTERNS.finditer(message_text):
            sentence = _surrounding_sentence(message_text, match.start())
            facts.append(TypedFact(
                fact_text=sentence,
                fact_type="preference",
                subject="contact",
                scope_kind="chat",
                confidence=0.75,
            ))

        # Commitments made by the owner.
        for match in self._COMMITMENT_PATTERNS.finditer(message_text):
            sentence = _surrounding_sentence(message_text, match.start())
            facts.append(TypedFact(
                fact_text=sentence,
                fact_type="commitment",
                subject="owner",
                scope_kind="chat",
                confidence=0.72,
            ))

        # Deduplicate by fact_text before returning.
        seen: set[str] = set()
        unique: list[TypedFact] = []
        for f in facts:
            key = f.fact_text.lower().strip()
            if key not in seen:
                seen.add(key)
                unique.append(f)

        return TypedExtractionResponse(facts=tuple(unique), blocked_by_privacy=False)


def _surrounding_sentence(text: str, match_start: int, max_len: int = 120) -> str:
    """Return the sentence containing the match position, trimmed to max_len."""
    # Find sentence boundaries.
    start = max(0, text.rfind(".", 0, match_start) + 1)
    end = text.find(".", match_start)
    if end == -1:
        end = len(text)
    sentence = text[start:end].strip()
    if len(sentence) > max_len:
        sentence = sentence[:max_len].rstrip()
    return sentence if sentence else text[match_start:match_start + max_len].strip()


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
