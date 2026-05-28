"""Stdlib HTTP server exposing the AI service endpoints."""

from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable, Any

from .llm import MockLLMGateway, ModelRouter, OpenRouterLLMGateway, UnconfiguredLLMGateway
from .schemas import (
    parse_draft_request,
    parse_embed_texts_request,
    parse_intent_request,
    parse_memory_request,
    parse_process_message_request,
    parse_risk_request,
    parse_summary_request,
    parse_typed_extraction_request,
)
from .service import AIService, build_service


class AIHTTPHandler(BaseHTTPRequestHandler):
    """HTTP handler for health, readiness, and AI JSON endpoints."""

    service: AIService
    mock_llm: bool

    def do_GET(self) -> None:
        """Handle liveness and readiness requests."""
        if self.path == "/health":
            self._write_json(200, self.service.health())
            return
        if self.path == "/ready":
            self._write_json(200, self.service.ready(self.mock_llm))
            return
        self._write_json(404, {"error": "not_found"})

    def do_POST(self) -> None:
        """Handle AI service JSON requests."""
        routes: dict[str, Callable[[dict[str, Any]], dict[str, Any]]] = {
            "/ai/process-message": self._process_message,
            "/ai/generate-draft": self._generate_draft,
            "/ai/classify-risk": self._classify_risk,
            "/ai/classify-intent": self._classify_intent,
            "/ai/extract-memory": self._extract_memory,
            "/ai/extract-facts-typed": self._extract_facts_typed,
            "/ai/embed-texts": self._embed_texts,
            "/ai/summarize-chat": self._summarize_chat,
        }
        route = routes.get(self.path)
        if route is None:
            self._write_json(404, {"error": "not_found"})
            return
        try:
            payload = self._read_json()
            self._log_json("ai request received", _request_summary(self.path, payload))
            response = route(payload)
            self._log_json("ai response ready", _response_summary(self.path, payload, response))
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

    def _generate_draft(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.generate_draft(parse_draft_request(payload)).to_dict()

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

    def _summarize_chat(self, payload: dict[str, Any]) -> dict[str, Any]:
        return self.service.summarize_chat(parse_summary_request(payload)).to_dict()

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


def create_service_from_env() -> tuple[AIService, bool]:
    """Create the service and report whether deterministic mock mode is active."""
    mock_llm = os.getenv("MOCK_LLM", "true").lower() in ("1", "true", "yes", "on")
    model = os.getenv("OPENROUTER_MODEL", "").strip()
    if mock_llm:
        gateway = MockLLMGateway()
        router = ModelRouter(gateway)
    elif os.getenv("OPENROUTER_API_KEY", "").strip() and model:
        gateway = OpenRouterLLMGateway(
            api_key=os.getenv("OPENROUTER_API_KEY", ""),
            model=model,
            fallback_model=os.getenv("OPENROUTER_FALLBACK_MODEL", ""),
            base_url=os.getenv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
            app_name=os.getenv("OPENROUTER_APP_NAME", "DoubleU"),
            app_url=os.getenv("OPENROUTER_APP_URL", ""),
            timeout_seconds=float(os.getenv("OPENROUTER_TIMEOUT_SECONDS", "30")),
        )
        router = ModelRouter(gateway, fixed_model=model)
    else:
        gateway = UnconfiguredLLMGateway()
        router = ModelRouter(gateway)
    return build_service(router), mock_llm


def make_handler(service: AIService, mock_llm: bool) -> type[AIHTTPHandler]:
    """Create a request handler class bound to a service instance."""

    class BoundAIHTTPHandler(AIHTTPHandler):
        """Request handler bound to a concrete service instance."""

    BoundAIHTTPHandler.service = service
    BoundAIHTTPHandler.mock_llm = mock_llm
    return BoundAIHTTPHandler


def _request_summary(path: str, payload: dict[str, Any]) -> dict[str, Any]:
    event = payload.get("event") if isinstance(payload.get("event"), dict) else {}
    settings = payload.get("chat_settings") if isinstance(payload.get("chat_settings"), dict) else {}
    return {
        "path": path,
        "request_id": payload.get("request_id", ""),
        "tenant_id": event.get("tenant_id", ""),
        "chat_id": event.get("chat_id", ""),
        "source_message_id": event.get("source_message_id", ""),
        "message_type": event.get("message_type", ""),
        "message_text": _truncate(str(event.get("message_text", "")), 1000),
        "mode": settings.get("mode", ""),
        "privacy_flags": settings.get("privacy_flags", []),
        "memory_context_count": len(payload.get("memory_context", []) or []),
    }


def _response_summary(path: str, payload: dict[str, Any], response: dict[str, Any]) -> dict[str, Any]:
    event = payload.get("event") if isinstance(payload.get("event"), dict) else {}
    usage = response.get("usage") if isinstance(response.get("usage"), dict) else {}
    return {
        "path": path,
        "request_id": payload.get("request_id", ""),
        "tenant_id": event.get("tenant_id", ""),
        "chat_id": event.get("chat_id", ""),
        "source_message_id": event.get("source_message_id", ""),
        "decision": response.get("decision", ""),
        "risk_level": response.get("risk_level", ""),
        "intent": response.get("intent", ""),
        "confidence": response.get("confidence", 0),
        "model_used": response.get("model_used", ""),
        "response_text": _truncate(str(response.get("response_text", "")), 1000),
        "usage_input_tokens": usage.get("input_tokens", 0),
        "usage_output_tokens": usage.get("output_tokens", 0),
        "memory_used_count": len(response.get("memory_used", []) or []),
    }


def _truncate(value: str, limit: int) -> str:
    value = value.strip()
    if len(value) <= limit:
        return value
    return value[:limit] + "...[truncated]"


def run(host: str = "0.0.0.0", port: int = 8081) -> None:
    """Run the stdlib HTTP service until interrupted."""
    service, mock_llm = create_service_from_env()
    server = ThreadingHTTPServer((host, port), make_handler(service, mock_llm))
    server.serve_forever()
