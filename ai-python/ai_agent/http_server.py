"""Stdlib HTTP server exposing the AI service endpoints."""

from __future__ import annotations

import atexit
import json
import os
import sys
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable, Any

from .config.flags import FeatureFlags, load_from_env as load_flags_from_env
from .contract_enums import LLMPurpose
from .observability.context import LLMCallContext, with_context
from .observability.langfuse_sink import HttpUploader, LangfuseSink
from .observability.trace import derive_trace_id, parse_traceparent
from .safety.input import InputSafetyScanner
from .safety.output import OutputSafetyScanner
from .schemas import (
    parse_embed_texts_request,
    parse_draft_role_request,
    parse_extract_media_request,
    parse_intent_request,
    parse_memory_request,
    parse_process_message_request,
    parse_rerank_facts_request,
    parse_risk_request,
    parse_summary_request,
    parse_synthesize_pattern_request,
    parse_topic_tagging_request,
    parse_typed_extraction_request,
)
from .service import AIService, build_service


@dataclass(frozen=True)
class ModelConfigResponse:
    """Wire shape of the GET /ai/model-config response."""

    fast: list[str]
    balanced: list[str]
    smart: list[str]
    default: str

    def to_dict(self) -> dict[str, Any]:
        """Return the model configuration in its JSON wire shape."""
        return {
            "fast": self.fast,
            "balanced": self.balanced,
            "smart": self.smart,
            "default": self.default,
        }


def _build_model_config() -> ModelConfigResponse:
    """Read MODEL_TIER_* and OPENROUTER_MODEL env vars and build the config response."""
    def _split(val: str) -> list[str]:
        return [m.strip() for m in val.split(",") if m.strip()]

    return ModelConfigResponse(
        fast=_split(os.getenv("MODEL_TIER_FAST", "")),
        balanced=_split(os.getenv("MODEL_TIER_BALANCED", "")),
        smart=_split(os.getenv("MODEL_TIER_SMART", "")),
        default=os.getenv("OPENROUTER_MODEL", ""),
    )


