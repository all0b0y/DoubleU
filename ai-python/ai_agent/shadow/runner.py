"""Shadow runner: runs legacy + new gateway side-by-side and records the diff.

Phase 5 wires this runner into :class:`ai_agent.llm.ModelRouter` whenever a
legacy gateway is configured.  Behaviour is governed by feature flags:

* ``shadow_mode_only=True``     → both gateways run, **legacy** response is
  returned to the caller.  Zero behavioural change for users; full Langfuse
  comparison data collected.
* ``shadow_mode_only=False``    + ``cutover=False`` → both gateways run, the
  **new (LiteLLM)** response is returned.  Legacy is observed only.
* ``cutover=True``              → legacy is skipped; only the new gateway
  runs.  Final state.
"""

from __future__ import annotations

import logging

from ai_agent.config.flags import FeatureFlags
from ai_agent.observability.context import current_context
from ai_agent.schemas import GatewayRequest, GatewayResponse, ModelTask

from .diff import gateway_diff
from . import metrics

logger = logging.getLogger(__name__)


_PURPOSE_BY_TASK = {
    ModelTask.DRAFT_REPLY: "draft",
    ModelTask.EXTRACT_FACTS: "extract",
    ModelTask.SUMMARIZE_CHAT: "summarize",
}


class ShadowRunner:
    """Wraps a primary + legacy gateway to drive side-by-side shadow traffic."""

    def __init__(
        self,
        primary,
        legacy,
        flags: FeatureFlags | None,
    ) -> None:
        """Create the runner.  ``flags`` controls the cutover semantics."""
        self._primary = primary
        self._legacy = legacy
        self._flags = flags or FeatureFlags()

    def complete(self, request: GatewayRequest) -> GatewayResponse:
        """Run primary (+ optional legacy) and return the response per flags."""
        if self._flags.cutover:
            return self._primary.complete(request)

        ctx = current_context()
        purpose_value = _PURPOSE_BY_TASK.get(request.task, "draft")

        # Always call the primary; legacy result is only used for divergence.
        primary_response = self._primary.complete(request)

        try:
            legacy_response = self._legacy.complete(request)
        except Exception as exc:  # noqa: BLE001 - legacy may be flaky during cutover
            logger.warning("shadow legacy gateway failed: %s", exc)
            return primary_response

        # Compute and record divergence; this is informational only.
        from ai_agent.contract_enums import LLMPurpose
        try:
            purpose_enum = LLMPurpose(purpose_value)
        except ValueError:
            purpose_enum = LLMPurpose.DRAFT
        diff = gateway_diff(legacy_response.text, primary_response.text, purpose_enum)
        metrics.record(purpose_enum, diff.classification)
        logger.info(
            "shadow_diff",
            extra={
                "trace_id": ctx.trace_id,
                "purpose": purpose_value,
                "classification": diff.classification,
                "score": diff.score,
            },
        )

        if self._flags.shadow_mode_only:
            return legacy_response
        return primary_response
