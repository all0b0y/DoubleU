"""Provider-agnostic model capability registry.

Separates two distinct questions that used to be conflated in
``prompts.profiles.ModelProfile``:

* **prompt_shape_family** (``prompts.families.resolve_family``) — HOW to
  render the system prompt (gpt/reasoning/agentic section structure). Driven
  entirely by env patterns; unchanged by this module.
* **model capabilities** (this module) — WHAT the provider/gateway actually
  accepts for a concrete slug: tool calling, forced ``tool_choice``, parallel
  tool calls, structured outputs, reasoning parameters, context length, and
  whether the provider is known to emit malformed tool-call JSON
  (``json_tool_reliability``).

Source of truth, in priority order, for each field:

1. A local env override for a known provider quirk (e.g.
   ``MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES``) — for a specific
   ``slug`` a deployment has observed misbehaving, regardless of what its
   family/metadata would otherwise predict. No model names are hardcoded in
   code; overrides are purely env-driven, matching ``MODEL_FAMILY_OVERRIDES``.
2. Pattern-based defaults derived from ``prompt_shape_family`` — every
   deployment without overrides still gets sane, conservative defaults.

``parse_openrouter_model`` is a separate, independently-tested pure parser
for OpenRouter ``/models``-shaped metadata (``id``, ``canonical_slug``,
``architecture``, ``supported_parameters``, ``context_length``,
``top_provider``). It is NOT wired into ``capabilities_for``'s runtime path —
fetching and caching that metadata is a stateful, network-dependent concern
or a build-time step (e.g. alongside ``scripts/render_litellm_config.py``)
out of scope here. The parser exists so that integration is a matter of
supplying real data to an already-tested function, not new capability logic.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field, replace
from typing import Any, Literal

from ..prompts.families import ModelFamily, resolve_family

JsonToolReliability = Literal["unknown", "ok", "degraded", "blocked"]

# Bump when the pattern-based defaults or override parsing below change in a
# way that could shift a routing decision — written into Langfuse trace
# metadata (_record_run) so "why did this model suddenly route differently"
# is answerable from the trace alone, without git-blaming this file.
CAPABILITY_PROFILE_VERSION = "1"

_JSON_TOOL_RELIABILITY_VALUES: tuple[JsonToolReliability, ...] = (
    "unknown", "ok", "degraded", "blocked",
)


@dataclass(frozen=True)
class ModelCapabilities:
    """What a concrete model slug actually supports, independent of prompt shape."""

    prompt_shape_family: ModelFamily
    supports_tools: bool
    # Whether the gateway can send a FORCED tool_choice (pydantic-ai's
    # ToolOutput for final_result) without a 400 — the same distinction
    # ``ModelProfile.supports_forced_tool_choice`` used to carry.
    supports_tool_choice: bool
    supports_parallel_tool_calls: bool
    supports_structured_outputs: bool
    supports_reasoning: bool
    # True when the model always reasons and exposes no way to disable it
    # (e.g. deepseek-r1) — vs. an optionally-toggled reasoning mode.
    reasoning_mandatory: bool
    reasoning_default_enabled: bool
    supported_reasoning_efforts: tuple[str, ...] = field(default_factory=tuple)
    supports_reasoning_max_tokens: bool = False
    # None means unknown — never guessed/defaulted, only ever set from real
    # metadata (parse_openrouter_model) or an explicit override.
    context_length: int | None = None
    json_tool_reliability: JsonToolReliability = "unknown"

    @property
    def output_mode(self) -> Literal["prompted", "tool"]:
        """Mirrors ``ModelProfile.output_mode`` — derived, not stored."""
        return "tool" if self.supports_tool_choice else "prompted"


# Pattern-based defaults per prompt-shape family. Conservative on purpose:
# unknown/absent capabilities default to the safer (False / "unknown") value
# rather than an optimistic guess.
_FAMILY_DEFAULTS: dict[ModelFamily, ModelCapabilities] = {
    ModelFamily.GPT: ModelCapabilities(
        prompt_shape_family=ModelFamily.GPT,
        supports_tools=True,
        supports_tool_choice=True,
        supports_parallel_tool_calls=True,
        supports_structured_outputs=True,
        supports_reasoning=False,
        reasoning_mandatory=False,
        reasoning_default_enabled=False,
    ),
    ModelFamily.REASONING: ModelCapabilities(
        prompt_shape_family=ModelFamily.REASONING,
        supports_tools=True,
        # Reasoning models reject the forced tool_choice ToolOutput sends for
        # final_result (400 — see doubleu-thinking-models-promptedoutput).
        supports_tool_choice=False,
        supports_parallel_tool_calls=False,
        supports_structured_outputs=False,
        supports_reasoning=True,
        reasoning_mandatory=True,
        reasoning_default_enabled=True,
    ),
    ModelFamily.AGENTIC: ModelCapabilities(
        prompt_shape_family=ModelFamily.AGENTIC,
        supports_tools=True,
        supports_tool_choice=True,
        supports_parallel_tool_calls=True,
        supports_structured_outputs=True,
        supports_reasoning=False,
        reasoning_mandatory=False,
        reasoning_default_enabled=False,
    ),
}

_OUTPUT_MODE_ENV: dict[ModelFamily, str] = {
    ModelFamily.GPT: "PROMPT_OUTPUT_MODE_GPT",
    ModelFamily.REASONING: "PROMPT_OUTPUT_MODE_REASONING",
    ModelFamily.AGENTIC: "PROMPT_OUTPUT_MODE_AGENTIC",
}


def provider_for(model_slug: str) -> str:
    """Best-effort provider name from an OpenRouter-style ``provider/model`` slug.

    Returns ``""`` for a slug with no ``/`` (e.g. a bare LiteLLM alias) rather
    than guessing — this is a display/trace convenience, not a capability
    source.
    """
    slug = (model_slug or "").strip()
    provider, sep, _rest = slug.partition("/")
    return provider if sep else ""


def _json_tool_reliability_override(model_slug: str) -> JsonToolReliability | None:
    """Parse ``MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES=slug=value,...``.

    Generalises the narrower env this project shipped first (a bare
    comma-separated deny-list disabling tool-heavy routing for specific
    slugs) into the capability registry: a slug observed emitting malformed
    tool-call JSON or 400ing on tool-heavy params (``.claude/analyze.md``)
    gets ``json_tool_reliability=blocked`` here instead of its own ad-hoc
    mechanism. Malformed entries are skipped, never raised — same tolerance
    as ``MODEL_FAMILY_OVERRIDES``.
    """
    slug = (model_slug or "").strip().lower()
    if not slug:
        return None
    raw = os.getenv("MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES", "")
    for pair in (p.strip() for p in raw.split(",") if p.strip()):
        entry_slug, _, value = pair.partition("=")
        entry_slug = entry_slug.strip().lower()
        value = value.strip().lower()
        if entry_slug != slug or value not in _JSON_TOOL_RELIABILITY_VALUES:
            continue
        return value  # type: ignore[return-value]
    return None


def capabilities_for(model_slug: str) -> ModelCapabilities:
    """Resolve the capability profile for a concrete model slug.

    Pure resolver — does not change which output mode/tools any agent
    actually uses; callers decide whether to act on it (mirrors
    ``prompts.profiles.profile_for``'s contract, which this replaces).
    """
    family = resolve_family(model_slug)
    base = _FAMILY_DEFAULTS[family]

    output_mode_raw = os.getenv(_OUTPUT_MODE_ENV[family], "").strip().lower()
    supports_tool_choice = (
        output_mode_raw == "tool"
        if output_mode_raw in ("prompted", "tool")
        else base.supports_tool_choice
    )

    json_tool_reliability = (
        _json_tool_reliability_override(model_slug) or base.json_tool_reliability
    )

    return replace(
        base,
        supports_tool_choice=supports_tool_choice,
        json_tool_reliability=json_tool_reliability,
    )


# --------------------------------------------------------------- OpenRouter

# instruct_type / id substrings OpenRouter uses for reasoning-shaped models.
# Pattern-matched, not an exhaustive brand list — new reasoning models that
# follow the same naming convention are caught without a code change; ones
# that don't are caught via supported_parameters containing "reasoning"
# (checked first, see parse_openrouter_model).
_REASONING_INSTRUCT_PATTERNS = ("r1", "reasoning", "qwq", "thinking")


def parse_openrouter_model(data: dict[str, Any]) -> ModelCapabilities:
    """Parse one OpenRouter ``/models`` list entry into :class:`ModelCapabilities`.

    Pure function over the JSON shape OpenRouter documents (``id``,
    ``canonical_slug``, ``architecture.instruct_type``,
    ``supported_parameters``, ``context_length``, ``top_provider``) — no
    network access. ``prompt_shape_family`` resolution prefers the
    ``reasoning`` signal in ``supported_parameters``/``instruct_type``; for
    everything else it defers to ``resolve_family`` (env patterns), since
    OpenRouter's metadata has no equivalent explicit signal for the
    agentic/XML-friendly shape — that classification is an env-configured,
    operator-observed property, not something a model listing encodes.
    """
    slug = str(data.get("canonical_slug") or data.get("id") or "")
    supported_params = {str(p).lower() for p in (data.get("supported_parameters") or ())}
    architecture = data.get("architecture") or {}
    instruct_type = str(architecture.get("instruct_type") or "").lower()

    is_reasoning = "reasoning" in supported_params or any(
        pattern in instruct_type or pattern in slug.lower()
        for pattern in _REASONING_INSTRUCT_PATTERNS
    )
    family = ModelFamily.REASONING if is_reasoning else resolve_family(slug)

    supports_tools = "tools" in supported_params
    supports_tool_choice = "tool_choice" in supported_params
    supports_reasoning = is_reasoning or "include_reasoning" in supported_params
    reasoning_mandatory = is_reasoning and "include_reasoning" not in supported_params

    context_length = data.get("context_length")
    if context_length is None:
        top_provider = data.get("top_provider") or {}
        context_length = top_provider.get("context_length")
    if context_length is not None:
        try:
            context_length = int(context_length)
        except (TypeError, ValueError):
            context_length = None

    return ModelCapabilities(
        prompt_shape_family=family,
        supports_tools=supports_tools,
        supports_tool_choice=supports_tool_choice,
        supports_parallel_tool_calls=supports_tools and not is_reasoning,
        supports_structured_outputs=(
            "response_format" in supported_params or "structured_outputs" in supported_params
        ),
        supports_reasoning=supports_reasoning,
        reasoning_mandatory=reasoning_mandatory,
        reasoning_default_enabled=supports_reasoning,
        supported_reasoning_efforts=("low", "medium", "high") if supports_reasoning else (),
        supports_reasoning_max_tokens="reasoning_max_tokens" in supported_params,
        context_length=context_length,
        # OpenRouter listings don't carry observed reliability — only a
        # local override (or direct operational experience) ever sets this
        # away from "unknown".
        json_tool_reliability="unknown",
    )
