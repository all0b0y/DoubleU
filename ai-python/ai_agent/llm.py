"""Model routing and gateway boundary for all LLM-like generation."""

from __future__ import annotations

from abc import ABC, abstractmethod
import json
import socket
from typing import Callable
import urllib.error
import urllib.request

from .schemas import GatewayRequest, GatewayResponse, ModelTask, RiskLevel, Usage


class LLMGateway(ABC):
    """Abstract boundary for provider-backed language model calls."""

    @abstractmethod
    def complete(self, request: GatewayRequest) -> GatewayResponse:
        """Return a completion for a routed gateway request."""


class MockLLMGateway(LLMGateway):
    """Deterministic gateway used when `MOCK_LLM=true`."""

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        """Return a deterministic mock completion for the task."""
        input_tokens = _estimate_tokens(request.system_prompt) + _estimate_tokens(request.user_payload)
        if request.task == ModelTask.SUMMARIZE_CHAT:
            text = _mock_summary(request.user_payload)
        elif request.task == ModelTask.EXTRACT_FACTS:
            text = _mock_facts(request.user_payload)
        else:
            text = _mock_draft(request.user_payload)
        return GatewayResponse(
            text=text,
            model_used=request.model_name,
            usage=Usage(input_tokens=input_tokens, output_tokens=_estimate_tokens(text)),
        )


class UnconfiguredLLMGateway(LLMGateway):
    """Gateway placeholder used when no provider implementation is configured."""

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        """Raise a clear error because provider calls are not configured."""
        raise RuntimeError("LLM gateway is not configured; set MOCK_LLM=true for deterministic local mode")


