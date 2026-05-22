"""Typed schemas and contract enums for the Python AI service."""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import Enum
from typing import Any


class Decision(str, Enum):
    """AI decision values shared by the service contract."""

    DRAFT = "draft"
    AUTO_REPLY = "auto_reply"
    IGNORE = "ignore"
    OBSERVE = "observe"
    ASK_OWNER = "ask_owner"
    ERROR_FALLBACK = "error_fallback"


class ChatMode(str, Enum):
    """Chat autonomy modes shared by the service contract."""

    DISABLED = "disabled"
    SILENT = "silent"
    MANUAL = "manual"
    SEMI_AUTO = "semi_auto"
    AUTO = "auto"


class PrivacyFlag(str, Enum):
    """Privacy flags shared by the service contract."""

    NO_MEMORY = "no_memory"
    NO_TRAINING = "no_training"
    PRIVATE = "private"
    NO_SUMMARY = "no_summary"


class RiskLevel(str, Enum):
    """Risk levels shared by the service contract."""

    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"
    CRITICAL = "critical"


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
    """Owner-supplied profile describing the contact in a specific chat."""

    name: str = ""
    gender: str = ""
    role: str = ""
    relationship_type: str = ""
    notes: str = ""


@dataclass(frozen=True)
class AIProcessMessageRequest(JsonDataclass):
    """Request body for `/ai/process-message`."""

    request_id: str
    event: NormalizedTelegramEvent
    chat_settings: ChatSettings
    memory_context: tuple[str, ...] = field(default_factory=tuple)
    feedback_context: tuple[FeedbackItem, ...] = field(default_factory=tuple)
    contact_profile: ContactProfileData | None = None


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


@dataclass(frozen=True)
class MemoryExtractionResponse(JsonDataclass):
    """Response body for `/ai/extract-memory`."""

    memories: tuple[str, ...]
    blocked_by_privacy: bool


@dataclass(frozen=True)
class ChatSummaryRequest(JsonDataclass):
    """Request body for `/ai/summarize-chat`."""

    messages: tuple[str, ...] = field(default_factory=tuple)
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)


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


@dataclass(frozen=True)
class GatewayResponse(JsonDataclass):
    """Model gateway response returned to the model router."""

    text: str
    model_used: str
    usage: Usage


def parse_process_message_request(payload: dict[str, Any]) -> AIProcessMessageRequest:
    """Parse a process-message JSON object into a typed request."""
    event = payload.get("event") or {}
    settings = payload.get("chat_settings") or {}
    memory_context = payload.get("memory_context") or []
    return AIProcessMessageRequest(
        request_id=str(payload["request_id"]),
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
    )


def _parse_contact_profile(raw: Any) -> ContactProfileData | None:
    """Parse an optional contact_profile dict into a typed dataclass."""
    if not isinstance(raw, dict):
        return None
    return ContactProfileData(
        name=str(raw.get("name", "")),
        gender=str(raw.get("gender", "")),
        role=str(raw.get("role", "")),
        relationship_type=str(raw.get("relationship_type", "")),
        notes=str(raw.get("notes", "")),
    )


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
