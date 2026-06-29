"""Behavioural tests for :func:`ai_agent.prompts.profiles.profile_for`.

Generalises the existing PromptedOutput-vs-ToolOutput split (see
``doubleu-thinking-models-promptedoutput``): reasoning models default to
``"prompted"`` (no forced tool_choice), gpt/agentic default to ``"tool"``.
``PROMPT_OUTPUT_MODE_<FAMILY>`` overrides the default per family; an absent
or invalid value degrades to that family's default rather than raising.
"""

from __future__ import annotations

from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.profiles import profile_for

_ENV_VARS = (
    "MODEL_FAMILY_DEFAULT",
    "MODEL_FAMILY_GPT_PATTERNS",
    "MODEL_FAMILY_REASONING_PATTERNS",
    "MODEL_FAMILY_AGENTIC_PATTERNS",
    "MODEL_FAMILY_OVERRIDES",
    "PROMPT_OUTPUT_MODE_GPT",
    "PROMPT_OUTPUT_MODE_REASONING",
    "PROMPT_OUTPUT_MODE_AGENTIC",
)


def _clear_env(monkeypatch) -> None:
    for name in _ENV_VARS:
        monkeypatch.delenv(name, raising=False)


def _pin_family(monkeypatch, family: str) -> None:
    monkeypatch.setenv("MODEL_FAMILY_DEFAULT", family)


def test_gpt_defaults_to_tool_output(monkeypatch) -> None:
    _clear_env(monkeypatch)
    _pin_family(monkeypatch, "gpt")
    profile = profile_for("any-slug")
    assert profile.family == ModelFamily.GPT
    assert profile.output_mode == "tool"
    assert profile.supports_forced_tool_choice is True


def test_reasoning_defaults_to_prompted_output(monkeypatch) -> None:
    _clear_env(monkeypatch)
    _pin_family(monkeypatch, "reasoning")
    profile = profile_for("any-slug")
    assert profile.family == ModelFamily.REASONING
    assert profile.output_mode == "prompted"
    assert profile.supports_forced_tool_choice is False


def test_agentic_defaults_to_tool_output(monkeypatch) -> None:
    _clear_env(monkeypatch)
    _pin_family(monkeypatch, "agentic")
    profile = profile_for("any-slug")
    assert profile.family == ModelFamily.AGENTIC
    assert profile.output_mode == "tool"
    assert profile.supports_forced_tool_choice is True


def test_env_override_flips_reasoning_to_tool_output(monkeypatch) -> None:
    _clear_env(monkeypatch)
    _pin_family(monkeypatch, "reasoning")
    monkeypatch.setenv("PROMPT_OUTPUT_MODE_REASONING", "tool")
    profile = profile_for("any-slug")
    assert profile.output_mode == "tool"
    assert profile.supports_forced_tool_choice is True


def test_env_override_flips_gpt_to_prompted_output(monkeypatch) -> None:
    _clear_env(monkeypatch)
    _pin_family(monkeypatch, "gpt")
    monkeypatch.setenv("PROMPT_OUTPUT_MODE_GPT", "prompted")
    profile = profile_for("any-slug")
    assert profile.output_mode == "prompted"
    assert profile.supports_forced_tool_choice is False


def test_invalid_env_value_falls_back_to_family_default(monkeypatch) -> None:
    _clear_env(monkeypatch)
    _pin_family(monkeypatch, "reasoning")
    monkeypatch.setenv("PROMPT_OUTPUT_MODE_REASONING", "not-a-real-mode")
    profile = profile_for("any-slug")
    assert profile.output_mode == "prompted"


def test_profile_family_matches_resolve_family(monkeypatch) -> None:
    """profile_for must defer to resolve_family rather than re-deriving it."""
    _clear_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_REASONING_PATTERNS", "qwen")
    profile = profile_for("openrouter/qwen3-32b")
    assert profile.family == ModelFamily.REASONING
