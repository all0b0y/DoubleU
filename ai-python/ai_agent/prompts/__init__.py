"""Structured prompting: Prompt Contract (``PromptSpec``) + model-family profiles.

Single source of truth for agent system prompts, rendered per model family
instead of one Russian "blob" shipped unchanged to every model. Behind the
``PROMPT_CONTRACT_ENABLED`` feature flag (default off) — see
``agents/runtime.py`` and ``pipelines/draft.py`` for the integration points.
"""

from __future__ import annotations

from . import library
from .families import ModelFamily, resolve_family
from .profiles import ModelProfile, profile_for
from .renderers import render
from .spec import PromptSpec

__all__ = [
    "ModelFamily",
    "ModelProfile",
    "PromptSpec",
    "library",
    "profile_for",
    "render",
    "resolve_family",
]
