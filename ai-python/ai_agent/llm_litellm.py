"""LiteLLM Proxy gateway (Phase 2).

LiteLLM is the OSS OpenAI-compatible proxy that owns retries, fallback chains,
idempotency dedup (Redis) and per-tenant budget caps.  This module is a thin
HTTP client that targets ``LITELLM_BASE_URL`` and forwards the per-purpose
metadata the proxy needs (idempotency key, tenant, purpose, trace_id).

No retry / fallback logic lives in Python — that is LiteLLM's job.  When the
proxy returns 5xx the gateway raises ``RuntimeError`` so the surrounding
pipeline marks the call as ``error_fallback``.
"""

from __future__ import annotations

import http.client
import json
import socket
import urllib.error
import urllib.request
from typing import Callable

from .observability.context import current_context
from .schemas import GatewayRequest, GatewayResponse, Usage


class LiteLLMGateway:
    """Provider-agnostic chat-completion gateway routed through LiteLLM.

    Headers carried per request:

    * ``Authorization: Bearer <master_key>`` — LiteLLM master key.
    * ``X-Idempotency-Key`` — taken from ``current_context().event_id`` so
      duplicate webhook replays hit the LiteLLM Redis cache instead of
      re-billing the provider.
    * ``X-LiteLLM-Tenant-ID`` — propagates ``current_context().tenant_id`` so
      per-tenant budgets and spend dashboards group correctly.
    * ``X-LiteLLM-Purpose`` — propagates the purpose tag (``draft``,
      ``extract``…) for model routing inside LiteLLM.
    * ``traceparent`` — W3C trace propagation so Langfuse joins Go + Python
      spans onto one trace.
    """

    def __init__(
        self,
        master_key: str,
        base_url: str = "http://litellm:4000",
        timeout_seconds: float = 30.0,
        transport: Callable[[urllib.request.Request, float], tuple[bytes, dict[str, str]]] | None = None,
    ) -> None:
        """Create a LiteLLM gateway with the master key and base URL."""
        self._master_key = master_key.strip()
        self._base_url = base_url.rstrip("/")
        self._timeout_seconds = timeout_seconds
        self._transport = transport or _urlopen_with_headers

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        """Send a chat-completion request to LiteLLM and return the response."""
        if not self._master_key:
            raise RuntimeError("LITELLM_MASTER_KEY is required for the LiteLLM gateway")
        ctx = current_context()
        payload: dict[str, object] = {
            "model": request.model_name,
            "messages": [
                {"role": "system", "content": request.system_prompt},
                {"role": "user", "content": request.user_payload},
            ],
            "temperature": 0.2,
        }
        # Pass trace_id via LiteLLM metadata so the proxy uses the same ID
        # when logging this call to Langfuse.  Without this LiteLLM generates
        # its own UUID and the app-side spans and the gateway generation appear
        # on different traces in the Langfuse UI.
        if ctx.trace_id:
            payload["metadata"] = {
                "trace_id": ctx.trace_id,
                "session_id": ctx.chat_id or None,
                "tags": [ctx.purpose.value] if ctx.purpose is not None else [],
                "generation_name": f"{ctx.purpose.value}.complete" if ctx.purpose is not None else "llm.complete",
            }
        headers = self._headers(ctx)
        http_request = urllib.request.Request(
            self._base_url + "/chat/completions",
            data=json.dumps(payload).encode("utf-8"),
            headers=headers,
            method="POST",
        )
        try:
            raw, response_headers = self._transport(http_request, self._timeout_seconds)
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")[:512]
            raise RuntimeError(f"LiteLLM request failed with status {exc.code}: {detail}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"LiteLLM request failed: {exc.reason}") from exc
        except (TimeoutError, socket.timeout) as exc:
            raise RuntimeError("LiteLLM request timed out") from exc
        response = json.loads(raw.decode("utf-8"))
        text = _extract_text(response)
        usage = response.get("usage") or {}
        cost_usd = _parse_cost(response_headers)
        return GatewayResponse(
            text=text,
            model_used=str(response.get("model") or request.model_name),
            usage=Usage(
                input_tokens=int(usage.get("prompt_tokens") or 0),
                output_tokens=int(usage.get("completion_tokens") or 0),
            ),
            cost_usd=cost_usd,
        )

    def _headers(self, ctx) -> dict[str, str]:
        """Build the LiteLLM HTTP headers for one call."""
        headers: dict[str, str] = {
            "Authorization": "Bearer " + self._master_key,
            "Content-Type": "application/json",
        }
        if ctx.event_id:
            headers["X-Idempotency-Key"] = ctx.event_id
        if ctx.tenant_id:
            headers["X-LiteLLM-Tenant-ID"] = ctx.tenant_id
        if ctx.purpose is not None:
            headers["X-LiteLLM-Purpose"] = ctx.purpose.value
        if ctx.trace_id:
            headers["traceparent"] = f"00-{ctx.trace_id}-0000000000000000-01"
        return headers


def _urlopen_with_headers(
    request: urllib.request.Request, timeout_seconds: float,
) -> tuple[bytes, dict[str, str]]:
    """Default transport: stdlib urlopen exposing response headers for cost parsing."""
    # RemoteDisconnected is raised by http.client during getresponse() when the
    # server closes the connection after the TCP handshake but before sending any
    # HTTP response headers.  urllib only wraps errors from the send phase, so we
    # normalise it to URLError here so the caller's existing handler picks it up.
    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            body = response.read()
            headers = {k.lower(): v for k, v in response.headers.items()}
            return body, headers
    except http.client.RemoteDisconnected as exc:
        raise urllib.error.URLError(exc) from exc


def _parse_cost(headers: dict[str, str]) -> float:
    """Parse the ``x-litellm-response-cost`` USD header LiteLLM emits."""
    raw = headers.get("x-litellm-response-cost") or headers.get("X-LiteLLM-Response-Cost")
    if not raw:
        return 0.0
    try:
        return float(raw)
    except (TypeError, ValueError):
        return 0.0


def _extract_text(response: dict[str, object]) -> str:
    """Extract the chat completion text from a LiteLLM response."""
    choices = response.get("choices")
    if not isinstance(choices, list) or not choices:
        raise RuntimeError("LiteLLM response did not include choices")
    first = choices[0]
    if not isinstance(first, dict):
        raise RuntimeError("LiteLLM response choice is invalid")
    message = first.get("message")
    if not isinstance(message, dict):
        raise RuntimeError("LiteLLM response did not include a message")
    content = message.get("content")
    if not isinstance(content, str):
        raise RuntimeError("LiteLLM response message content is invalid")
    return content.strip()
