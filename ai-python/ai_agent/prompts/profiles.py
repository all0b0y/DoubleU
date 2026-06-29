"""Model capability profile: what a model family is safe to ask for structured output.

Generalises the hack documented in ``doubleu-thinking-models-promptedoutput``:
the draft agent had to move to ``PromptedOutput`` because reasoning models
(qwen3, minimax) reject the forced ``tool_choice=required`` pydantic-ai's
default ``ToolOutput`` sends for the ``final_result`` call.  ``ModelProfile``
makes that distinction explicit and queryable instead of leaving it as a
single special-cased agent.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Literal

from .families import ModelFamily, resolve_family

OutputMode = Literal["prompted", "tool"]

# Default output mode per family when its PROMPT_OUTPUT_MODE_<FAMILY> env is
# unset: reasoning models default to "prompted" (no forced tool_choice);
# gpt/agentic default to "tool" (native structured output).
_DEFAULT_OUTPUT_MODE: dict[ModelFamily, OutputMode] = {
    ModelFamily.GPT: "tool",
    ModelFamily.REASONING: "prompted",
    ModelFamily.AGENTIC: "tool",
}

_OUTPUT_MODE_ENV: dict[ModelFamily, str] = {
    ModelFamily.GPT: "PROMPT_OUTPUT_MODE_GPT",
    ModelFamily.REASONING: "PROMPT_OUTPUT_MODE_REASONING",
    ModelFamily.AGENTIC: "PROMPT_OUTPUT_MODE_AGENTIC",
}


@dataclass(frozen=True)
class ModelProfile:
    """Capability profile for one resolved :class:`ModelFamily`."""

    family: ModelFamily
    output_mode: OutputMode
    # False for reasoning models: they 400 on the tool_choice ToolOutput
    # forces for the final_result call. Agents that stay on ToolOutput
    # despite this should log a guardrail warning rather than crash.
    supports_forced_tool_choice: bool


def profile_for(model_slug: str) -> ModelProfile:
    """Resolve the capability profile for a concrete model slug.

    ``output_mode`` is read from ``PROMPT_OUTPUT_MODE_<FAMILY>`` (e.g.
    ``PROMPT_OUTPUT_MODE_REASONING``); an unset or invalid value falls back to
    the family default. This is a pure resolver — it does not change which
    output mode any agent actually uses; callers decide whether to act on it
    (see the guardrail in ``agents/runtime.py``).
    """
    family = resolve_family(model_slug)
    default_mode = _DEFAULT_OUTPUT_MODE[family]
    raw = os.getenv(_OUTPUT_MODE_ENV[family], "").strip().lower()
    output_mode: OutputMode = raw if raw in ("prompted", "tool") else default_mode
    return ModelProfile(
        family=family,
        output_mode=output_mode,
        supports_forced_tool_choice=(output_mode == "tool"),
    )
