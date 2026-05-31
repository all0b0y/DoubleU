"""Feature flags for the LLM Safety & Observability Pipeline.

Every flag defaults safe-off so the production runtime is unchanged until an
operator explicitly opts in via environment variables.  Tests pass explicit
booleans through the constructor and never read the environment.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Literal

GatewayKind = Literal["none", "litellm"]
ObservabilitySink = Literal["none", "langfuse"]


@dataclass(frozen=True)
class FeatureFlags:
    """Snapshot of the runtime feature flag state.

    Mirrors ``backend-go/internal/config/flags.go`` so Go and Python switch on
    the same env variables.  ``shadow_mode_only`` and ``cutover`` jointly drive
    Phase 5 cutover semantics (see docs/cutover-runbook.md).
    """

    safety_input_enabled: bool = False
    safety_output_enabled: bool = False
    llm_gateway: GatewayKind = "none"
    observability_sink: ObservabilitySink = "none"
    shadow_mode_only: bool = False
    cutover: bool = False


def _bool(value: str | None) -> bool:
    return (value or "").strip().lower() in ("1", "true", "yes", "on")


def _gateway(value: str | None) -> GatewayKind:
    raw = (value or "").strip().lower()
    if raw == "litellm":
        return "litellm"
    return "none"


def _sink(value: str | None) -> ObservabilitySink:
    raw = (value or "").strip().lower()
    if raw == "langfuse":
        return "langfuse"
    return "none"


def load_from_env() -> FeatureFlags:
    """Read FeatureFlags from environment variables."""
    return FeatureFlags(
        safety_input_enabled=_bool(os.getenv("SAFETY_INPUT_ENABLED")),
        safety_output_enabled=_bool(os.getenv("SAFETY_OUTPUT_ENABLED")),
        llm_gateway=_gateway(os.getenv("LLM_GATEWAY")),
        observability_sink=_sink(os.getenv("OBSERVABILITY_SINK")),
        shadow_mode_only=_bool(os.getenv("SHADOW_MODE_ONLY")),
        cutover=_bool(os.getenv("CUTOVER")),
    )