class AIHTTPHandler(BaseHTTPRequestHandler):
    """HTTP handler for health, readiness, and AI JSON endpoints."""

    service: AIService
    mock_llm: bool
    flags: FeatureFlags = FeatureFlags()

    def do_GET(self) -> None:
        """Handle liveness, readiness, and config requests."""
        if self.path == "/health":
            self._write_json(200, self.service.health())
            return
        if self.path == "/ready":
            self._write_json(200, self.service.ready(self.mock_llm))
            return
        if self.path == "/ai/model-config":
            self._write_json(200, _build_model_config().to_dict())
            return
        self._write_json(404, {"error": "not_found"})

    def do_POST(self) -> None:
        """Handle AI service JSON requests."""
        routes: dict[str, Callable[[dict[str, Any]], dict[str, Any]]] = {
            "/ai/process-message": self._process_message,
            "/ai/classify-risk": self._classify_risk,
            "/ai/classify-intent": self._classify_intent,
            "/ai/extract-memory": self._extract_memory,
            "/ai/extract-facts-typed": self._extract_facts_typed,
            "/ai/embed-texts": self._embed_texts,
            "/ai/extract-media": self._extract_media,
            "/ai/draft-role": self._draft_role,
            "/ai/summarize-chat": self._summarize_chat,
            "/ai/tag-topic": self._tag_topic,
            "/ai/rerank-facts": self._rerank_facts,
            "/ai/synthesize-pattern": self._synthesize_pattern,
        }
        route = routes.get(self.path)
        if route is None:
            self._write_json(404, {"error": "not_found"})
            return
        try:
            payload = self._read_json()
            trace_id, ctx = self._build_call_context(payload)
            payload.setdefault("trace_id", trace_id)
            self._log_json("ai request received", {"trace_id": trace_id, **_request_summary(self.path, payload)})
            t0 = time.perf_counter()
            with with_context(ctx):
                response = route(payload)
            latency_ms = (time.perf_counter() - t0) * 1000
            self._log_json("ai response ready", {"trace_id": trace_id, **_response_summary(self.path, payload, response, latency_ms)})
            self._write_json(200, response)
        except (KeyError, TypeError, ValueError) as exc:
            self._log_json("ai request rejected", {"path": self.path, "error": str(exc)})
            self._write_json(400, {"error": "bad_request", "detail": str(exc)})
        except RuntimeError as exc:
            self._log_json("ai request failed", {"path": self.path, "error": str(exc)})
            self._write_json(503, {"error": "llm_unavailable", "detail": str(exc)})
        except (BrokenPipeError, ConnectionResetError) as exc:
            # Client closed the connection before we finished writing — log and move on.
            # This happens when the Go HTTP client times out while the LLM is still running.
            self._log_json("ai response not delivered: client disconnected", {
                "path": self.path,
                "error": str(exc),
            })

    def log_message(self, format: str, *args: object) -> None:
        """Suppress default noisy request logging for tests and local runs."""
        return

    def _process_message(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.process_message(parse_process_message_request(payload)).to_dict()

    def _classify_risk(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.classify_risk(parse_risk_request(payload)).to_dict()

    def _classify_intent(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.classify_intent(parse_intent_request(payload)).to_dict()

    def _extract_memory(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.extract_memory(parse_memory_request(payload)).to_dict()

    def _extract_facts_typed(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.extract_facts_typed(parse_typed_extraction_request(payload)).to_dict()

    def _embed_texts(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.embed_texts(parse_embed_texts_request(payload)).to_dict()

    def _extract_media(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.extract_media(parse_extract_media_request(payload)).to_dict()

    def _draft_role(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.draft_role(parse_draft_role_request(payload)).to_dict()

    def _summarize_chat(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.summarize_chat(parse_summary_request(payload)).to_dict()

    def _tag_topic(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.tag_topic(parse_topic_tagging_request(payload)).to_dict()

    def _rerank_facts(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.rerank_facts(parse_rerank_facts_request(payload)).to_dict()

    def _synthesize_pattern(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.synthesize_pattern(parse_synthesize_pattern_request(payload)).to_dict()

    # _PURPOSE_BY_PATH maps each endpoint to the LLMPurpose tag the safety /
    # observability layers will see in the call context.
    _PURPOSE_BY_PATH: dict[str, LLMPurpose] = {
        "/ai/process-message": LLMPurpose.DRAFT,
        "/ai/classify-risk": LLMPurpose.CLASSIFY,
        "/ai/classify-intent": LLMPurpose.CLASSIFY,
        "/ai/extract-memory": LLMPurpose.EXTRACT,
        "/ai/extract-facts-typed": LLMPurpose.EXTRACT,
        "/ai/embed-texts": LLMPurpose.EMBED,
        "/ai/extract-media": LLMPurpose.EXTRACT,
        "/ai/draft-role": LLMPurpose.DRAFT,
        "/ai/summarize-chat": LLMPurpose.SUMMARIZE,
        "/ai/tag-topic": LLMPurpose.EXTRACT,
        "/ai/rerank-facts": LLMPurpose.EXTRACT,
        "/ai/synthesize-pattern": LLMPurpose.EXTRACT,
    }

    def _build_call_context(self, payload: dict[str, Any]) -> tuple[str, LLMCallContext]:
        """Derive a trace_id and ``LLMCallContext`` for the current request.

        Precedence:
          1. ``traceparent`` header (W3C standard, preferred).
          2. ``X-DoubleU-Trace-ID`` (transitional fallback for one release).
          3. ``derive_trace_id(event.event_id)`` for process-message events.
          4. ``payload["trace_id"]`` echoed back from the Go backend.
        """
        trace_id = parse_traceparent(self.headers.get("traceparent"))
        if not trace_id:
            trace_id = (self.headers.get("X-DoubleU-Trace-ID") or "").strip()
        if not trace_id:
            event = payload.get("event") if isinstance(payload.get("event"), dict) else None
            if event is not None:
                trace_id = derive_trace_id(str(event.get("event_id", "")))
        if not trace_id:
            trace_id = str(payload.get("trace_id", "")).strip()

        ctx_payload = payload.get("event") if isinstance(payload.get("event"), dict) else payload
        ctx = LLMCallContext(
            trace_id=trace_id,
            tenant_id=str(ctx_payload.get("tenant_id", "")),
            chat_id=str(ctx_payload.get("chat_id", "")),
            event_id=str(ctx_payload.get("event_id", "")),
            purpose=self._PURPOSE_BY_PATH.get(self.path),
        )
        return trace_id, ctx

    def _read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        payload = json.loads(raw.decode("utf-8") if raw else "{}")
        if not isinstance(payload, dict):
            raise ValueError("JSON body must be an object")
        return payload

    def _write_json(self, status: int, payload: dict[str, Any]) -> None:
        raw = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _log_json(self, message: str, fields: dict[str, Any]) -> None:
        record = {"level": "INFO", "msg": message, **fields}
        print(json.dumps(record, ensure_ascii=False, separators=(",", ":")), file=sys.stdout, flush=True)


def create_service_from_env() -> tuple[AIService, bool, FeatureFlags]:
    """Create the service and report whether deterministic mock mode is active.

    Returns a ``(service, mock_llm, flags)`` tuple so callers can mount the
    flags on the request handler.  All generation runs through the pydantic-ai
    :class:`AgentRuntime`: ``MOCK_LLM`` selects the deterministic TestModel,
    otherwise the agents resolve OpenRouter-backed models from the environment.
    The Phase 1/3 safety scanners and the Langfuse sink are built here (flag-
    gated) and injected into the runtime.
    """
    flags = load_flags_from_env()
    mock_llm = os.getenv("MOCK_LLM", "true").lower() in ("1", "true", "yes", "on")
    input_safety = InputSafetyScanner(flags=flags) if flags.safety_input_enabled else None
    output_safety = OutputSafetyScanner(flags=flags) if flags.safety_output_enabled else None
    sink = _build_langfuse_sink(flags)
    # Imported lazily so importing the HTTP module never hard-requires pydantic-ai.
    from .agents import build_runtime

    agents = build_runtime(
        input_safety=input_safety,
        output_safety=output_safety,
        sink=sink,
        two_stage_draft=flags.two_stage_draft,
        prompt_contract_enabled=flags.prompt_contract_enabled,
    )
    return build_service(agents), mock_llm, flags


def _build_langfuse_sink(flags: FeatureFlags) -> LangfuseSink | None:
    """Instantiate a LangfuseSink with a live HTTP uploader when OBSERVABILITY_SINK=langfuse.

    Returns ``None`` when the sink is disabled or credentials are missing so
    the agent runtime degrades to noop observability without changing call sites.
    """
    if flags.observability_sink != "langfuse":
        return None
    host = os.getenv("LANGFUSE_HOST", "").strip()
    public_key = os.getenv("LANGFUSE_PUBLIC_KEY", "").strip()
    secret_key = os.getenv("LANGFUSE_SECRET_KEY", "").strip()
    if not (host and public_key and secret_key):
        return None
    uploader = HttpUploader(host=host, public_key=public_key, secret_key=secret_key)
    sink = LangfuseSink(host=host, public_key=public_key, secret_key=secret_key, uploader=uploader)
    atexit.register(sink.flush)
    return sink


def make_handler(service: AIService, mock_llm: bool, flags: FeatureFlags | None = None) -> type[AIHTTPHandler]:
    """Create a request handler class bound to a service instance."""

    class BoundAIHTTPHandler(AIHTTPHandler):
        """Request handler bound to a concrete service instance."""

    BoundAIHTTPHandler.service = service
    BoundAIHTTPHandler.mock_llm = mock_llm
    BoundAIHTTPHandler.flags = flags or FeatureFlags()
    return BoundAIHTTPHandler


def _extract_context(path: str, payload: dict[str, Any]) -> dict[str, Any]:
    """Extract per-endpoint context fields from the payload.

    process-message uses a nested event/chat_settings envelope.
    All other endpoints use a flat schema — fields live at the top level.
    embed-texts has no message_text; reports texts_count instead.
    """
    event = payload.get("event") if isinstance(payload.get("event"), dict) else None
    settings = payload.get("chat_settings") if isinstance(payload.get("chat_settings"), dict) else None

    if event is not None:
        source: dict[str, Any] = event
        mode = settings.get("mode", "") if settings else ""
        privacy_flags = settings.get("privacy_flags", []) if settings else []
    else:
        source = payload
        mode = payload.get("mode", "")
        privacy_flags = payload.get("privacy_flags", [])

    ctx: dict[str, Any] = {
        "tenant_id": source.get("tenant_id", ""),
        "chat_id": source.get("chat_id", ""),
        "source_message_id": source.get("source_message_id", ""),
        "message_type": source.get("message_type", ""),
        "mode": mode,
        "privacy_flags": privacy_flags,
        "memory_context_count": len(payload.get("memory_context", []) or []),
    }
    if path == "/ai/embed-texts":
        ctx["texts_count"] = len(payload.get("texts", []))
    else:
        ctx["message_text"] = _truncate(str(source.get("message_text", "")), 1000)
    return ctx


def _request_summary(path: str, payload: dict[str, Any]) -> dict[str, Any]:
    # Elide raw media bytes from the request preview: base64 blobs are large and
    # are private user content, not useful in logs.
    preview = payload
    if path == "/ai/draft-role" and "text" in preview:
        preview = {**preview, "text": f"<{len(str(payload.get('text', '')))} chars>"}
    for field_name in ("data_base64", "audio_base64"):
        if field_name in preview:
            preview = {
                **preview,
                field_name: f"<{len(str(payload.get(field_name, '')))} b64 chars>",
            }
    return {
        "path": path,
        "request_id": payload.get("request_id", ""),
        **_extract_context(path, payload),
        "request_body_preview": _truncate(json.dumps(preview, ensure_ascii=False), 2000),
    }


def _response_summary(path: str, payload: dict[str, Any], response: dict[str, Any], latency_ms: float) -> dict[str, Any]:
    ctx = _extract_context(path, payload)
    usage = response.get("usage") if isinstance(response.get("usage"), dict) else {}
    return {
        "path": path,
        "request_id": payload.get("request_id", ""),
        "tenant_id": ctx["tenant_id"],
        "chat_id": ctx["chat_id"],
        "source_message_id": ctx["source_message_id"],
        "decision": response.get("decision", ""),
        "risk_level": response.get("risk_level", ""),
        "intent": response.get("intent", ""),
        "confidence": response.get("confidence", 0),
        "model_used": response.get("model_used", ""),
        "response_text": _truncate(str(response.get("response_text", "")), 1000),
        "usage_input_tokens": usage.get("input_tokens", 0),
        "usage_output_tokens": usage.get("output_tokens", 0),
        "memory_used_count": len(response.get("memory_used", []) or []),
        "latency_ms": round(latency_ms, 1),
        "response_body_preview": (
            "<role draft omitted>"
            if path == "/ai/draft-role"
            else _truncate(json.dumps(response, ensure_ascii=False), 2000)
        ),
    }


def _truncate(value: str, limit: int) -> str:
    value = value.strip()
    if len(value) <= limit:
        return value
    return value[:limit] + "...[truncated]"


def run(host: str = "0.0.0.0", port: int = 8081) -> None:
    """Run the stdlib HTTP service until interrupted."""
    service, mock_llm, flags = create_service_from_env()
    server = ThreadingHTTPServer((host, port), make_handler(service, mock_llm, flags))
    server.serve_forever()
