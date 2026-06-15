"""Model factory for the pydantic-ai agent layer.

Model independence is a configuration concern: every agent task resolves to an
OpenRouter-routed model name from the environment, so swapping OpenAI / Claude
/ Gemini / Qwen / DeepSeek is an env change, not a code change.  A configured
fallback model wraps the primary in :class:`FallbackModel`, replacing the
hand-rolled retry-with-fallback logic of the legacy OpenRouter gateway.

``MOCK_LLM=true`` swaps every model for :class:`TestModel`, which fabricates
schema-valid structured output — the agent equivalent of MockLLMGateway.
"""

from __future__ import annotations

import os
from enum import Enum

from pydantic_ai.models import Model
from pydantic_ai.models.fallback import FallbackModel
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.models.test import TestModel
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
    """Resolve the OpenRouter model name for a task from the environment."""
    name = os.getenv(_TASK_ENV[task], "").strip()
    if name:
        return name
    return os.getenv("OPENROUTER_MODEL", "").strip()


def build_model(task: AgentTask) -> Model:
    """Build the pydantic-ai model for one agent task.

    Raises RuntimeError when neither mock mode nor OpenRouter credentials are
    configured — mirrors the legacy UnconfiguredLLMGateway contract.
    """
    if os.getenv("MOCK_LLM", "true").lower() in ("1", "true", "yes", "on"):
        return TestModel()
    api_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    name = model_name_for(task)
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
