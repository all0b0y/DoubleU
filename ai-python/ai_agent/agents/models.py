"""Model factory for the pydantic-ai agent layer.

Model independence is a configuration concern: every agent task resolves to an
OpenRouter-routed model name from the environment, so swapping OpenAI / Claude
/ Gemini / Qwen / DeepSeek is an env change, not a code change.  A configured
fallback model wraps the primary in :class:`FallbackModel`, replacing the
hand-rolled retry-with-fallback logic of the legacy OpenRouter gateway.

``MOCK_LLM=true`` swaps every model for :class:`TestModel`, which fabricates
schema-valid structured output — the agent equivalent of MockLLMGateway.

``LLM_GATEWAY=litellm`` routes all agent calls through the LiteLLM proxy
(``LITELLM_BASE_URL`` + ``LITELLM_MASTER_KEY``) instead of calling OpenRouter
directly.  LiteLLM requires an ``openrouter/`` prefix for OpenRouter-hosted
models; ``model_name_for`` returns the bare slug (e.g. ``deepseek/deepseek-v4-
flash``), so ``build_model`` prepends it automatically when routing via LiteLLM.
Per-task litellm aliases (``MODEL_DRAFT``, ``MODEL_CHEAP``) can be set to
pre-configured litellm model names (e.g. ``draft-primary``) to skip the prefix
logic and benefit from litellm-side fallbacks and rate-limiting.
"""

from __future__ import annotations

import os
from enum import Enum

from pydantic_ai.models import Model
from pydantic_ai.models.fallback import FallbackModel
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.models.test import TestModel
from pydantic_ai.providers.openai import OpenAIProvider
from pydantic_ai.providers.openrouter import OpenRouterProvider


class AgentTask(str, Enum):
    """Agent task → env-configured model binding."""

    DRAFT = "draft"
    CRITIC = "critic"
    EXTRACT = "extract"
    SUMMARIZE = "summarize"
    TOPIC = "topic"
    RERANK = "rerank"
    REFLECT = "reflect"


class ModelTier(str, Enum):
    """LLM quality tiers for per-role draft generation.

    Each tier maps to a comma-separated env list of model slugs.  The first
    slug is the primary; the rest are FallbackModel fallbacks tried in order.
    An absent or empty env var degrades to the DRAFT task model.
    """

    FAST = "fast"
    BALANCED = "balanced"
    SMART = "smart"


# Env variable for each tier's model list (comma-separated, first = primary).
_TIER_ENV: dict[ModelTier, str] = {
    ModelTier.FAST: "MODEL_TIER_FAST",
    ModelTier.BALANCED: "MODEL_TIER_BALANCED",
    ModelTier.SMART: "MODEL_TIER_SMART",
}


# Per-task env override and its fallback chain. Every task degrades to
# OPENROUTER_MODEL so a single-variable setup keeps working.  Topic tagging and
# reranking are cheap auxiliary calls, so they share the cheap model tier.
_TASK_ENV = {
    AgentTask.DRAFT: "MODEL_DRAFT",
    AgentTask.CRITIC: "MODEL_CRITIC",
    AgentTask.EXTRACT: "MODEL_CHEAP",
    AgentTask.SUMMARIZE: "MODEL_CHEAP",
    AgentTask.TOPIC: "MODEL_CHEAP",
    AgentTask.RERANK: "MODEL_CHEAP",
    AgentTask.REFLECT: "MODEL_CHEAP",
}


def model_name_for(task: AgentTask) -> str:
    """Resolve the model name for a task from the environment."""
    name = os.getenv(_TASK_ENV[task], "").strip()
    if name:
        return name
    return os.getenv("OPENROUTER_MODEL", "").strip()


def build_model(task: AgentTask) -> Model:
    """Build the pydantic-ai model for one agent task.

    Routes through LiteLLM when ``LLM_GATEWAY=litellm``; uses OpenRouter
    directly otherwise.  Raises RuntimeError when credentials are missing.
    """
    if os.getenv("MOCK_LLM", "true").lower() in ("1", "true", "yes", "on"):
        return TestModel()
    name = model_name_for(task)
    gateway = os.getenv("LLM_GATEWAY", "none").strip().lower()
    if gateway == "litellm":
        return _build_litellm_model(name)
    return _build_openrouter_model(name)


def build_tier_model(tier: str) -> Model | None:
    """Build the pydantic-ai model for a named tier (fast/balanced/smart).

    Parses the tier's env list (e.g. ``MODEL_TIER_FAST``), builds a
    :class:`FallbackModel` chain from all slugs using the same routing logic
    as :func:`build_model`, and returns the chain.

    Returns ``None`` when:
    - ``tier`` is not a recognised :class:`ModelTier` value,
    - the env var is absent or empty,
    - ``MOCK_LLM=true`` (the caller should use :func:`build_model` instead).

    The ``None`` return allows callers to fall back to the task-level model
    without extra branching.
    """
    if os.getenv("MOCK_LLM", "true").lower() in ("1", "true", "yes", "on"):
        return None
    try:
        tier_enum = ModelTier(tier.lower().strip())
    except ValueError:
        return None
    env_var = _TIER_ENV[tier_enum]
    raw = os.getenv(env_var, "").strip()
    if not raw:
        return None
    slugs = [s.strip() for s in raw.split(",") if s.strip()]
    if not slugs:
        return None
    gateway = os.getenv("LLM_GATEWAY", "none").strip().lower()
    builder = _build_litellm_model if gateway == "litellm" else _build_openrouter_model
    models = [builder(slug) for slug in slugs]
    if len(models) == 1:
        return models[0]
    return FallbackModel(*models)


def _build_litellm_model(name: str) -> Model:
    """Build an OpenAI-compatible model routed through the LiteLLM proxy.

    Model names that don't already carry a provider prefix get ``openrouter/``
    prepended so LiteLLM routes them through OpenRouter instead of
    auto-detecting api.deepseek.com / api.openai.com (which would fail because
    those provider API keys are typically unset in this stack).
    """
    litellm_url = os.getenv("LITELLM_BASE_URL", "").strip()
    litellm_key = os.getenv("LITELLM_MASTER_KEY", "").strip()
    if not litellm_url or not litellm_key or not name:
        raise RuntimeError(
            "LLM_GATEWAY=litellm but LITELLM_BASE_URL, LITELLM_MASTER_KEY or "
            "model name is not configured"
        )
    # Named litellm aliases (e.g. "draft-primary") contain no "/" — leave as-is.
    # Bare provider slugs (e.g. "deepseek/deepseek-v4-flash") need the prefix.
    litellm_name = name if (not name or "/" not in name or name.startswith("openrouter/")) else f"openrouter/{name}"
    provider = OpenAIProvider(base_url=litellm_url, api_key=litellm_key)
    return OpenAIChatModel(litellm_name, provider=provider)


def _build_openrouter_model(name: str) -> Model:
    """Build a model that calls OpenRouter directly."""
    api_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    if not api_key or not name:
        raise RuntimeError(
            "agent model unconfigured: set MOCK_LLM=true or OPENROUTER_API_KEY + OPENROUTER_MODEL"
        )
    provider = OpenRouterProvider(api_key=api_key)
    primary = OpenAIChatModel(name, provider=provider)
    fallback_name = os.getenv("OPENROUTER_FALLBACK_MODEL", "").strip()
    if fallback_name and fallback_name != name:
        return FallbackModel(primary, OpenAIChatModel(fallback_name, provider=provider))
    return primary
