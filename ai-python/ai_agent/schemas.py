"""Typed schemas and contract enums for the Python AI service.

Enum values (Decision, ChatMode, PrivacyFlag, RiskLevel) are re-exported from
``contract_enums`` which is generated from ``contracts/contracts.json``.
Regenerate via ``scripts/generate_contracts.py`` — do not redefine them here.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import Enum
from typing import Any

from ai_agent.contract_enums import (
    ChatMode,
    Decision,
    LLMPurpose,
    PrivacyFlag,
    RiskLevel,
)

__all__ = ["ChatMode", "Decision", "LLMPurpose", "PrivacyFlag", "RiskLevel"]


class ModelTask(str, Enum):
    """Supported model-router task classes."""

    DRAFT_REPLY = "draft_reply"
    SUMMARIZE_CHAT = "summarize_chat"
    EXTRACT_FACTS = "extract_facts"


class JsonDataclass:
    """Mixin for serializing dataclass schemas to JSON-compatible dictionaries."""

    def to_dict(self) -> dict[str, Any]:
        """Return a JSON-compatible dictionary using enum values."""
        return _json_ready(asdict(self))


@dataclass(frozen=True)
class Usage(JsonDataclass):
    """Token accounting returned by the model gateway."""

    input_tokens: int = 0
    output_tokens: int = 0


@dataclass(frozen=True)
class NormalizedTelegramEvent(JsonDataclass):
    """Normalized Telegram message event from the Go backend."""

    event_id: str
    tenant_id: str
    chat_id: str
    telegram_update_id: int
    business_connection_id: str
    source_message_id: str
    sender_id: str
    message_text: str
    message_type: str
    created_at: str


@dataclass(frozen=True)
class ChatSettings(JsonDataclass):
    """Chat-level autonomy and privacy settings."""

    tenant_id: str
    chat_id: str
    mode: ChatMode
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)


@dataclass(frozen=True)
class FeedbackItem(JsonDataclass):
    """One owner feedback entry on a previous draft."""

    rating: str  # "up" or "down"
    response_text: str
    feedback_text: str = ""


@dataclass(frozen=True)
class ContactProfileData(JsonDataclass):
    """Owner-supplied profile describing the contact in a specific chat.

    Covers three concerns the Go ``ContactProfile`` carries:
      * Собеседник — who the contact is (``name``/``gender``/``role``/…).
      * Я с ним — how the OWNER writes to this contact (``owner_style``, ``tone``,
        ``emoji_usage``, ``address_form``).  ``owner_style`` is owner-authored and
        belongs in the OWNER prompt section, not the contact description.
      * Чат — what the conversation is about (``chat_context``, ``do_not_discuss``).
    """

    name: str = ""
    gender: str = ""
    role: str = ""
    relationship_type: str = ""
    # Я с ним — owner's writing style toward this contact (from style_notes /
    # the legacy notes me_style field).  This is the primary basis for the draft.
    owner_style: str = ""
    tone: str = ""
    emoji_usage: str = ""
    address_form: str = ""
    # Чат — conversation framing supplied by the owner.
    chat_context: str = ""
    do_not_discuss: tuple[str, ...] = field(default_factory=tuple)
    # Собеседник/Чат — attributes the Go ContactProfile already sends but the
    # prompt previously dropped on the floor.
    topics: tuple[str, ...] = field(default_factory=tuple)
    language: str = ""
    contact_timezone: str = ""
    preferred_address: str = ""
    # Legacy free-text notes, retained for backward compatibility.  Never rendered
    # raw into the prompt — owner_style / chat_context are derived from it.
    notes: str = ""


@dataclass(frozen=True)
class UserSettingsData(JsonDataclass):
    """Owner identity/preferences (User Layer) used to ground draft generation.

    The Go backend already sends ``user_settings`` on every process-message
    request; these fields were previously parsed away and never reached the prompt.
    """

    display_name: str = ""
    bio: str = ""
    timezone: str = ""
    primary_language: str = ""
    language_for_drafts: str = ""


@dataclass(frozen=True)
class AIProcessMessageRequest(JsonDataclass):
    """Request body for `/ai/process-message`."""

    request_id: str
    event: NormalizedTelegramEvent
    chat_settings: ChatSettings
    memory_context: tuple[str, ...] = field(default_factory=tuple)
    feedback_context: tuple[FeedbackItem, ...] = field(default_factory=tuple)
    contact_profile: ContactProfileData | None = None
    user_settings: UserSettingsData | None = None
    # trace_id is the deterministic W3C trace-id derived from event.event_id.
    # Default "" keeps positional construction backwards-compatible.
    trace_id: str = ""


@dataclass(frozen=True)
class AIProcessMessageResponse(JsonDataclass):
    """Response body for `/ai/process-message`."""

    decision: Decision
    response_text: str
    confidence: float
    risk_level: RiskLevel
    intent: str
    model_used: str
    usage: Usage
    memory_used: tuple[str, ...] = field(default_factory=tuple)
    # Set by the Phase 3 OutputSafetyScanner when toxicity/sensitive is found
    # in the draft but repair could not salvage it.  Bot UI annotates with ⚠.
    flagged: bool = False


@dataclass(frozen=True)
class AuditEvent(JsonDataclass):
    """Audit event describing a policy decision."""

    audit_id: str
    tenant_id: str
    chat_id: str
    source_message_id: str
    decision: Decision
    reason: str
    created_at: str
    trace_id: str = ""


@dataclass(frozen=True)
class DraftRequest(JsonDataclass):
    """Request body for `/ai/generate-draft`."""

    request_id: str
    message_text: str
    memory_context: tuple[str, ...] = field(default_factory=tuple)
    tone: str = "natural"


@dataclass(frozen=True)
class DraftResponse(JsonDataclass):
    """Response body for `/ai/generate-draft`."""

    draft_text: str
    confidence: float
    model_used: str
    usage: Usage


@dataclass(frozen=True)
class RiskRequest(JsonDataclass):
    """Request body for `/ai/classify-risk`."""

    message_text: str


@dataclass(frozen=True)
class RiskResponse(JsonDataclass):
    """Response body for `/ai/classify-risk`."""

    risk_level: RiskLevel
    reasons: tuple[str, ...]


@dataclass(frozen=True)
class IntentRequest(JsonDataclass):
    """Request body for `/ai/classify-intent`."""

    message_text: str


@dataclass(frozen=True)
class IntentResponse(JsonDataclass):
    """Response body for `/ai/classify-intent`."""

    intent: str
    confidence: float


@dataclass(frozen=True)
class MemoryExtractionRequest(JsonDataclass):
    """Request body for `/ai/extract-memory`."""

    message_text: str
    tenant_id: str = ""
    chat_id: str = ""
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)
    trace_id: str = ""


@dataclass(frozen=True)
class MemoryExtractionResponse(JsonDataclass):
    """Response body for `/ai/extract-memory`."""

    memories: tuple[str, ...]
    blocked_by_privacy: bool


@dataclass(frozen=True)
class TypedFact(JsonDataclass):
    """A single typed, atomic fact extracted from a message.

    Mirrors the SemanticFact contract (§5.4): fact_type and subject determine
    the scope routing on the Go side.  scope_kind is a hint; Go applies the
    deterministic routing rules from the design document.
    """

    fact_text: str
    # One of: preference, biographical, commitment, event, relationship, health, taboo.
    fact_type: str
    # One of: contact, owner, relationship, third_party.
    subject: str
    # Routing hint: "chat" or "owner_global".  Go enforces deterministic routing.
    scope_kind: str = "chat"
    confidence: float = 0.75


@dataclass(frozen=True)
class TypedExtractionRequest(JsonDataclass):
    """Request body for `/ai/extract-facts-typed`."""

    message_text: str
    tenant_id: str = ""
    chat_id: str = ""
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)
    trace_id: str = ""
    # Last few messages in the chat (oldest first) for multi-turn fact detection.
    recent_messages: tuple[str, ...] = field(default_factory=tuple)


@dataclass(frozen=True)
class TypedExtractionResponse(JsonDataclass):
    """Response body for `/ai/extract-facts-typed`.

    The diagnostic fields (``model_used``, ``raw_output``, ``llm_facts_count``,
    ``deterministic_facts_count``) let the Go backend persist an ``extraction_log``
    row so the pipeline stops being a black box — previously a returned ``[]`` was
    indistinguishable from a silent failure.  They default to empty so existing
    positional callers/tests keep working.
    """

    facts: tuple[TypedFact, ...]
    blocked_by_privacy: bool
    model_used: str = ""
    # Verbatim LLM output (truncated) — what the model actually returned before
    # normalization, for debugging "why so few facts".
    raw_output: str = ""
    llm_facts_count: int = 0
    deterministic_facts_count: int = 0


@dataclass(frozen=True)
class TopicTaggingRequest(JsonDataclass):
    """Request body for `/ai/tag-topic`.

    The Go backend sends the current message and recent history so the AI can
    produce a short topic label for the active episode.
    """

    message_text: str
    recent_messages: tuple[str, ...] = field(default_factory=tuple)
    tenant_id: str = ""
    chat_id: str = ""


@dataclass(frozen=True)
class TopicTaggingResponse(JsonDataclass):
    """Response body for `/ai/tag-topic`."""

    topic: str
    model_used: str = ""


@dataclass(frozen=True)
class RerankFactsRequest(JsonDataclass):
    """Request body for `/ai/rerank-facts`.

    The Go backend sends a query and a list of candidate facts; the AI returns
    the top-k most relevant fact IDs in order of descending relevance.
    """

    query: str
    facts: tuple[dict, ...]  # [{"id": int, "text": str}, ...]
    top_k: int = 5


@dataclass(frozen=True)
class RerankFactsResponse(JsonDataclass):
    """Response body for `/ai/rerank-facts`."""

    ranked_ids: tuple[int, ...]
    model_used: str = ""


@dataclass(frozen=True)
class EmbedTextsRequest(JsonDataclass):
    """Request body for `/ai/embed-texts`.

    The Go backend sends batches of fact/episode texts to embed.
    Returns float vectors (4096-dim for qwen/qwen3-embedding-8b) per input text.
    """

    texts: tuple[str, ...]
    model: str = "qwen/qwen3-embedding-8b"
    trace_id: str = ""


@dataclass(frozen=True)
class EmbedTextsResponse(JsonDataclass):
    """Response body for `/ai/embed-texts`.

    embeddings[i] corresponds to texts[i]; vectors are 4096-dim float lists.
    """

    embeddings: tuple[tuple[float, ...], ...]
    model_used: str


@dataclass(frozen=True)
class ChatSummaryRequest(JsonDataclass):
    """Request body for `/ai/summarize-chat`."""

    messages: tuple[str, ...] = field(default_factory=tuple)
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)
    trace_id: str = ""


@dataclass(frozen=True)
class ChatSummaryResponse(JsonDataclass):
    """Response body for `/ai/summarize-chat`."""

    summary: str
    action_items: tuple[str, ...]
    model_used: str
    usage: Usage
    blocked_by_privacy: bool = False


@dataclass(frozen=True)
class GatewayRequest(JsonDataclass):
    """Model gateway request built by the model router."""

    task: ModelTask
    model_name: str
    system_prompt: str
    user_payload: str
    metadata: dict[str, Any] = field(default_factory=dict)
    trace_id: str = ""


@dataclass(frozen=True)
class GatewayResponse(JsonDataclass):
    """Model gateway response returned to the model router."""

    text: str
    model_used: str
    usage: Usage
    # cost_usd is parsed from the LiteLLM ``x-litellm-response-cost`` header
    # (Phase 2).  Defaults to 0.0 when the legacy direct provider path is used.
    cost_usd: float = 0.0


def parse_process_message_request(payload: dict[str, Any]) -> AIProcessMessageRequest:
    """Parse a process-message JSON object into a typed request."""
    event = payload.get("event") or {}
    settings = payload.get("chat_settings") or {}
    memory_context = payload.get("memory_context") or []
    return AIProcessMessageRequest(
        request_id=str(payload["request_id"]),
        trace_id=str(payload.get("trace_id", "")),
        event=NormalizedTelegramEvent(
            event_id=str(event["event_id"]),
            tenant_id=str(event["tenant_id"]),
            chat_id=str(event["chat_id"]),
            telegram_update_id=int(event["telegram_update_id"]),
            business_connection_id=str(event["business_connection_id"]),
            source_message_id=str(event["source_message_id"]),
            sender_id=str(event["sender_id"]),
            message_text=str(event["message_text"]),
            message_type=str(event.get("message_type", "text")),
            created_at=str(event["created_at"]),
        ),
        chat_settings=ChatSettings(
            tenant_id=str(settings["tenant_id"]),
            chat_id=str(settings["chat_id"]),
            mode=ChatMode(str(settings["mode"])),
            privacy_flags=_parse_privacy_flags(settings.get("privacy_flags", [])),
        ),
        memory_context=tuple(str(item) for item in memory_context),
        feedback_context=_parse_feedback_context(payload.get("feedback_context", [])),
        contact_profile=_parse_contact_profile(payload.get("contact_profile")),
        user_settings=_parse_user_settings(payload.get("user_settings")),
    )


def parse_draft_request(payload: dict[str, Any]) -> DraftRequest:
    """Parse a generate-draft JSON object into a typed request."""
    memory_context = payload.get("memory_context") or []
    return DraftRequest(
        request_id=str(payload.get("request_id", "")),
        message_text=str(payload["message_text"]),
        memory_context=tuple(str(item) for item in memory_context),
        tone=str(payload.get("tone", "natural")),
    )


def parse_risk_request(payload: dict[str, Any]) -> RiskRequest:
    """Parse a classify-risk JSON object into a typed request."""
    return RiskRequest(message_text=str(payload["message_text"]))


def parse_intent_request(payload: dict[str, Any]) -> IntentRequest:
    """Parse a classify-intent JSON object into a typed request."""
    return IntentRequest(message_text=str(payload["message_text"]))


def parse_memory_request(payload: dict[str, Any]) -> MemoryExtractionRequest:
    """Parse an extract-memory JSON object into a typed request."""
    return MemoryExtractionRequest(
        message_text=str(payload["message_text"]),
        tenant_id=str(payload.get("tenant_id", "")),
        chat_id=str(payload.get("chat_id", "")),
        privacy_flags=_parse_privacy_flags(payload.get("privacy_flags", [])),
        trace_id=str(payload.get("trace_id", "")),
    )


def parse_embed_texts_request(payload: dict[str, Any]) -> EmbedTextsRequest:
    """Parse an embed-texts JSON object into a typed request."""
    texts = payload.get("texts") or []
    return EmbedTextsRequest(
        texts=tuple(str(t) for t in texts),
        model=str(payload.get("model", "qwen/qwen3-embedding-8b")),
        trace_id=str(payload.get("trace_id", "")),
    )


def parse_typed_extraction_request(payload: dict[str, Any]) -> TypedExtractionRequest:
    """Parse an extract-facts-typed JSON object into a typed request."""
    recent_raw = payload.get("recent_messages") or []
    return TypedExtractionRequest(
        message_text=str(payload["message_text"]),
        tenant_id=str(payload.get("tenant_id", "")),
        chat_id=str(payload.get("chat_id", "")),
        privacy_flags=_parse_privacy_flags(payload.get("privacy_flags", [])),
        trace_id=str(payload.get("trace_id", "")),
        recent_messages=tuple(str(m) for m in recent_raw if m),
    )


def parse_topic_tagging_request(payload: dict[str, Any]) -> TopicTaggingRequest:
    """Parse a tag-topic JSON object into a typed request."""
    recent_raw = payload.get("recent_messages") or []
    return TopicTaggingRequest(
        message_text=str(payload.get("message_text", "")),
        recent_messages=tuple(str(m) for m in recent_raw if m),
        tenant_id=str(payload.get("tenant_id", "")),
        chat_id=str(payload.get("chat_id", "")),
    )


def parse_rerank_facts_request(payload: dict[str, Any]) -> RerankFactsRequest:
    """Parse a rerank-facts JSON object into a typed request."""
    raw_facts = payload.get("facts") or []
    facts = tuple(
        {"id": int(f["id"]), "text": str(f["text"])}
        for f in raw_facts
        if isinstance(f, dict) and "id" in f and "text" in f
    )
    return RerankFactsRequest(
        query=str(payload.get("query", "")),
        facts=facts,
        top_k=int(payload.get("top_k", 5)),
    )


def parse_summary_request(payload: dict[str, Any]) -> ChatSummaryRequest:
    """Parse a summarize-chat JSON object into a typed request."""
    raw_messages = payload.get("messages") or []
    if isinstance(raw_messages, str):
        messages = (raw_messages,)
    else:
        messages = tuple(str(message) for message in raw_messages)
    return ChatSummaryRequest(
        messages=messages,
        privacy_flags=_parse_privacy_flags(payload.get("privacy_flags", [])),
        trace_id=str(payload.get("trace_id", "")),
    )


def _parse_contact_profile(raw: Any) -> ContactProfileData | None:
    """Parse an optional contact_profile dict into a typed dataclass.

    The Go backend sends both structured style fields (``style_notes``, ``tone``,
    ``emoji_usage``, ``address_form``, ``chat_context``, ``do_not_discuss``) and a
    legacy ``notes`` JSON string ``{"me_style", "chat_context"}`` used by the Mini
    App.  Structured fields win; the ``notes`` JSON is parsed as a fallback so chats
    whose profile predates the structured columns still surface owner style.
    """
    if not isinstance(raw, dict):
        return None
    legacy = _parse_legacy_notes(raw.get("notes", ""))
    owner_style = str(raw.get("style_notes", "")).strip() or legacy.get("me_style", "")
    chat_context = str(raw.get("chat_context", "")).strip() or legacy.get("chat_context", "")
    return ContactProfileData(
        name=str(raw.get("name", "")),
        gender=str(raw.get("gender", "")),
        role=str(raw.get("role", "")),
        relationship_type=str(raw.get("relationship_type", "")),
        owner_style=owner_style,
        tone=str(raw.get("tone", "")),
        emoji_usage=str(raw.get("emoji_usage", "")),
        address_form=str(raw.get("address_form", "")),
        chat_context=chat_context,
        do_not_discuss=_parse_str_tuple(raw.get("do_not_discuss")),
        topics=_parse_str_tuple(raw.get("topics")),
        language=str(raw.get("language", "")),
        contact_timezone=str(raw.get("contact_timezone", "")),
        preferred_address=str(raw.get("preferred_address", "")),
        notes=str(raw.get("notes", "")),
    )


def _parse_user_settings(raw: Any) -> UserSettingsData | None:
    """Parse an optional user_settings dict into a typed dataclass."""
    if not isinstance(raw, dict):
        return None
    return UserSettingsData(
        display_name=str(raw.get("display_name", "")),
        bio=str(raw.get("bio", "")),
        timezone=str(raw.get("timezone", "")),
        primary_language=str(raw.get("primary_language", "")),
        language_for_drafts=str(raw.get("language_for_drafts", "")),
    )


def _parse_legacy_notes(notes: Any) -> dict[str, str]:
    """Parse the legacy ``notes`` JSON string into a flat dict, tolerating garbage."""
    if not isinstance(notes, str) or not notes.strip():
        return {}
    try:
        parsed = json.loads(notes)
    except (json.JSONDecodeError, ValueError):
        return {}
    if not isinstance(parsed, dict):
        return {}
    return {str(k): str(v) for k, v in parsed.items() if isinstance(v, str)}


def _parse_str_tuple(value: Any) -> tuple[str, ...]:
    """Coerce a JSON list of strings into a tuple, dropping non-strings/blanks."""
    if not isinstance(value, list):
        return ()
    return tuple(str(v).strip() for v in value if isinstance(v, str) and str(v).strip())


def _parse_feedback_context(values: Any) -> tuple[FeedbackItem, ...]:
    if not isinstance(values, list):
        return ()
    items = []
    for item in values:
        if isinstance(item, dict):
            items.append(FeedbackItem(
                rating=str(item.get("rating", "up")),
                response_text=str(item.get("response_text", "")),
                feedback_text=str(item.get("feedback_text", "")),
            ))
    return tuple(items)


def _parse_privacy_flags(values: Any) -> tuple[PrivacyFlag, ...]:
    if not isinstance(values, list):
        return ()
    return tuple(PrivacyFlag(str(value)) for value in values)


def _json_ready(value: Any) -> Any:
    if isinstance(value, Enum):
        return value.value
    if isinstance(value, tuple):
        return [_json_ready(item) for item in value]
    if isinstance(value, list):
        return [_json_ready(item) for item in value]
    if isinstance(value, dict):
        return {str(key): _json_ready(item) for key, item in value.items()}
    return value
