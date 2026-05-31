"""Typed fact extraction pipeline.

Combines the deterministic regex extractor with an LLM-driven typed extractor.
LLM facts win on collisions; deterministic facts cover gaps when the LLM fails
or returns an empty list.  Called asynchronously by the Go backend after the
draft is dispatched (see backlog §7.1).
"""

from __future__ import annotations

import json
import logging

from ..classifiers import TypedMemoryExtractor
from ..llm import ModelRouter
from ..schemas import (
    ModelTask,
    PrivacyFlag,
    TypedExtractionRequest,
    TypedExtractionResponse,
    TypedFact,
)
from ..security import PromptBuilder


logger = logging.getLogger(__name__)


# Allowed values for typed-fact fields; anything outside is normalized to a
# safe default so a hallucinated LLM output cannot widen the contract.
_VALID_FACT_TYPES = frozenset(
    {"preference", "biographical", "commitment", "event", "relationship", "health", "taboo"}
)
_VALID_SUBJECTS = frozenset({"contact", "owner", "relationship", "third_party"})
_VALID_SCOPES = frozenset({"chat", "owner_global"})


def extract_facts_typed(
    typed_extractor: TypedMemoryExtractor,
    prompt_builder: PromptBuilder,
    router: ModelRouter,
    request: TypedExtractionRequest,
) -> TypedExtractionResponse:
    """Extract typed, scoped facts from ``request.message_text``.

    Returns a response whose facts carry ``fact_type``/``subject``/``scope_kind``
    so the Go backend can route them without hard-coded defaults.
    """
    if PrivacyFlag.NO_MEMORY in request.privacy_flags:
        return TypedExtractionResponse(facts=(), blocked_by_privacy=True)

    deterministic = typed_extractor.extract(request.message_text, request.privacy_flags)
    llm_facts, raw_output, model_used = _extract_facts_llm_typed(
        prompt_builder, router, request.message_text, request.privacy_flags
    )

    merged = list(llm_facts)
    llm_texts = {f.fact_text.lower().strip() for f in llm_facts}
    for f in deterministic.facts:
        if f.fact_text.lower().strip() not in llm_texts:
            merged.append(f)

    return TypedExtractionResponse(
        facts=tuple(merged),
        blocked_by_privacy=False,
        model_used=model_used,
        raw_output=raw_output[:2000],
        llm_facts_count=len(llm_facts),
        deterministic_facts_count=len(deterministic.facts),
    )


def _extract_facts_llm_typed(
    prompt_builder: PromptBuilder,
    router: ModelRouter,
    message_text: str,
    privacy_flags: tuple[PrivacyFlag, ...],
) -> tuple[tuple[TypedFact, ...], str, str]:
    """Ask the LLM for typed facts.

    Returns ``(facts, raw_output, model_used)``.  ``facts`` is empty on failure
    or when memory is off; ``raw_output``/``model_used`` are surfaced for the
    extraction-log diagnostics so a returned ``[]`` is distinguishable from a
    silent error.
    """
    if PrivacyFlag.NO_MEMORY in privacy_flags or not message_text.strip():
        return (), "", ""
    system_prompt, user_payload = prompt_builder.build(
        (
            "Extract ONLY durable, long-lived facts from the message below — stable "
            "traits that will still be true weeks from now (profession, place of "
            "residence/study, family, long-standing preferences, commitments with a "
            "clear deadline). "
            "Do NOT extract transient or in-the-moment statements (what someone is "
            "doing right now, today, this moment, current mood, one-off small talk). "
            "For example 'sitting in class right now' is transient — skip it; "
            "'studies medicine at university X' is durable — keep it. "
            "Output a JSON array of objects, each with keys: "
            "fact_text (string, ≤120 chars), "
            "fact_type (one of: preference, biographical, commitment, event, relationship, health, taboo), "
            "subject (one of: contact, owner, relationship, third_party), "
            "scope_kind (one of: chat, owner_global), "
            "confidence (float 0–1; use ≥0.8 only when the message states the fact "
            "explicitly and unambiguously). "
            "health and biographical facts about the owner should use scope_kind=owner_global. "
            "Return [] if there are no durable facts. "
            "Output only the JSON array, no prose."
        ),
        message_text,
        (),
    )
    raw = ""
    model_used = ""
    try:
        response = router.complete(ModelTask.EXTRACT_FACTS, system_prompt, user_payload)
        model_used = response.model_used
        raw = response.text.strip()
        if not raw:
            return (), raw, model_used
        items = json.loads(raw)
        if not isinstance(items, list):
            return (), raw, model_used
        facts: list[TypedFact] = []
        for item in items:
            normalized = _normalize_typed_fact(item)
            if normalized is not None:
                facts.append(normalized)
        return tuple(facts), raw, model_used
    except Exception as exc:
        logger.warning(
            "typed extraction LLM failed",
            extra={"error": str(exc), "raw_preview": raw[:500]},
        )
        # Surface the error text as raw_output so the extraction_log captures it.
        return (), f"ERROR: {exc}", model_used


def _normalize_typed_fact(item: object) -> TypedFact | None:
    """Validate a single LLM-output dict and coerce it to :class:`TypedFact`."""
    if not isinstance(item, dict):
        return None
    text = str(item.get("fact_text", "")).strip()
    if not text or len(text) < 5 or len(text) > 120:
        return None
    fact_type = str(item.get("fact_type", "preference"))
    if fact_type not in _VALID_FACT_TYPES:
        fact_type = "preference"
    subject = str(item.get("subject", "contact"))
    if subject not in _VALID_SUBJECTS:
        subject = "contact"
    scope = str(item.get("scope_kind", "chat"))
    if scope not in _VALID_SCOPES:
        scope = "chat"
    # Deterministic scope override: owner health/biographical always global.
    if subject == "owner" and fact_type in ("health", "biographical"):
        scope = "owner_global"
    confidence = float(item.get("confidence", 0.75))
    return TypedFact(
        fact_text=text,
        fact_type=fact_type,
        subject=subject,
        scope_kind=scope,
        confidence=min(max(confidence, 0.0), 1.0),
    )
