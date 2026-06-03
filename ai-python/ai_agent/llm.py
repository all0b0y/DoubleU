"""Model routing and gateway boundary for all LLM-like generation."""

from __future__ import annotations

from abc import ABC, abstractmethod
import http.client
import json
import socket
from typing import TYPE_CHECKING, Callable
import urllib.error
import urllib.request

from .observability.context import current_context
from .observability.results import SafetyBlocked
from .schemas import GatewayRequest, GatewayResponse, ModelTask, RiskLevel, Usage

if TYPE_CHECKING:  # pragma: no cover - import only for type hints
    from .config.flags import FeatureFlags
    from .observability.langfuse_sink import LangfuseSink
    from .safety.input import InputSafetyScanner
    from .safety.output import OutputSafetyScanner


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
    """Select model tiers and delegate all generation to the configured gateway.

    The router is the single chokepoint for Phase 1 input safety and Phase 3
    output safety: both scanners attach here so concrete gateways
    (Mock/OpenRouter/LiteLLM) never see raw user payloads or unfiltered
    responses.  When both scanners are ``None`` the router behaves exactly as
    before.
    """

    # Semantic aliases that map purpose/task to LiteLLM proxy model names.
    # When LLM_GATEWAY=litellm these are used instead of the mock tier names
    # so the proxy resolves them against its own config.yaml model_list.
    # Risk-tier differentiation moves inside the proxy (fallback chains).
    _LITELLM_TASK_ALIASES: dict[ModelTask, str] = {
        ModelTask.SUMMARIZE_CHAT: "summarize-cheap",
        ModelTask.EXTRACT_FACTS: "extract-cheap",
    }
    _LITELLM_DRAFT_ALIAS = "draft-primary"

    def __init__(
        self,
        gateway: LLMGateway,
        fixed_model: str = "",
        flags: "FeatureFlags | None" = None,
        input_safety: "InputSafetyScanner | None" = None,
        output_safety: "OutputSafetyScanner | None" = None,
        legacy_gateway: LLMGateway | None = None,
        sink: "LangfuseSink | None" = None,
    ) -> None:
        """Create a router backed by a concrete LLM gateway."""
        self._gateway = gateway
        self._fixed_model = fixed_model.strip()
        self._flags = flags
        self._input_safety = input_safety
        self._output_safety = output_safety
        # legacy_gateway is set during Phase 5 shadow / cutover to drive the
        # ShadowRunner.  None means there is no parallel legacy path to run.
        self._legacy_gateway = legacy_gateway
        self._sink = sink

    def route(self, task: ModelTask, risk_level: RiskLevel = RiskLevel.LOW, complexity: str = "normal") -> str:
        """Return the model name selected for a task and risk profile.

        When ``LLM_GATEWAY=litellm`` the method returns the semantic aliases
        declared in the LiteLLM ``config.yaml`` model_list so the proxy can
        apply its own fallback and retry chains.  On the legacy OpenRouter
        direct path the old mock tier names are kept so existing OpenRouter
        routing logic is unchanged.
        """
        if self._fixed_model:
            return self._fixed_model
        if self._flags is not None and self._flags.llm_gateway == "litellm":
            return self._LITELLM_TASK_ALIASES.get(task, self._LITELLM_DRAFT_ALIAS)
        # Legacy / mock tier names for the OpenRouter direct path and mock mode.
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
        """Route a task and return the gateway completion.

        Pipeline:
          1. (Phase 1) InputSafetyScanner.scan — may rewrite prompts or block.
          2. Concrete gateway.complete — Mock/OpenRouter/LiteLLM.
          3. (Phase 3) OutputSafetyScanner.scan — may rewrite or flag.

        Phase 5 shadow mode delegates step 2 to :class:`ShadowRunner` so a
        legacy gateway and the new LiteLLM gateway run side-by-side.
        """
        ctx = current_context()
        try:
            scanned_system, scanned_user = system_prompt, user_payload
            if self._input_safety is not None:
                scanned_system, scanned_user = self._input_safety.scan(
                    system_prompt, user_payload, task, ctx,
                )
            request = GatewayRequest(
                task=task,
                model_name=self.route(task, risk_level=risk_level, complexity=complexity),
                system_prompt=scanned_system,
                user_payload=scanned_user,
                metadata={"risk_level": risk_level.value, "complexity": complexity},
                trace_id=ctx.trace_id,
            )
            response = self._invoke_gateway(request)
        except SafetyBlocked as exc:
            return GatewayResponse(
                text=exc.canned_reply,
                model_used="blocked",
                usage=Usage(),
            )
        if self._output_safety is not None:
            text, flagged = self._output_safety.scan(
                response.text, task, ctx,
            )
            if text != response.text or flagged:
                response = GatewayResponse(
                    text=text,
                    model_used=response.model_used,
                    usage=response.usage,
                    cost_usd=response.cost_usd,
                )
        self._record_to_sink(ctx, request, response)
        return response

    def _record_to_sink(self, ctx: object, request: GatewayRequest, response: GatewayResponse) -> None:
        """Fire-and-forget: push trace / safety-span / generation events to the sink.

        Generations are only recorded when the gateway is NOT LiteLLM because
        the LiteLLM ``success_callback`` already logs the provider call; app-side
        spans enrich that trace without duplicating the generation entry.
        """
        if self._sink is None or not ctx.trace_id:  # type: ignore[union-attr]
            return
        # Trace metadata groups all spans for this request under one trace_id.
        self._sink.record_trace(ctx.trace_id, {  # type: ignore[union-attr]
            "name": ctx.purpose.value if ctx.purpose else "request",  # type: ignore[union-attr]
            "tenant_id": ctx.tenant_id or None,  # type: ignore[union-attr]
            "purpose": ctx.purpose.value if ctx.purpose else None,  # type: ignore[union-attr]
        })
        # Input-safety span: attach scanner results when the safety gate ran.
        if ctx.safety_results:  # type: ignore[union-attr]
            self._sink.record_span(ctx.trace_id, "input_safety", {  # type: ignore[union-attr]
                "passed": all(r.passed for r in ctx.safety_results),  # type: ignore[union-attr]
                "scanner_count": len(ctx.safety_results),  # type: ignore[union-attr]
            })
        # Output-safety span: record when the output gate ran and flagged content.
        if self._output_safety is not None:
            self._sink.record_span(ctx.trace_id, "output_safety", {  # type: ignore[union-attr]
                "flagged": ctx.output_flagged,  # type: ignore[union-attr]
            })
        # Generation: only on non-LiteLLM paths to avoid duplicate entries
        # (LiteLLM already records the provider call via success_callback).
        is_litellm = self._flags is not None and self._flags.llm_gateway == "litellm"
        if not is_litellm and response.model_used != "blocked":
            self._sink.record_generation(
                trace_id=ctx.trace_id,  # type: ignore[union-attr]
                model=response.model_used,
                prompt=f"[system]\n{request.system_prompt[:400]}\n[user]\n{request.user_payload[:400]}",
                completion=response.text,
                privacy_flags=ctx.privacy_flags,  # type: ignore[union-attr]
                usage={
                    "input_tokens": response.usage.input_tokens,
                    "output_tokens": response.usage.output_tokens,
                },
                cost_usd=response.cost_usd,
            )

    def _invoke_gateway(self, request: GatewayRequest) -> GatewayResponse:
        """Call the primary gateway, optionally side-by-side with legacy."""
        if self._legacy_gateway is None:
            return self._gateway.complete(request)
        from .shadow.runner import ShadowRunner  # local import avoids cycle
        return ShadowRunner(
            primary=self._gateway,
            legacy=self._legacy_gateway,
            flags=self._flags,
        ).complete(request)


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
    # RemoteDisconnected is raised by http.client during getresponse() (after the
    # request is sent) when the server closes the connection without sending any
    # HTTP response.  urllib.request only wraps OSError raised during the send
    # phase, so RemoteDisconnected from the read phase escapes as a raw exception
    # unless we convert it here.
    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            return response.read()
    except http.client.RemoteDisconnected as exc:
        raise urllib.error.URLError(exc) from exc


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
