"""Per-request LLM call context propagated via ContextVar.

The Phase 0 envelope decision is two-layer: the HTTP boundary keeps its
existing per-purpose request schemas, while *inside* Python a typed dataclass
``LLMCallContext`` lives in a ``contextvars.ContextVar`` carrying the
mutable redactions + safety_results that the pipeline modules read and write.

Pipelines never thread the context as an explicit argument: they call
``current_context()`` where needed.  The HTTP handler binds a fresh context
per request via ``with_context(...)``; nested ``with_context`` calls (e.g.
shadow mode running two gateways) restore the parent on exit.
"""

from __future__ import annotations

from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass, field
from typing import Iterator

from ai_agent.contract_enums import LLMPurpose, PrivacyFlag

from .results import ScanResult


@dataclass
class LLMCallContext:
    """Per-request envelope for one LLM call (or one webhook handling)."""

    trace_id: str = ""
    tenant_id: str = ""
    chat_id: str = ""
    event_id: str = ""
    purpose: LLMPurpose | None = None
    privacy_flags: tuple[PrivacyFlag, ...] = field(default_factory=tuple)
    model_hint: str = ""
    schema_id: str = ""
    # placeholder map produced by InputSafetyScanner.Anonymize and consumed
    # by OutputSafetyScanner.deanonymize.
    redactions: dict[str, str] = field(default_factory=dict)
    safety_results: list[ScanResult] = field(default_factory=list)
    # Set by OutputSafetyScanner when a non-blocking scanner finding (e.g.
    # toxicity in a draft) requires the bot UI to annotate the draft with ⚠.
    output_flagged: bool = False


_empty_context = LLMCallContext()
_ctx_var: ContextVar[LLMCallContext] = ContextVar("llm_call_context", default=_empty_context)


def current_context() -> LLMCallContext:
    """Return the LLMCallContext bound to the current task/thread."""
    return _ctx_var.get()


@contextmanager
def with_context(ctx: LLMCallContext) -> Iterator[LLMCallContext]:
    """Bind ``ctx`` as the current LLMCallContext within the ``with`` block."""
    token = _ctx_var.set(ctx)
    try:
        yield ctx
    finally:
        _ctx_var.reset(token)
