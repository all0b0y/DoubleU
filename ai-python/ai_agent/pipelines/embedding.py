"""Embedding pipeline.

Hosts the urllib transport for ``/ai/embed-texts`` and the OpenRouter ↔ OpenAI
credential fallback.  Lives outside the LLM gateway because embeddings have
their own provider routing and are not part of the chat-completion path.
"""

from __future__ import annotations

import json
import logging
import os
import urllib.request

from ..schemas import EmbedTextsRequest, EmbedTextsResponse


logger = logging.getLogger(__name__)


def embed_texts(request: EmbedTextsRequest) -> EmbedTextsResponse:
    """Generate embeddings for a batch of texts.

    Prefers OpenRouter credentials when available (the production deployment
    only sets ``OPENROUTER_*`` envs), falling back to ``OPENAI_*`` envs for
    local dev.  Upstream failures are surfaced via a warning log so silent
    embed misses no longer go undetected; on failure the response carries
    zero-length vectors and ``model_used="error"`` so callers can proceed
    without crashing.
    """
    openrouter_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    openai_key = os.getenv("OPENAI_API_KEY", "").strip()
    api_key = openrouter_key or openai_key
    if not api_key or not request.texts:
        empty: tuple[tuple[float, ...], ...] = tuple(() for _ in request.texts)
        return EmbedTextsResponse(embeddings=empty, model_used="none")

    if openrouter_key:
        base_url = os.getenv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1")
    else:
        base_url = os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
    model = request.model
    body = json.dumps({"input": list(request.texts), "model": model}).encode("utf-8")
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
    }
    if openrouter_key:
        app_name = os.getenv("OPENROUTER_APP_NAME", "").strip()
        app_url = os.getenv("OPENROUTER_APP_URL", "").strip()
        if app_url:
            headers["HTTP-Referer"] = app_url
        if app_name:
            headers["X-Title"] = app_name
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/embeddings",
        data=body,
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read())
        embeddings = tuple(
            tuple(item["embedding"])
            for item in sorted(data["data"], key=lambda x: x["index"])
        )
        return EmbedTextsResponse(embeddings=embeddings, model_used=model)
    except Exception as exc:
        # Surface the upstream failure: silent swallows previously masked a
        # 404 from OpenRouter for `text-embedding-3-small`, leaving
        # `fact_embeddings` permanently empty.
        logger.warning(
            "embed_texts failed",
            extra={"model": model, "base_url": base_url, "error": str(exc)},
        )
        empty = tuple(() for _ in request.texts)
        return EmbedTextsResponse(embeddings=empty, model_used="error")
