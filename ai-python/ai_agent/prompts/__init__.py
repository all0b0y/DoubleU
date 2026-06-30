"""Structured prompting: Prompt Contract (``PromptSpec``) + model-family shape.

Single source of truth for agent system prompts: every agent task (draft,
orchestrator, critic, and the extraction/summary/topic/rerank/pattern/
role_draft auxiliary tasks) renders its :class:`PromptSpec` into the unified
``<prompt_contract>`` XML envelope shaped per model family, instead of one
"blob" shipped unchanged to every model — see ``agents/runtime.py`` for the
integration points and ``library.py`` for the per-task builders.

What a model/provider actually *supports* (tool calling, structured outputs,
reasoning params, ...) is a separate concern from how its prompt is shaped —
see :mod:`ai_agent.agents.model_capabilities`, not this package.
"""

from __future__ import annotations

from . import library
from .families import ModelFamily, resolve_family
from .renderers import render
from .spec import PromptSpec

__all__ = [
    "ModelFamily",
    "PromptSpec",
    "library",
    "render",
    "resolve_family",
]