class OpenRouterLLMGateway(LLMGateway):
    """Gateway implementation that sends routed requests to OpenRouter."""

    def __init__(
        self,
        api_key: str,
        model: str,
        fallback_model: str = "",
        base_url: str = "https://openrouter.ai/api/v1",
        app_name: str = "DoubleU",
        app_url: str = "",
        timeout_seconds: float = 30.0,
        transport: Callable[[urllib.request.Request, float], bytes] | None = None,
    ) -> None:
        """Create an OpenRouter gateway with provider credentials and HTTP transport."""
        self._api_key = api_key.strip()
        self._model = model.strip()
        self._fallback_model = fallback_model.strip()
        self._base_url = base_url.rstrip("/")
        self._app_name = app_name.strip() or "DoubleU"
        self._app_url = app_url.strip()
        self._timeout_seconds = timeout_seconds
        self._transport = transport or _urlopen_bytes

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        """Send a chat-completion request to OpenRouter and return normalized text and usage."""
        if not self._api_key:
            raise RuntimeError("OPENROUTER_API_KEY is required when MOCK_LLM=false")
        if not self._model:
            raise RuntimeError("OPENROUTER_MODEL is required when MOCK_LLM=false")
        try:
            return self._complete_with_model(request, self._model)
        except RuntimeError:
            if not self._fallback_model or self._fallback_model == self._model:
                raise
            return self._complete_with_model(request, self._fallback_model)

    def _complete_with_model(self, request: GatewayRequest, model: str) -> GatewayResponse:
        """Send a chat-completion request for one concrete OpenRouter model."""
        payload = {
            "model": model,
            "messages": [
                {"role": "system", "content": request.system_prompt},
                {"role": "user", "content": request.user_payload},
            ],
            "temperature": 0.2,
        }
        http_request = urllib.request.Request(
            self._base_url + "/chat/completions",
            data=json.dumps(payload).encode("utf-8"),
            headers=self._headers(),
            method="POST",
        )
        try:
            raw = self._transport(http_request, self._timeout_seconds)
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")[:512]
            raise RuntimeError(f"OpenRouter request failed with status {exc.code}: {detail}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"OpenRouter request failed: {exc.reason}") from exc
        except (TimeoutError, socket.timeout) as exc:
            raise RuntimeError("OpenRouter request timed out") from exc
        response = json.loads(raw.decode("utf-8"))
        text = _openrouter_text(response)
        usage = response.get("usage") or {}
        return GatewayResponse(
            text=text,
            model_used=str(response.get("model") or model),
            usage=Usage(
                input_tokens=int(usage.get("prompt_tokens") or 0),
                output_tokens=int(usage.get("completion_tokens") or 0),
            ),
        )

    def _headers(self) -> dict[str, str]:
        """Build OpenRouter HTTP headers without exposing provider secrets."""
        headers = {
            "Authorization": "Bearer " + self._api_key,
            "Content-Type": "application/json",
            "X-Title": self._app_name,
        }
        if self._app_url:
            headers["HTTP-Referer"] = self._app_url
        return headers


class ModelRouter:
    """Select model tiers and delegate all generation to the configured gateway."""

    def __init__(self, gateway: LLMGateway, fixed_model: str = "") -> None:
        """Create a router backed by a concrete LLM gateway."""
        self._gateway = gateway
        self._fixed_model = fixed_model.strip()

    def route(self, task: ModelTask, risk_level: RiskLevel = RiskLevel.LOW, complexity: str = "normal") -> str:
        """Return the model name selected for a task and risk profile."""
        if self._fixed_model:
            return self._fixed_model
        if task in (ModelTask.SUMMARIZE_CHAT, ModelTask.EXTRACT_FACTS):
            return "mock-fast-summary-v1"
        if risk_level in (RiskLevel.HIGH, RiskLevel.CRITICAL) or complexity == "complex":
            return "mock-strong-v1"
        if risk_level == RiskLevel.MEDIUM:
            return "mock-base-v1"
        return "mock-fast-v1"

    def complete(
        self,
        task: ModelTask,
        system_prompt: str,
        user_payload: str,
        risk_level: RiskLevel = RiskLevel.LOW,
        complexity: str = "normal",
    ) -> GatewayResponse:
        """Route a task and return the gateway completion."""
        request = GatewayRequest(
            task=task,
            model_name=self.route(task, risk_level=risk_level, complexity=complexity),
            system_prompt=system_prompt,
            user_payload=user_payload,
            metadata={"risk_level": risk_level.value, "complexity": complexity},
        )
        return self._gateway.complete(request)


def _mock_draft(user_payload: str) -> str:
    cleaned = _extract_first_untrusted(user_payload)
    if not cleaned:
        return "Thanks, I will get back to you shortly."
    if "?" in cleaned:
        return "Thanks for the question. I will check the details and reply shortly."
    if any(word in cleaned.lower() for word in ("meeting", "call", "schedule")):
        return "Thanks, that works. Please send the preferred time and I will confirm."
    return "Thanks for the message. I will review it and get back to you soon."


def _mock_facts(user_payload: str) -> str:
    cleaned = _extract_first_untrusted(user_payload).lower()
    facts = []
    for keyword, fact in (
        ("my name is", "name mentioned"),
        ("i prefer", "preference mentioned"),
        ("i work", "work context mentioned"),
        ("call me", "preferred name mentioned"),
    ):
        if keyword in cleaned:
            facts.append(fact)
    return "; ".join(facts) if facts else ""


def _mock_summary(user_payload: str) -> str:
    messages = [line.strip() for line in user_payload.splitlines() if line.strip() and not line.startswith("<")]
    visible = [line for line in messages if not line.startswith("</")]
    if not visible:
        return "No chat content to summarize."
    first = visible[0][:120]
    return f"Summary: {len(visible)} message(s) reviewed. Main topic: {first}"


def _extract_first_untrusted(user_payload: str) -> str:
    lines = []
    inside = False
    for line in user_payload.splitlines():
        if line.startswith("<untrusted_user_message>"):
            inside = True
            continue
        if line.startswith("</untrusted_user_message>"):
            break
        if inside:
            lines.append(line)
    return "\n".join(lines).strip()


def _estimate_tokens(text: str) -> int:
    return max(0, (len(text.split()) + 1))


def _urlopen_bytes(request: urllib.request.Request, timeout_seconds: float) -> bytes:
    with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
        return response.read()


def _openrouter_text(response: dict[str, object]) -> str:
    choices = response.get("choices")
    if not isinstance(choices, list) or not choices:
        raise RuntimeError("OpenRouter response did not include choices")
    first_choice = choices[0]
    if not isinstance(first_choice, dict):
        raise RuntimeError("OpenRouter response choice is invalid")
    message = first_choice.get("message")
    if not isinstance(message, dict):
        raise RuntimeError("OpenRouter response did not include a message")
    content = message.get("content")
    if not isinstance(content, str):
        raise RuntimeError("OpenRouter response message content is invalid")
    return content.strip()
