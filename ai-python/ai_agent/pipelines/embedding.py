"""Embedding pipeline.

Hosts the urllib transport for ``/ai/embed-texts``.

When ``LLM_GATEWAY=litellm`` all embedding calls are routed through the LiteLLM
proxy (``embed-qwen`` alias → ``openrouter/qwen/qwen3-embedding-8b``) so every
provider call goes through OpenRouter and is logged to Langfuse.  The direct
OpenRouter / OpenAI path is kept as a fallback for local dev when LiteLLM is
not running.
"""

from __future__ import annotations

import json
import logging
import os
import urllib.request

from ..schemas import EmbedTextsRequest, EmbedTextsResponse


logger = logging.getLogger(__name__)

# LiteLLM alias declared in infra/litellm/config.yaml.
# Resolves to openrouter/qwen/qwen3-embedding-8b so the call always goes
# through OpenRouter, never to a provider API directly.
_LITELLM_EMBED_ALIAS = "embed-qwen"


def embed_texts(request: EmbedTextsRequest) -> EmbedTextsResponse:
    """Generate embeddings for a batch of texts.

    Routing precedence:
    1. LiteLLM proxy (``LLM_GATEWAY=litellm``) — preferred in all deployments.
       Ensures every provider call goes through OpenRouter and is observable.
    2. OpenRouter direct — fallback for local dev without LiteLLM running.
    3. OpenAI direct — last resort for pure-OpenAI local setups.

    Upstream failures are surfaced via a warning log so silent embed misses no
    longer go undetected; on failure the response carries zero-length vectors
    and ``model_used="error"`` so callers can proceed without crashing.
    """
    if not request.texts:
        return EmbedTextsResponse(embeddings=(), model_used="none")

    base_url, api_key, model, extra_headers = _resolve_endpoint(request.model)
    if not api_key:
        empty: tuple[tuple[float, ...], ...] = tuple(() for _ in request.texts)
        return EmbedTextsResponse(embeddings=empty, model_used="none")

    body = json.dumps({"input": list(request.texts), "model": model}).encode("utf-8")
    headers: dict[str, str] = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
        **extra_headers,
    }
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
        logger.warning(
            "embed_texts failed",
            extra={"model": model, "base_url": base_url, "error": str(exc)},
        )
        empty = tuple(() for _ in request.texts)
        return EmbedTextsResponse(embeddings=empty, model_used="error")


def _resolve_endpoint(
    requested_model: str,
) -> tuple[str, str, str, dict[str, str]]:
    """Return (base_url, api_key, model, extra_headers) for the embedding call.

    Prefers the LiteLLM proxy when ``LLM_GATEWAY=litellm`` is set and both
    ``LITELLM_BASE_URL`` and ``LITELLM_MASTER_KEY`` are configured.  Falls back
    to direct OpenRouter or OpenAI credentials.
    """
    gateway = os.getenv("LLM_GATEWAY", "").strip().lower()
    litellm_url = os.getenv("LITELLM_BASE_URL", "").strip()
    litellm_key = os.getenv("LITELLM_MASTER_KEY", "").strip()

    if gateway == "litellm" and litellm_url and litellm_key:
        return litellm_url, litellm_key, _LITELLM_EMBED_ALIAS, {}

    openrouter_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    openai_key = os.getenv("OPENAI_API_KEY", "").strip()
    api_key = openrouter_key or openai_key
    if not api_key:
        return "", "", requested_model, {}

    if openrouter_key:
        base_url = os.getenv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1")
        extra: dict[str, str] = {}
        app_name = os.getenv("OPENROUTER_APP_NAME", "").strip()
        app_url = os.getenv("OPENROUTER_APP_URL", "").strip()
        if app_url:
            extra["HTTP-Referer"] = app_url
        if app_name:
            extra["X-Title"] = app_name
        return base_url, api_key, requested_model, extra

    base_url = os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
    return base_url, api_key, requested_model, {}
