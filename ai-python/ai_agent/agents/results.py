"""Typed agent outputs.

These pydantic models ARE the output contract of each agent: pydantic-ai
enforces the schema natively (tool-based structured output), which replaces
the hand-written JSON-format prompt rules and the ``_parse_structured_reply``
/ ``strip_code_fences`` salvage parsing of the legacy pipelines.  Validation
that used to live in normalizer functions (intent whitelist, confidence
clamping, fact-type enums, profile-field whitelist) is expressed as field
types so a hallucinated value can never widen the contract.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

# Mirrors the regex classifier intents plus choice_question, which only the
# LLM can produce (ARCH-2/ARCH-4).
Intent = Literal[
    "question",
    "scheduling",
    "commercial",
    "acknowledgement",
    "request",
    "choice_question",
    "general_message",
]


class ResponsePlan(BaseModel):
    """A contact-facing response form prepared by an interaction tool."""

    model_config = ConfigDict(frozen=True)

    shape: Literal["buttons"] = "buttons"
    prompt: str = Field(min_length=1, description="Текст вопроса над inline-кнопками.")
    options: tuple[str, ...] = Field(
        min_length=2,
        max_length=4,
        description="Короткие варианты ответа собеседника.",
    )


class ScheduledJob(BaseModel):
    """A deferred scheduler job prepared by a tool for Go-owned persistence."""

    model_config = ConfigDict(frozen=True)

    kind: Literal["reminder"] = "reminder"
    target: Literal["owner", "contact"]
    fire_at: datetime
    payload: dict[str, Any] = Field(default_factory=dict)
    created_by: Literal["ai"] = "ai"
    idempotency_key: str = Field(min_length=1)


class OrchestratorResult(BaseModel):
    """Tool-stage output consumed by the final writer, never by the contact."""

    model_config = ConfigDict(frozen=True)

    gathered_context: tuple[str, ...] = Field(
        default=(),
        description="Собранные инструментами факты и результаты, по одному пункту на строку.",
    )
    suggested_intent: str = Field(
        default="",
        max_length=100,
        description="Предполагаемый тип ответа для писателя.",
    )
    notes: str = Field(
        default="",
        description="Короткие указания писателю о важных ограничениях и пробелах в данных.",
    )
    response_plan: ResponsePlan | None = Field(
        default=None,
        description="Форма ответа, созданная вызванным interaction-инструментом.",
    )


class DraftResult(BaseModel):
    """Structured draft: reply + intent + options + confidence + topic (ARCH-4)."""

    model_config = ConfigDict(frozen=True)

    reply: str = Field(
        min_length=1,
        description="Текст ответа собеседнику от лица назначенной персоны — без пояснений.",
    )
    intent: Intent = Field(
        default="general_message",
        description=(
            "Интент последнего сообщения собеседника. choice_question — ТОЛЬКО если"
            " собеседник предлагает выбор из явных альтернатив (обычно с «или» и вопросом)."
        ),
    )
    options: tuple[str, ...] = Field(
        default=(),
        max_length=4,
        description=(
            "Для intent=choice_question — 2-4 коротких готовых ответа от лица назначенной персоны."
            " Для остальных интентов — пустой список."
        ),
    )
    confidence: float = Field(
        default=0.5,
        ge=0.0,
        le=1.0,
        description=(
            "Самооценка 0..1: насколько reply уместно отправить автоматически без проверки"
            " владельцем (учитывай неоднозначность вопроса и нехватку контекста)."
        ),
    )
    topic: str = Field(
        default="",
        max_length=100,
        description="Тема разговора — короткая именная группа в 3-7 словах.",
    )


class CriticVerdict(BaseModel):
    """Reviewer verdict on a draft before it reaches the owner/contact."""

    model_config = ConfigDict(frozen=True)

    verdict: Literal["approve", "revise"] = Field(
        description="approve — черновик можно отправлять как есть; revise — нужны правки.",
    )
    issues: tuple[str, ...] = Field(
        default=(),
        max_length=5,
        description="Краткий список конкретных проблем черновика (пусто при approve).",
    )
    revised_reply: str = Field(
        default="",
        description="При verdict=revise — исправленный текст ответа; иначе пустая строка.",
    )


class ExtractedFact(BaseModel):
    """One durable, long-lived fact extracted from the conversation."""

    model_config = ConfigDict(frozen=True)

    fact_text: str = Field(min_length=5, max_length=120)
    fact_type: Literal[
        "preference", "biographical", "commitment", "event", "relationship", "health", "taboo"
    ] = "preference"
    subject: Literal["contact", "owner", "relationship", "third_party"] = "contact"
    confidence: float = Field(
        default=0.75,
        ge=0.0,
        le=1.0,
        description="≥0.8 только когда факт назван в сообщении явно и однозначно.",
    )


class ProfileSuggestionResult(BaseModel):
    """Identity attribute of the CONTACT revealed explicitly by the message (MEM-4)."""

    model_config = ConfigDict(frozen=True)

    field_name: Literal["relationship_type", "preferred_address", "language", "tone", "gender"]
    suggested_value: str = Field(min_length=1, max_length=64)
    rationale: str = Field(default="", max_length=120, description="Цитата-доказательство из сообщения.")


class ExtractionResult(BaseModel):
    """Full typed-extraction output: durable facts + contact-profile suggestions."""

    model_config = ConfigDict(frozen=True)

    facts: tuple[ExtractedFact, ...] = ()
    profile_suggestions: tuple[ProfileSuggestionResult, ...] = ()


class RoleDraftCustomField(BaseModel):
    """One owner-editable custom field proposed for an AI role."""

    model_config = ConfigDict(frozen=True)

    title: str = Field(min_length=1, max_length=64)
    description: str = Field(
        min_length=1,
        description="Зачем это поле нужно роли и как оно влияет на ответы.",
    )
    context: str = Field(default="", max_length=5000)


class RoleDraft(BaseModel):
    """A complete owner-editable AI-role draft generated from free text."""

    model_config = ConfigDict(frozen=True)

    name: str = Field(min_length=1, max_length=64)
    description: str = Field(min_length=1)
    speaking_style: str = ""
    goals: str = ""
    constraints: str = ""
    custom_fields: tuple[RoleDraftCustomField, ...] = ()


class SummaryResult(BaseModel):
    """Episode/chat summary with actionable follow-ups (ARCH-10)."""

    model_config = ConfigDict(frozen=True)

    summary: str = Field(min_length=1, description="Краткая сводка переписки на русском.")
    action_items: tuple[str, ...] = Field(
        default=(), description="Конкретные обязательства и задачи, упомянутые в переписке."
    )
    recommendations: tuple[str, ...] = Field(
        default=(), description="Проактивные рекомендации владельцу по этому чату."
    )


class PatternResult(BaseModel):
    """A durable behavioural pattern synthesised from a cluster of facts (#5).

    ``pattern`` is intentionally allowed to be empty: when the facts do not
    support a stable, non-obvious pattern the model must return "" so the
    backend writes nothing rather than inventing an insight.
    """

    model_config = ConfigDict(frozen=True)

    pattern: str = Field(
        default="",
        max_length=200,
        description=(
            "Устойчивый поведенческий паттерн контакта в 1 фразе (например"
            " «регулярно перегружен работой, часто в стрессе»). Пустая строка —"
            " если факты НЕ образуют устойчивого паттерна. Не выдумывай."
        ),
    )
    subject: Literal["contact", "owner", "relationship", "third_party"] = "contact"
    confidence: float = Field(
        default=0.5,
        ge=0.0,
        le=1.0,
        description="Уверенность 0..1 в том, что это устойчивый паттерн, а не разовость.",
    )


class TopicResult(BaseModel):
    """Short topic label for the active conversation episode."""

    model_config = ConfigDict(frozen=True)

    topic: str = Field(
        default="",
        max_length=100,
        description=(
            "Тема разговора — короткая именная группа в 3-7 слов (например"
            " «обсуждение договора», «планирование встречи»). Без кавычек и пояснений."
        ),
    )


class RerankResult(BaseModel):
    """Listwise cross-encoder verdict: candidate fact IDs, most relevant first."""

    model_config = ConfigDict(frozen=True)

    ranked_ids: tuple[int, ...] = Field(
        default=(),
        description=(
            "ID фактов (числа после «ID=») в порядке убывания релевантности к запросу."
            " Верни только реально релевантные; нерелевантные опусти."
        ),
    )
