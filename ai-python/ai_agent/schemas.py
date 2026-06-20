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
class AIRoleCustomFieldData(JsonDataclass):
    """Owner-defined extra role attribute (title + why + optional context).

    Lets owners extend a role beyond the fixed fields; each is injected into the
    persona prompt.  ``title`` and ``description`` are required on the way in;
    ``context`` is optional reference text for that field.
    """

    title: str = ""
    description: str = ""
    context: str = ""


@dataclass(frozen=True)
class MCPServerSpecData(JsonDataclass):
    """One MCP server a role may call, with its per-role tool allow-list (#7).

    ``allowed_tools`` empty means every tool the server exposes is allowed; a
    non-empty tuple restricts the role to exactly those tools (deny-by-default).
    ``auth_header`` is an optional Authorization header value for the server.
    """

    name: str = ""
    transport: str = "http"  # "http" (streamable) or "sse"
    url: str = ""
    auth_header: str = ""
    allowed_tools: tuple[str, ...] = field(default_factory=tuple)


@dataclass(frozen=True)
class AIRoleData(JsonDataclass):
    """Owner-authored AI persona assigned to the chat.

    The pivot from "digital copy of the owner": the AI reproduces the role the
    owner described ("мой менеджер", "техподдержка", any invented character),
    answering on the owner's behalf in that persona.  ``description`` is the
    primary prompt material; the rest are optional refinements.
    """

    name: str = ""
    description: str = ""
    speaking_style: str = ""
    goals: str = ""
    constraints: str = ""
    # Owner-supplied reference material (FAQ, pricing, docs) injected into the
    # draft prompt so the persona answers with that knowledge.
    context: str = ""
    # Owner-defined extra fields beyond the fixed ones above.
    custom_fields: tuple[AIRoleCustomFieldData, ...] = field(default_factory=tuple)
    # MCP servers this role may call (#7); empty means no external tools.
    mcp_servers: tuple[MCPServerSpecData, ...] = field(default_factory=tuple)
    # speak_as_owner=True (default): AI impersonates the owner — contact thinks
    # they are talking to a human.  False: AI responds as the role persona
    # itself (assistant / companion); no owner impersonation.
    speak_as_owner: bool = True
    # model_tier selects the LLM quality tier for draft generation:
    # "fast", "balanced" (default), or "smart".  Empty / unrecognised value
    # degrades to the DRAFT task model (MODEL_DRAFT / OPENROUTER_MODEL).
    model_tier: str = "balanced"


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
    # ai_role is the persona assigned to this chat; None means the default
    # assistant persona (reply in the owner's observed style).
    ai_role: AIRoleData | None = None
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
    text_format: str = "plain"
    memory_used: tuple[str, ...] = field(default_factory=tuple)
    # Set by the Phase 3 OutputSafetyScanner when toxicity/sensitive is found
    # in the draft but repair could not salvage it.  Bot UI annotates with ⚠.
    flagged: bool = False
    # Owner-facing service note (e.g. the critical-risk warning).  It must be
    # rendered on the owner's card only and is NEVER sendable to the contact —
    # response_text stays empty when this is set (ARCH-1).
    owner_note: str = ""
    # Suggested short replies when intent == "choice_question" (ARCH-4).
    options: tuple[str, ...] = field(default_factory=tuple)
    # 3-7 word conversation topic produced by the same draft call; replaces the
    # separate /ai/tag-topic call in the hot path (ARCH-4).
    topic: str = ""


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

    Mirrors the SemanticFact contract.  Memory is strictly per-chat, so
    ``scope_kind`` is always "chat"; the field stays on the wire for
    backwards compatibility and the Go backend enforces the invariant.
    """

    fact_text: str
    # One of: preference, biographical, commitment, event, relationship, health, taboo.
    fact_type: str
    # One of: contact, owner, relationship, third_party.
    subject: str
    # Always "chat" — per-chat memory invariant (see module docstring).
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
class ProfileSuggestion(JsonDataclass):
    """An AI-proposed update to one contact-profile field (§4.5, MEM-4).

    Suggestions are never applied directly: the Go backend stages them in
    ``contact_profile_suggestions`` and the owner accepts or rejects each one.
    """

    field_name: str
    suggested_value: str
    rationale: str = ""


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
    # Identity attributes of the contact proposed for the profile (§4.5, MEM-4);
    # staged via suggestions, never written to the profile directly.
    profile_suggestions: tuple[ProfileSuggestion, ...] = field(default_factory=tuple)


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
    # Proactive owner-facing suggestions derived from the conversation; the Go
    # contract has carried this field since v4 but Python never filled it (ARCH-10).
    recommendations: tuple[str, ...] = field(default_factory=tuple)


@dataclass(frozen=True)
class SynthesizePatternRequest(JsonDataclass):
    """Request body for `/ai/synthesize-pattern` (sleep-time reflection).

    ``cluster_facts`` is one cluster of semantically-related facts the backend
    found; the service returns a single durable behavioural pattern (or empty).
    """

    cluster_facts: tuple[str, ...] = field(default_factory=tuple)
    tenant_id: str = ""
    chat_id: str = ""
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)
    trace_id: str = ""


@dataclass(frozen=True)
class SynthesizePatternResponse(JsonDataclass):
    """Response body for `/ai/synthesize-pattern`.

    ``pattern`` is empty when the facts do not form a durable pattern — the
    backend then writes nothing.
    """

    pattern: str
    subject: str
    confidence: float
    model_used: str
    usage: Usage
    blocked_by_privacy: bool = False


# Media kinds the extract-media endpoint understands. Mirrors the Go
# MediaDescriptor.Kind values; anything else routes to "unsupported".
MEDIA_KINDS = ("voice", "audio", "photo", "document")


@dataclass(frozen=True)
class ExtractMediaRequest(JsonDataclass):
    """Request body for `/ai/extract-media`.

    The Go transport layer downloads a Telegram attachment (it owns the bot
    token) and posts the raw bytes here as base64. The AI service — which already
    holds the provider credentials — turns the media into plain text BEFORE the
    frozen `/ai/process-message` call, keeping the agent text-only. ``kind`` is
    one of :data:`MEDIA_KINDS`; ``duration`` is voice/audio seconds (0 otherwise).
    """

    kind: str
    data_base64: str
    mime_type: str = ""
    duration: int = 0
    trace_id: str = ""


@dataclass(frozen=True)
class ExtractMediaResponse(JsonDataclass):
    """Response body for `/ai/extract-media`.

    ``text`` is the extracted plain text (voice transcript / image OCR+caption /
    document text), empty when nothing could be extracted. ``provenance`` labels
    how it was produced so the caller wraps it with the right untrusted-input
    marker and the model never mistakes transcribed user content for instructions.
    """

    text: str
    provenance: str  # voice_transcript|image_caption|document_text|unsupported|error
    model_used: str


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
        ai_role=_parse_ai_role(payload.get("ai_role")),
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


def parse_synthesize_pattern_request(payload: dict[str, Any]) -> SynthesizePatternRequest:
    """Parse a synthesize-pattern JSON object into a typed request."""
    raw_facts = payload.get("cluster_facts") or []
    if isinstance(raw_facts, str):
        cluster_facts = (raw_facts,)
    else:
        cluster_facts = tuple(str(fact) for fact in raw_facts)
    return SynthesizePatternRequest(
        cluster_facts=cluster_facts,
        tenant_id=str(payload.get("tenant_id", "")),
        chat_id=str(payload.get("chat_id", "")),
        privacy_flags=_parse_privacy_flags(payload.get("privacy_flags", [])),
        trace_id=str(payload.get("trace_id", "")),
    )


def parse_extract_media_request(payload: dict[str, Any]) -> ExtractMediaRequest:
    """Parse an extract-media JSON object into a typed request.

    Validates the boundary: ``kind`` and ``data_base64`` are required (untrusted
    input from the transport layer). An unknown ``kind`` is allowed through so a
    new Telegram media type degrades to "unsupported" rather than a 400.
    """
    kind = str(payload.get("kind", "")).strip()
    data_base64 = str(payload.get("data_base64", ""))
    if not kind:
        raise ValueError("kind is required")
    if not data_base64:
        raise ValueError("data_base64 is required")
    return ExtractMediaRequest(
        kind=kind,
        data_base64=data_base64,
        mime_type=str(payload.get("mime_type", "")),
        duration=int(payload.get("duration", 0) or 0),
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


def _parse_ai_role(raw: Any) -> AIRoleData | None:
    """Parse an optional ai_role dict; a role without a name is treated as absent."""
    if not isinstance(raw, dict):
        return None
    name = str(raw.get("name", "")).strip()
    if not name:
        return None
    # speak_as_owner defaults to True when absent (backward compat with existing
    # roles that predate migration 111).
    speak_as_owner = raw.get("speak_as_owner")
    if not isinstance(speak_as_owner, bool):
        speak_as_owner = True
    # model_tier: accept only the three known values; anything else degrades to
    # "balanced" so existing roles without the field keep working.
    raw_tier = str(raw.get("model_tier", "")).strip().lower()
    model_tier = raw_tier if raw_tier in {"fast", "balanced", "smart"} else "balanced"
    return AIRoleData(
        name=name,
        description=str(raw.get("description", "")).strip(),
        speaking_style=str(raw.get("speaking_style", "")).strip(),
        goals=str(raw.get("goals", "")).strip(),
        constraints=str(raw.get("constraints", "")).strip(),
        context=str(raw.get("context", "")).strip(),
        custom_fields=_parse_role_custom_fields(raw.get("custom_fields")),
        mcp_servers=_parse_mcp_servers(raw.get("mcp_servers")),
        speak_as_owner=speak_as_owner,
        model_tier=model_tier,
    )


def _parse_mcp_servers(raw: Any) -> tuple[MCPServerSpecData, ...]:
    """Parse the optional mcp_servers list; entries without a url are dropped."""
    if not isinstance(raw, list):
        return ()
    servers: list[MCPServerSpecData] = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        url = str(item.get("url", "")).strip()
        if not url:
            continue
        transport = str(item.get("transport", "http")).strip().lower()
        if transport not in ("http", "sse"):
            transport = "http"
        allowed = item.get("allowed_tools")
        allowed_tools = tuple(str(t) for t in allowed) if isinstance(allowed, list) else ()
        servers.append(
            MCPServerSpecData(
                name=str(item.get("name", "")).strip(),
                transport=transport,
                url=url,
                auth_header=str(item.get("auth_header", "")).strip(),
                allowed_tools=allowed_tools,
            )
        )
    return tuple(servers)


def _parse_role_custom_fields(raw: Any) -> tuple[AIRoleCustomFieldData, ...]:
    """Parse the optional custom_fields list; fields without a title are dropped."""
    if not isinstance(raw, list):
        return ()
    fields: list[AIRoleCustomFieldData] = []
    for item in raw:
        if not isinstance(item, dict):
            continue
        title = str(item.get("title", "")).strip()
        if not title:
            continue
        fields.append(
            AIRoleCustomFieldData(
                title=title,
                description=str(item.get("description", "")).strip(),
                context=str(item.get("context", "")).strip(),
            )
        )
    return tuple(fields)


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
