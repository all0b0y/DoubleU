"""Model-family resolution: which prompt shape a model slug expects.

Sources agree the same instruction text should NOT be shipped unchanged to
every model: GPT-style chat models want explicit sections + a verification
checklist (no chain-of-thought prompting); reasoning/"thinking" models
(qwen3, minimax, deepseek-R) want the opposite — a minimal brief, because
asking them to "think step by step" fights their built-in reasoning and
forced tool_choice 400s (see ``doubleu-thinking-models-promptedoutput``);
Claude-style agentic models want an XML role/objective/constraints/tool_policy
shape.  ``resolve_family`` is the single place that maps a model slug to one
of these three shapes, driven entirely by env patterns — no model names are
hardcoded here.
"""

from __future__ import annotations

import os
from enum import Enum


class ModelFamily(str, Enum):
    """Prompt-shape family a model slug resolves to."""

    GPT = "gpt"
    REASONING = "reasoning"
    AGENTIC = "agentic"


def _csv(value: str) -> tuple[str, ...]:
    """Split a comma-separated env value the same way ``_tier_slugs`` does."""
    return tuple(part.strip() for part in value.split(",") if part.strip())


def _overrides(value: str) -> dict[str, ModelFamily]:
    """Parse ``slug=family[,slug=family...]`` pairs, skipping malformed entries."""
    overrides: dict[str, ModelFamily] = {}
    for pair in _csv(value):
        slug, _, family_raw = pair.partition("=")
        slug = slug.strip().lower()
        if not slug or not family_raw:
            continue
        try:
            overrides[slug] = ModelFamily(family_raw.strip().lower())
        except ValueError:
            continue
    return overrides


def resolve_family(model_slug: str) -> ModelFamily:
    """Resolve the prompt-shape family for a concrete model slug.

    Resolution order (first match wins):

    1. An exact ``slug=family`` pair in ``MODEL_FAMILY_OVERRIDES`` — highest
       priority, for pinning one specific model regardless of patterns.
    2. A case-insensitive substring match against ``MODEL_FAMILY_REASONING_
       PATTERNS``, then ``MODEL_FAMILY_AGENTIC_PATTERNS``, then
       ``MODEL_FAMILY_GPT_PATTERNS`` (checked in that order).
    3. ``MODEL_FAMILY_DEFAULT`` (default ``"gpt"``) when nothing matches —
       including when ``model_slug`` is empty (e.g. ``MOCK_LLM`` / unset env).

    All env values default to empty, so an unconfigured deployment resolves
    every model to ``MODEL_FAMILY_DEFAULT`` — no model name is hardcoded here.
    """
    slug = (model_slug or "").strip().lower()

    overrides = _overrides(os.getenv("MODEL_FAMILY_OVERRIDES", ""))
    if slug in overrides:
        return overrides[slug]

    if slug:
        for pattern in _csv(os.getenv("MODEL_FAMILY_REASONING_PATTERNS", "")):
            if pattern.lower() in slug:
                return ModelFamily.REASONING
        for pattern in _csv(os.getenv("MODEL_FAMILY_AGENTIC_PATTERNS", "")):
            if pattern.lower() in slug:
                return ModelFamily.AGENTIC
        for pattern in _csv(os.getenv("MODEL_FAMILY_GPT_PATTERNS", "")):
            if pattern.lower() in slug:
                return ModelFamily.GPT

    default_raw = os.getenv("MODEL_FAMILY_DEFAULT", "").strip().lower() or ModelFamily.GPT.value
    try:
        return ModelFamily(default_raw)
    except ValueError:
        return ModelFamily.GPT
