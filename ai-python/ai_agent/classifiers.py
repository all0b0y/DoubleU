"""Deterministic risk, intent, and memory classifiers."""

from __future__ import annotations

import re

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
    """Classify message risk using deterministic MVP policy rules.

    All keyword matching is word-prefix anchored (``\\b`` + stem) instead of raw
    substring search: bare substrings produced false CRITICAL/HIGH hits on
    innocent Russian words («страх»→«трах», «корабля»→«бля», «анализ»→«анал»),
    which blocked auto-reply and spammed the owner with manual-handling cards.
    """

    # Profanity stems (Russian and English), word-prefix anchored.
    _PROFANITY_RE = re.compile(
        r"\b(?:ху[йеёи]|пизд|ебл|ёбл|еблан|залуп|бля|сука|пидор|пидар|мудак|"
        r"ёб|заеб|наеб|выеб|охуе|ахуе|ебат|ебан|уёб|"
        r"fuck|shit|asshole|bitch|cunt|dick|pussy)",
        re.IGNORECASE,
    )

    # Sexual content stems, word-prefix anchored. Lookaheads keep common
    # innocent words out: «анализ/аналитика/аналог», «куница».
    _SEXUAL_RE = re.compile(
        r"\b(?:секс|трах|отсос|минет|куни(?!ц)|анал(?!из|ит|ог)|эротик|порно|"
        r"гол(?:ый|ая)|раздева|покажи грудь|покажи попу|"
        r"sex|nude|porn|blowjob|erotic|naked|horny)",
        re.IGNORECASE,
    )

    # Credentials / payment-instrument requests (RU + EN): critical.
    _CREDENTIALS_RE = re.compile(
        r"\b(?:password|passcode|2fa|verification code|card number|ssn|cvv|"
        r"парол|пин-?код|код подтверждения|смс-?код|номер карты|секретн\w* слово)",
        re.IGNORECASE,
    )

    # Financial commitments (RU + EN): high.
    _FINANCIAL_RE = re.compile(
        r"\b(?:wire transfer|bank account|refund|invoice|payment|crypto|"
        r"перевод денег|оплат|предоплат|счёт на оплату|счет на оплату|"
        r"возврат денег|крипт)",
        re.IGNORECASE,
    )

    # Legal / contractual topics (RU + EN): high.
    _LEGAL_RE = re.compile(
        r"\b(?:contract|lawsuit|legal|signature|nda|liability|"
        r"договор|контракт|подпис|юрист|претензи|неустойк|расторж)",
        re.IGNORECASE,
    )

    # Urgency / escalation signals (RU + EN): medium.
    _URGENCY_RE = re.compile(
        r"\b(?:urgent|asap|deadline|complaint|angry|escalate|"
        r"срочно|дедлайн|жалоб|эскалац|как можно скорее)",
        re.IGNORECASE,
    )

    def classify(self, message_text: str, message_type: str = "text") -> RiskResponse:
        """Return a risk level and concise reasons for the supplied message.

        ``message_type`` is the inbound attachment kind (text/photo/voice/
        audio/document). It gates the *topical* keyword rules below: a
        ``document`` is reference material the owner forwarded for the agent to
        analyze, so its business/legal/financial vocabulary is descriptive — not
        a risky request directed at the owner — and must not permanently lock
        every document out of auto-reply. Hard-safety signals (prompt injection,
        sexual content, credential/PII leakage) stay in force for all content,
        including document bodies, because those are sensitive regardless of
        whether the text was typed or extracted from an attachment.
        """
        reasons: list[str] = []
        level = RiskLevel.LOW

        # --- Hard-safety signals: always applied, any content source. ---
        if contains_prompt_injection(message_text):
            reasons.append("prompt_injection_attempt")
            level = _max_risk(level, RiskLevel.HIGH)

        # Sexual content: critical — must be handled manually by owner.
        if self._SEXUAL_RE.search(message_text):
            reasons.append("sexual_content")
            level = _max_risk(level, RiskLevel.CRITICAL)

        if self._CREDENTIALS_RE.search(message_text):
            reasons.append("sensitive_credentials_or_financial_data")
            level = _max_risk(level, RiskLevel.CRITICAL)

        # --- Topical signals: a risky *request from the contact*. Skipped for
        # forwarded documents, whose vocabulary is descriptive reference text.
        # Still applied to typed text, captions, voice and image content. ---
        if message_type != "document":
            # Aggression/profanity is a *signal* that the current approach isn't
            # landing — not a blocking risk (owner decision). Tag it so the writer
            # can adjust tone, but never escalate the level: an angry contact still
            # gets an auto-reply in auto/semi rather than being held for the owner.
            if self._PROFANITY_RE.search(message_text):
                reasons.append("aggression_signal")

            if self._FINANCIAL_RE.search(message_text):
                reasons.append("financial_commitment")
                level = _max_risk(level, RiskLevel.HIGH)

            if self._LEGAL_RE.search(message_text):
                reasons.append("legal_or_contractual_topic")
                level = _max_risk(level, RiskLevel.HIGH)

            if self._URGENCY_RE.search(message_text):
                reasons.append("time_sensitive_or_escalation")
                level = _max_risk(level, RiskLevel.MEDIUM)

        if not reasons:
            reasons.append("routine_message")

        return RiskResponse(risk_level=level, reasons=tuple(reasons))


