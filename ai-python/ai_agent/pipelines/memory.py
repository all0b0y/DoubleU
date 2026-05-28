"""Regex-based memory extraction pipeline.

This is the synchronous fast path used in :meth:`AIService.process_message`
to fill ``memory_used``.  The richer LLM-driven extraction lives in
:mod:`ai_agent.pipelines.extraction` and runs asynchronously after the draft
is dispatched (see backlog item §7.1).
"""

from __future__ import annotations

from ..classifiers import MemoryExtractor
from ..schemas import MemoryExtractionRequest, MemoryExtractionResponse


def extract_memory(
    extractor: MemoryExtractor, request: MemoryExtractionRequest
) -> MemoryExtractionResponse:
    """Run :class:`MemoryExtractor` while honoring ``request.privacy_flags``."""
    return extractor.extract(request.message_text, request.privacy_flags)
