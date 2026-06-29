"""Behavioural tests for env-driven model-family resolution.

``resolve_family`` is pure env-in/enum-out, so these tests exercise the env
matrix directly rather than building any request/runtime fixtures: overrides
beat patterns, patterns beat the default, and everything degrades to
``MODEL_FAMILY_DEFAULT`` (default "gpt") when env is empty or invalid — no
model name is ever hardcoded in the resolver itself.
"""

from __future__ import annotations

from ai_agent.prompts.families import ModelFamily, resolve_family

_FAMILY_ENV_VARS = (
    "MODEL_FAMILY_DEFAULT",
    "MODEL_FAMILY_GPT_PATTERNS",
    "MODEL_FAMILY_REASONING_PATTERNS",
    "MODEL_FAMILY_AGENTIC_PATTERNS",
    "MODEL_FAMILY_OVERRIDES",
)


def _clear_family_env(monkeypatch) -> None:
    for name in _FAMILY_ENV_VARS:
        monkeypatch.delenv(name, raising=False)


def test_empty_env_defaults_to_gpt_for_any_slug(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    assert resolve_family("openrouter/anything-at-all") == ModelFamily.GPT
    assert resolve_family("") == ModelFamily.GPT


def test_default_env_overrides_the_builtin_default(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_DEFAULT", "reasoning")
    assert resolve_family("unmatched-slug") == ModelFamily.REASONING


def test_invalid_default_value_falls_back_to_gpt(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_DEFAULT", "not-a-real-family")
    assert resolve_family("unmatched-slug") == ModelFamily.GPT


def test_reasoning_pattern_matches_substring_case_insensitively(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_REASONING_PATTERNS", "qwen,minimax")
    assert resolve_family("openrouter/QWEN3-32b-thinking") == ModelFamily.REASONING
    assert resolve_family("openrouter/minimax-m3") == ModelFamily.REASONING
    # Non-matching slug still degrades to the default.
    assert resolve_family("openrouter/deepseek-v4-flash") == ModelFamily.GPT


def test_agentic_pattern_matches_substring(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_AGENTIC_PATTERNS", "claude")
    assert resolve_family("anthropic/claude-3.7-sonnet") == ModelFamily.AGENTIC


def test_gpt_pattern_wins_over_a_non_gpt_default(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_DEFAULT", "agentic")
    monkeypatch.setenv("MODEL_FAMILY_GPT_PATTERNS", "gpt-4")
    assert resolve_family("openai/gpt-4o") == ModelFamily.GPT
    # Still falls through to the (non-gpt) default when nothing matches.
    assert resolve_family("openai/o3") == ModelFamily.AGENTIC


def test_pattern_priority_reasoning_beats_agentic_beats_gpt(monkeypatch) -> None:
    """When several pattern lists match the same slug, reasoning is checked
    first, then agentic, then gpt — matching resolve_family's documented order."""
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_REASONING_PATTERNS", "shared")
    monkeypatch.setenv("MODEL_FAMILY_AGENTIC_PATTERNS", "shared")
    monkeypatch.setenv("MODEL_FAMILY_GPT_PATTERNS", "shared")
    assert resolve_family("vendor/shared-model") == ModelFamily.REASONING

    monkeypatch.delenv("MODEL_FAMILY_REASONING_PATTERNS", raising=False)
    assert resolve_family("vendor/shared-model") == ModelFamily.AGENTIC

    monkeypatch.delenv("MODEL_FAMILY_AGENTIC_PATTERNS", raising=False)
    assert resolve_family("vendor/shared-model") == ModelFamily.GPT


def test_exact_override_beats_a_matching_pattern(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_REASONING_PATTERNS", "qwen")
    monkeypatch.setenv("MODEL_FAMILY_OVERRIDES", "openrouter/qwen3-32b=gpt")
    # Without the override this slug would resolve to reasoning.
    assert resolve_family("openrouter/qwen3-32b-thinking") == ModelFamily.REASONING
    assert resolve_family("openrouter/qwen3-32b") == ModelFamily.GPT


def test_overrides_are_trimmed_and_case_insensitive(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_OVERRIDES", " Vendor/Model-X = AGENTIC , other=gpt ")
    assert resolve_family("vendor/model-x") == ModelFamily.AGENTIC
    assert resolve_family("VENDOR/MODEL-X") == ModelFamily.AGENTIC
    assert resolve_family("other") == ModelFamily.GPT


def test_malformed_override_entry_is_skipped_not_raised(monkeypatch) -> None:
    _clear_family_env(monkeypatch)
    monkeypatch.setenv("MODEL_FAMILY_OVERRIDES", "no-equals-sign,x=not-a-family,y=gpt")
    # Neither malformed entry crashes resolution; "y=gpt" still applies.
    assert resolve_family("y") == ModelFamily.GPT
    assert resolve_family("x") == ModelFamily.GPT  # invalid family value ignored → default