class IntentClassifier:
    """Classify user-message intent using deterministic MVP rules.

    Keyword matching is word-boundary anchored (substring search mislabeled
    «booking»→"ok") and covers Russian alongside English — without the Russian
    keywords every RU message fell to ``general_message`` (0.72), which made
    the auto-reply confidence gate unreachable for the product's main language.
    """

    _QUESTION_RE = re.compile(
        r"\b(?:what|when|where|how|can you|could you|"
        r"что|когда|где|как|почему|сколько|можешь|можете)\b",
        re.IGNORECASE,
    )
    _SCHEDULING_RE = re.compile(
        r"\b(?:meeting|call|schedule|tomorrow|today|calendar|"
        r"встреч|созвон|звонок|завтра|сегодня|расписани|перенес)",
        re.IGNORECASE,
    )
    _COMMERCIAL_RE = re.compile(
        r"\b(?:price|quote|invoice|payment|cost|refund|"
        r"цен[аыу]|стоимост|стоит|оплат|счёт|счет|возврат)",
        re.IGNORECASE,
    )
    _ACK_RE = re.compile(
        r"\b(?:thanks|thank you|ok|okay|great|sounds good|"
        r"спасибо|благодарю|ок|окей|хорошо|отлично|договорились|понял|поняла)\b",
        re.IGNORECASE,
    )
    _REQUEST_RE = re.compile(
        r"\b(?:please|send|share|need|make|prepare|"
        r"пожалуйста|пришли|отправь|скинь|нужно|надо|сделай|подготовь)",
        re.IGNORECASE,
    )

    def classify(self, message_text: str) -> IntentResponse:
        """Return a best-effort intent label and confidence."""
        if "?" in message_text or self._QUESTION_RE.search(message_text):
            return IntentResponse(intent="question", confidence=0.86)
        if self._SCHEDULING_RE.search(message_text):
            return IntentResponse(intent="scheduling", confidence=0.88)
        if self._COMMERCIAL_RE.search(message_text):
            return IntentResponse(intent="commercial", confidence=0.84)
        if self._ACK_RE.search(message_text):
            return IntentResponse(intent="acknowledgement", confidence=0.82)
        if self._REQUEST_RE.search(message_text):
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
    """Extract typed facts from a message using deterministic rules.

    Each candidate is classified into a fact_type and subject.  Memory is
    strictly per-chat, so every fact is chat-scoped; the Go backend enforces
    the same invariant on write.

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
    # Bare prepositions («к», "by") are intentionally excluded: they match
    # nearly every Russian/English sentence and flooded semantic_facts with
    # bogus commitment rows at confidence 0.72.
    _COMMITMENT_PATTERNS = re.compile(
        r"\b(i will|я сделаю|я пришлю|i'll send|i'll do|пришлю|отправлю|"
        r"обещаю|promise|deadline|дедлайн)\b",
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

        # Health facts about the CONTACT: extraction only ever runs on contact
        # messages (owner-authored messages exit the pipeline earlier with
        # owner_message_observed), so first-person health statements belong to
        # the contact (ARCH-11). Chat-scoped and hard-included on retrieval (§8.2).
        for match in self._HEALTH_PATTERNS.finditer(message_text):
            sentence = _surrounding_sentence(message_text, match.start())
            facts.append(TypedFact(
                fact_text=sentence,
                fact_type="health",
                subject="contact",
                scope_kind="chat",
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

        # Commitments made by the CONTACT («я пришлю договор» in a contact
        # message is the contact's obligation, not the owner's). Mislabelling
        # them as owner commitments fed foreign obligations into the prompt as
        # the owner's own via OpenCommitments (ARCH-11).
        for match in self._COMMITMENT_PATTERNS.finditer(message_text):
            sentence = _surrounding_sentence(message_text, match.start())
            facts.append(TypedFact(
                fact_text=sentence,
                fact_type="commitment",
                subject="contact",
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


def _max_risk(left: RiskLevel, right: RiskLevel) -> RiskLevel:
    order = {
        RiskLevel.LOW: 0,
        RiskLevel.MEDIUM: 1,
        RiskLevel.HIGH: 2,
        RiskLevel.CRITICAL: 3,
    }
    return left if order[left] >= order[right] else right
