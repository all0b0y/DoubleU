"""Cross-encoder reranking pipeline.

The Go retrieval path fetches a broad set of candidate facts (top-20 vector +
BM25), then calls this endpoint to reduce them to the top-k most relevant ones.
An LLM acts as a listwise cross-encoder: it sees the query and all candidates
together and returns a ranked list of fact IDs.
"""

from __future__ import annotations

import json
import logging

from ..llm import ModelRouter
from ..schemas import ModelTask, RerankFactsRequest, RerankFactsResponse
from ..security import PromptBuilder

logger = logging.getLogger(__name__)

# Sentinel used when the LLM returns nothing usable — fall back to input order.
_EMPTY: tuple[int, ...] = ()


def rerank_facts(
    router: ModelRouter,
    request: RerankFactsRequest,
) -> RerankFactsResponse:
    """Rerank candidate facts by relevance to ``request.query``.

    Uses the cheap extract model as a listwise cross-encoder.  On any failure
    (LLM error, malformed JSON) returns the input IDs in their original order
    so the retrieval path always has something to work with.
    """
    if not request.facts or not request.query.strip():
        return RerankFactsResponse(ranked_ids=_EMPTY)

    # Build a numbered list of facts for the LLM.
    fact_lines = "\n".join(
        f"{i + 1}. [ID={f['id']}] {f['text']}"
        for i, f in enumerate(request.facts)
    )
    prompt_builder = PromptBuilder()
    system_prompt, user_payload = prompt_builder.build(
        (
            f"You are a relevance ranker. Given the user query and a numbered list of facts, "
            f"select the {request.top_k} most relevant facts and return their IDs "
            f"(the numbers after 'ID=') as a JSON array in order from most to least relevant. "
            f"Return ONLY the JSON array of integers, no prose. "
            f"If fewer than {request.top_k} facts are relevant, return only those that are."
        ),
        f"QUERY: {request.query}\n\nFACTS:\n{fact_lines}",
        (),
    )

    model_used = ""
    try:
        response = router.complete(ModelTask.EXTRACT_FACTS, system_prompt, user_payload)
        model_used = response.model_used
        raw = response.text.strip()
        if not raw:
            return _fallback(request, model_used)
        items = json.loads(raw)
        if not isinstance(items, list):
            return _fallback(request, model_used)
        # Filter to valid IDs that appear in the candidate set.
        valid_ids = {int(f["id"]) for f in request.facts}
        ranked = tuple(int(x) for x in items if isinstance(x, (int, float)) and int(x) in valid_ids)
        if not ranked:
            return _fallback(request, model_used)
        return RerankFactsResponse(ranked_ids=ranked[: request.top_k], model_used=model_used)
    except Exception as exc:
        logger.warning("reranking failed, using original order", extra={"error": str(exc)})
        return _fallback(request, model_used)


def _fallback(request: RerankFactsRequest, model_used: str) -> RerankFactsResponse:
    """Return facts in their original order, truncated to top_k."""
    ids = tuple(int(f["id"]) for f in request.facts)[: request.top_k]
    return RerankFactsResponse(ranked_ids=ids, model_used=model_used)
