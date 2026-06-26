"""Application service for AI orchestration and policy decisions.

This module owns :class:`AIService` — the thin orchestrator that wires the
deterministic classifiers and the pydantic-ai :class:`AgentRuntime` to the
endpoints.  All LLM-bound work (draft, typed extraction, summarization, topic
tagging, fact reranking) runs through the agent runtime; the deterministic
classifiers (risk/intent/memory) and the policy thresholds stay in-process so
the contract and the backend's DecisionEngine are unchanged.  Embedding
transport is a direct provider call (not an LLM agent) and lives in its own
pipeline module.
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone
from typing import TYPE_CHECKING
from uuid import NAMESPACE_URL, uuid5

logger = logging.getLogger(__name__)

if TYPE_CHECKING:  # pydantic-ai is imported lazily by the HTTP factory
    from .agents.runtime import AgentRuntime

from .classifiers import (
    IntentClassifier,
    MemoryExtractor,
    RiskClassifier,
    TypedMemoryExtractor,
)
from .pipelines import (
    decisions,
    draft,
    embedding,
    intent,
    media,
    memory,
    risk,
)
from .schemas import (
    AIProcessMessageRequest,
    AIProcessMessageResponse,
    AuditEvent,
    ChatSummaryRequest,
    ChatSummaryResponse,
    Decision,
    DraftRoleRequest,
    DraftRoleResponse,
    EmbedTextsRequest,
    EmbedTextsResponse,
    ExtractMediaRequest,
    ExtractMediaResponse,
    IntentRequest,
    IntentResponse,
    MemoryExtractionRequest,
    MemoryExtractionResponse,
    PrivacyFlag,
    RerankFactsRequest,
    RerankFactsResponse,
    RiskRequest,
    RiskResponse,
    SynthesizePatternRequest,
    SynthesizePatternResponse,
    TopicTaggingRequest,
    TopicTaggingResponse,
    TypedExtractionRequest,
    TypedExtractionResponse,
    Usage,
)


class InMemoryAuditSink:
    """Collect audit events produced by the AI service."""

    def __init__(self) -> None:
        """Create an empty audit event sink."""
        self._events: list[AuditEvent] = []

    def record(self, event: AuditEvent) -> None:
        """Store an audit event."""
        self._events.append(event)

    def events(self) -> tuple[AuditEvent, ...]:
        """Return all recorded audit events."""
        return tuple(self._events)


class AIService:
    """Technical MVP AI orchestration service.

    Holds the deterministic classifiers and the agent runtime, and delegates
    the per-endpoint work to them.  No LLM transport, prompt assembly or policy
    thresholds live here directly — generation goes through the agents, policy
    through :mod:`ai_agent.pipelines.decisions`.
    """

    def __init__(
        self,
        agents: "AgentRuntime",
        risk_classifier: RiskClassifier | None = None,
        intent_classifier: IntentClassifier | None = None,
        memory_extractor: MemoryExtractor | None = None,
        typed_extractor: TypedMemoryExtractor | None = None,
        audit_sink: InMemoryAuditSink | None = None,
    ) -> None:
        """Create an AI service backed by the pydantic-ai agent runtime."""
        self._agents = agents
        self._risk_classifier = risk_classifier or RiskClassifier()
        self._intent_classifier = intent_classifier or IntentClassifier()
        self._memory_extractor = memory_extractor or MemoryExtractor()
        self._typed_extractor = typed_extractor or TypedMemoryExtractor()
        self._audit_sink = audit_sink or InMemoryAuditSink()

    def health(self) -> dict[str, str]:
        """Return liveness information for ``/health``."""
        return {"status": "ok"}

    def ready(self, mock_llm: bool) -> dict[str, object]:
        """Return readiness information for ``/ready``."""
        return {"status": "ready" if mock_llm else "not_ready", "mock_llm": mock_llm}

    def audit_events(self) -> tuple[AuditEvent, ...]:
        """Return audit events produced by this service instance."""
        return self._audit_sink.events()

    def classify_risk(self, request: RiskRequest) -> RiskResponse:
        """Classify risk for a user-controlled message."""
        return risk.classify_risk(self._risk_classifier, request)

    def classify_intent(self, request: IntentRequest) -> IntentResponse:
        """Classify intent for a user-controlled message."""
        return intent.classify_intent(self._intent_classifier, request)

    def extract_memory(self, request: MemoryExtractionRequest) -> MemoryExtractionResponse:
        """Extract memory candidates while honoring privacy flags."""
        return memory.extract_memory(self._memory_extractor, request)

    def extract_facts_typed(self, request: TypedExtractionRequest) -> TypedExtractionResponse:
        """Extract typed, scoped facts from a message (deterministic + agent)."""
        return self._agents.extract_facts_typed(self._typed_extractor, request)

    def summarize_chat(self, request: ChatSummaryRequest) -> ChatSummaryResponse:
        """Summarize chat messages while honoring ``no_summary`` privacy."""
        return self._agents.summarize_chat(request)

    def embed_texts(self, request: EmbedTextsRequest) -> EmbedTextsResponse:
        """Generate embeddings for a batch of texts via the embedding pipeline."""
        return embedding.embed_texts(request)

    def extract_media(self, request: ExtractMediaRequest) -> ExtractMediaResponse:
        """Turn a media attachment (voice/audio/photo/document) into plain text.

        A direct provider call (like embedding), not an agent task: keeps the
        agent text-only so the frozen process-message contract is untouched.
        """
        return media.extract_media(request)

    def draft_role(self, request: DraftRoleRequest) -> DraftRoleResponse:
        """Generate an editable AI-role draft from a free-text description."""
        return self._agents.draft_role(request)

    def tag_topic(self, request: TopicTaggingRequest) -> TopicTaggingResponse:
        """Return a short topic label for the active conversation episode."""
        return self._agents.tag_topic(request)

    def rerank_facts(self, request: RerankFactsRequest) -> RerankFactsResponse:
        """Rerank a set of candidate facts by relevance to a query."""
        return self._agents.rerank_facts(request)

    def synthesize_pattern(self, request: SynthesizePatternRequest) -> SynthesizePatternResponse:
        """Synthesise a behaviour pattern from a fact cluster (sleep-time reflection)."""
        return self._agents.synthesize_pattern(request)

    def process_message(self, request: AIProcessMessageRequest) -> AIProcessMessageResponse:
        """Process a normalized Telegram event into an AI decision proposal.

        Risk stays deterministic (charter: policy is backend-owned); intent and
        confidence come from the structured draft output when the agent
        produced it (ARCH-4), with the regex classifier + synthetic confidence
        as the fallback.
        """
        risk_response = self._risk_classifier.classify(request.event.message_text)
        intent_response = self._intent_classifier.classify(request.event.message_text)
        no_memory = PrivacyFlag.NO_MEMORY in request.chat_settings.privacy_flags

        # Synchronous memory extraction in the hot reply path is intentionally limited to
        # the fast regex extractor.  LLM-based extraction is expensive and blocks reply
        # latency; per the design (§7.1) it runs asynchronously after the draft is
        # returned.  IMPORTANT: never include request.memory_context here — that causes
        # the self-poisoning loop described in the Bug Backlog.
        extracted_facts = self._memory_extractor.extract(
            request.event.message_text, request.chat_settings.privacy_flags
        ).memories
        memory_used = () if no_memory else extracted_facts

        try:
            outcome = self._agents.draft_outcome(
                request, risk_response.risk_level, risk_response.reasons
            )
            intent = outcome.intent or intent_response.intent
            if outcome.confidence is not None:
                confidence = decisions.combined_confidence(
                    outcome.confidence, risk_response.risk_level
                )
            else:
                confidence = decisions.confidence_for(
                    risk_response.risk_level, intent_response.confidence
                )
            decision = decisions.decide(
                request.chat_settings.mode,
                request.chat_settings.privacy_flags,
                risk_response.risk_level,
                confidence,
            )
        except RuntimeError as exc:
            logger.exception("draft agent failed — falling back to error_fallback: %s", exc)
            outcome = draft.DraftOutcome(text="", model_used="none", usage=Usage())
            intent = intent_response.intent
            decision = Decision.ERROR_FALLBACK
            confidence = 0.0

        # Output-safety findings (e.g. toxicity in draft) propagate as a
        # non-blocking ``flagged`` signal so the bot UI annotates the draft.
        from .observability.context import current_context
        flagged = current_context().output_flagged
        result = AIProcessMessageResponse(
            decision=decision,
            response_text=outcome.text if decision in (Decision.DRAFT, Decision.AUTO_REPLY) else "",
            confidence=confidence,
            risk_level=risk_response.risk_level,
            intent=intent,
            model_used=outcome.model_used,
            usage=outcome.usage,
            text_format=outcome.text_format,
            memory_used=memory_used,
            flagged=flagged,
            owner_note=outcome.owner_note,
            options=outcome.options,
            response_plan=outcome.response_plan,
            scheduled_jobs=outcome.scheduled_jobs,
            topic=outcome.topic,
        )
        # Label the Langfuse trace with the actual decision so an auto-sent reply
        # is distinguishable from an owner draft (the trace is opened under the
        # generic "draft" generation purpose otherwise).
        self._agents.record_decision(decision.value, intent or "", confidence)
        self._record_audit(request, result, ",".join(risk_response.reasons))
        return result

    def _record_audit(
        self,
        request: AIProcessMessageRequest,
        response: AIProcessMessageResponse,
        reason: str,
    ) -> None:
        audit_id = str(uuid5(NAMESPACE_URL, f"{request.request_id}:{request.event.source_message_id}"))
        # Prefer the trace_id propagated on the request, fall back to the
        # ContextVar set by the HTTP handler, then derive from event.event_id
        # so direct AIService callers (tests, sync orchestrators) still get a
        # stable trace identifier on every audit row.
        trace_id = request.trace_id
        if not trace_id:
            from .observability.context import current_context
            trace_id = current_context().trace_id
        if not trace_id and request.event.event_id:
            from .observability.trace import derive_trace_id
            trace_id = derive_trace_id(request.event.event_id)
        self._audit_sink.record(
            AuditEvent(
                audit_id=audit_id,
                tenant_id=request.event.tenant_id,
                chat_id=request.event.chat_id,
                source_message_id=request.event.source_message_id,
                decision=response.decision,
                reason=reason,
                created_at=datetime.now(timezone.utc).isoformat(),
                trace_id=trace_id,
            )
        )


def build_service(
    agents: "AgentRuntime",
    audit_sink: InMemoryAuditSink | None = None,
) -> AIService:
    """Create an AI service instance backed by the agent runtime."""
    return AIService(agents=agents, audit_sink=audit_sink)
