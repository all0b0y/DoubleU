"""Behavioural tests for :mod:`ai_agent.agents.model_capabilities`.

Two halves: ``capabilities_for`` (the runtime resolver — env override then
family-pattern defaults, replacing ``prompts.profiles.profile_for``) and
``parse_openrouter_model`` (a pure parser for OpenRouter ``/models``-shaped
metadata, exercised against representative fixtures per provider family —
not wired to a live fetch, see the module docstring for why).
"""

from __future__ import annotations

from ai_agent.agents.model_capabilities import (
    CAPABILITY_PROFILE_VERSION,
    ModelCapabilities,
    capabilities_for,
    parse_openrouter_model,
    provider_for,
)
from ai_agent.prompts.families import ModelFamily

_ENV_VARS = (
    "MODEL_FAMILY_DEFAULT",
    "MODEL_FAMILY_GPT_PATTERNS",
    "MODEL_FAMILY_REASONING_PATTERNS",
    "MODEL_FAMILY_AGENTIC_PATTERNS",
    "MODEL_FAMILY_OVERRIDES",
    "PROMPT_OUTPUT_MODE_GPT",
    "PROMPT_OUTPUT_MODE_REASONING",
    "PROMPT_OUTPUT_MODE_AGENTIC",
    "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES",
)


def _clear_env(monkeypatch) -> None:
    for name in _ENV_VARS:
        monkeypatch.delenv(name, raising=False)


def _pin_family(monkeypatch, family: str) -> None:
    monkeypatch.setenv("MODEL_FAMILY_DEFAULT", family)


class TestCapabilitiesForFamilyDefaults:
    """Mirrors the old test_prompt_profiles.py coverage for output_mode, plus
    the richer capability fields this registry adds."""

    def test_gpt_defaults_to_tool_choice_and_full_tool_support(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        _pin_family(monkeypatch, "gpt")
        c = capabilities_for("any-slug")
        assert c.prompt_shape_family == ModelFamily.GPT
        assert c.supports_tool_choice is True
        assert c.output_mode == "tool"
        assert c.supports_tools is True
        assert c.supports_structured_outputs is True
        assert c.supports_reasoning is False

    def test_reasoning_defaults_to_prompted_and_mandatory_reasoning(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        _pin_family(monkeypatch, "reasoning")
        c = capabilities_for("any-slug")
        assert c.prompt_shape_family == ModelFamily.REASONING
        assert c.supports_tool_choice is False
        assert c.output_mode == "prompted"
        assert c.supports_reasoning is True
        assert c.reasoning_mandatory is True
        assert c.reasoning_default_enabled is True

    def test_agentic_defaults_to_tool_choice_and_full_tool_support(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        _pin_family(monkeypatch, "agentic")
        c = capabilities_for("any-slug")
        assert c.prompt_shape_family == ModelFamily.AGENTIC
        assert c.supports_tool_choice is True
        assert c.output_mode == "tool"

    def test_env_override_flips_reasoning_to_tool_output(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        _pin_family(monkeypatch, "reasoning")
        monkeypatch.setenv("PROMPT_OUTPUT_MODE_REASONING", "tool")
        c = capabilities_for("any-slug")
        assert c.supports_tool_choice is True
        assert c.output_mode == "tool"

    def test_env_override_flips_gpt_to_prompted_output(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        _pin_family(monkeypatch, "gpt")
        monkeypatch.setenv("PROMPT_OUTPUT_MODE_GPT", "prompted")
        c = capabilities_for("any-slug")
        assert c.supports_tool_choice is False
        assert c.output_mode == "prompted"

    def test_invalid_output_mode_env_falls_back_to_family_default(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        _pin_family(monkeypatch, "reasoning")
        monkeypatch.setenv("PROMPT_OUTPUT_MODE_REASONING", "not-a-real-mode")
        c = capabilities_for("any-slug")
        assert c.supports_tool_choice is False

    def test_family_resolution_defers_to_resolve_family(self, monkeypatch) -> None:
        """capabilities_for must not re-derive family resolution itself."""
        _clear_env(monkeypatch)
        monkeypatch.setenv("MODEL_FAMILY_REASONING_PATTERNS", "qwen")
        c = capabilities_for("openrouter/qwen3-32b")
        assert c.prompt_shape_family == ModelFamily.REASONING

    def test_default_json_tool_reliability_is_unknown(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        c = capabilities_for("any-slug")
        assert c.json_tool_reliability == "unknown"


class TestJsonToolReliabilityOverride:
    """MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES replaces the narrower
    2.5 ORCHESTRATOR_TOOL_INCOMPATIBLE_SLUGS deny-list with a generalised,
    multi-value capability override."""

    def test_blocked_override_is_applied(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        monkeypatch.setenv(
            "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES", "bad-slug=blocked"
        )
        assert capabilities_for("bad-slug").json_tool_reliability == "blocked"
        assert capabilities_for("good-slug").json_tool_reliability == "unknown"

    def test_degraded_and_ok_values_are_applied(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        monkeypatch.setenv(
            "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES",
            "flaky-slug=degraded,trusted-slug=ok",
        )
        assert capabilities_for("flaky-slug").json_tool_reliability == "degraded"
        assert capabilities_for("trusted-slug").json_tool_reliability == "ok"

    def test_malformed_entries_are_skipped_not_raised(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        monkeypatch.setenv(
            "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES",
            "no-equals-sign,x=not-a-real-value,y=blocked",
        )
        assert capabilities_for("y").json_tool_reliability == "blocked"
        assert capabilities_for("x").json_tool_reliability == "unknown"

    def test_override_is_case_insensitive_on_slug_and_value(self, monkeypatch) -> None:
        _clear_env(monkeypatch)
        monkeypatch.setenv(
            "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES", "Bad-Slug=BLOCKED"
        )
        assert capabilities_for("bad-slug").json_tool_reliability == "blocked"


class TestProviderFor:
    def test_extracts_provider_prefix(self) -> None:
        assert provider_for("deepseek/deepseek-v4-flash") == "deepseek"
        assert provider_for("openai/gpt-4o") == "openai"

    def test_bare_alias_has_no_provider(self) -> None:
        assert provider_for("draft-primary") == ""
        assert provider_for("") == ""


def test_capability_profile_version_is_a_non_empty_string() -> None:
    assert isinstance(CAPABILITY_PROFILE_VERSION, str) and CAPABILITY_PROFILE_VERSION


class TestParseOpenrouterModel:
    """Representative fixtures per the plan's required provider coverage:
    deepseek/*, qwen/*, moonshotai/kimi/*, z-ai/glm/*, minimax/*, plus a
    generic OpenAI-shaped tool-calling model."""

    def test_deepseek_r1_resolves_to_mandatory_reasoning(self) -> None:
        data = {
            "id": "deepseek/deepseek-r1",
            "canonical_slug": "deepseek/deepseek-r1",
            "context_length": 64000,
            "architecture": {"instruct_type": "deepseek-r1", "tokenizer": "DeepSeek"},
            "top_provider": {"context_length": 64000},
            "supported_parameters": ["temperature", "top_p", "reasoning", "max_tokens"],
        }
        c = parse_openrouter_model(data)
        assert c.prompt_shape_family == ModelFamily.REASONING
        assert c.supports_reasoning is True
        assert c.reasoning_mandatory is True
        assert c.supported_reasoning_efforts == ("low", "medium", "high")
        assert c.context_length == 64000

    def test_deepseek_v3_chat_is_not_reasoning(self) -> None:
        data = {
            "id": "deepseek/deepseek-chat",
            "canonical_slug": "deepseek/deepseek-chat",
            "architecture": {"instruct_type": None, "tokenizer": "DeepSeek"},
            "supported_parameters": ["tools", "tool_choice", "temperature"],
        }
        c = parse_openrouter_model(data)
        assert c.supports_reasoning is False
        assert c.supports_tools is True
        assert c.supports_tool_choice is True

    def test_qwen3_thinking_variant_resolves_to_reasoning(self) -> None:
        data = {
            "id": "qwen/qwen3-235b-a22b-thinking-2507",
            "canonical_slug": "qwen/qwen3-235b-a22b-thinking-2507",
            "context_length": 262144,
            "architecture": {"instruct_type": "qwen3", "tokenizer": "Qwen"},
            "supported_parameters": ["reasoning", "tools", "temperature"],
        }
        c = parse_openrouter_model(data)
        assert c.prompt_shape_family == ModelFamily.REASONING
        assert c.supports_tools is True
        assert c.context_length == 262144

    def test_qwen_plain_instruct_is_not_reasoning(self) -> None:
        data = {
            "id": "qwen/qwen-2.5-72b-instruct",
            "canonical_slug": "qwen/qwen-2.5-72b-instruct",
            "architecture": {"instruct_type": "chatml", "tokenizer": "Qwen"},
            "supported_parameters": ["tools", "tool_choice", "temperature"],
        }
        c = parse_openrouter_model(data)
        assert c.supports_reasoning is False

    def test_moonshotai_kimi_resolves_via_tool_support(self) -> None:
        data = {
            "id": "moonshotai/kimi-k2",
            "canonical_slug": "moonshotai/kimi-k2",
            "architecture": {"instruct_type": None, "tokenizer": "Moonshot"},
            "supported_parameters": ["tools", "tool_choice", "temperature"],
        }
        c = parse_openrouter_model(data)
        assert c.supports_reasoning is False
        assert c.supports_tools is True
        assert c.supports_tool_choice is True

    def test_z_ai_glm_with_reasoning_param_resolves_to_reasoning(self) -> None:
        data = {
            "id": "z-ai/glm-4.6",
            "canonical_slug": "z-ai/glm-4.6",
            "architecture": {"instruct_type": "glm4", "tokenizer": "GLM"},
            "supported_parameters": ["tools", "reasoning", "temperature"],
        }
        c = parse_openrouter_model(data)
        assert c.prompt_shape_family == ModelFamily.REASONING
        assert c.supports_tools is True
        # No include_reasoning toggle present in this fixture -> treated as
        # always-on, matching the conservative default for a bare "reasoning"
        # supported_parameter with no documented opt-out.
        assert c.reasoning_mandatory is True

    def test_minimax_resolves_via_tool_support_no_reasoning(self) -> None:
        data = {
            "id": "minimax/minimax-m1",
            "canonical_slug": "minimax/minimax-m1",
            "architecture": {"instruct_type": None, "tokenizer": "MiniMax"},
            "supported_parameters": ["tools", "temperature"],
        }
        c = parse_openrouter_model(data)
        assert c.supports_reasoning is False
        assert c.supports_tools is True

    def test_generic_tool_calling_model_with_structured_outputs(self) -> None:
        data = {
            "id": "openai/gpt-4o",
            "canonical_slug": "openai/gpt-4o-2024-08-06",
            "context_length": 128000,
            "architecture": {"instruct_type": None, "tokenizer": "GPT"},
            "top_provider": {"context_length": 128000},
            "supported_parameters": [
                "tools", "tool_choice", "response_format", "temperature", "max_tokens",
            ],
        }
        c = parse_openrouter_model(data)
        assert c.supports_tools is True
        assert c.supports_tool_choice is True
        assert c.supports_structured_outputs is True
        assert c.supports_reasoning is False
        assert c.context_length == 128000

    def test_optional_reasoning_toggle_via_include_reasoning_is_not_mandatory(self) -> None:
        data = {
            "id": "some-provider/some-reasoning-model",
            "canonical_slug": "some-provider/some-reasoning-model",
            "architecture": {"instruct_type": "thinking", "tokenizer": "X"},
            "supported_parameters": ["reasoning", "include_reasoning", "tools"],
        }
        c = parse_openrouter_model(data)
        assert c.supports_reasoning is True
        assert c.reasoning_mandatory is False

    def test_missing_context_length_falls_back_to_top_provider(self) -> None:
        data = {
            "id": "vendor/model",
            "canonical_slug": "vendor/model",
            "architecture": {},
            "top_provider": {"context_length": 32000},
            "supported_parameters": [],
        }
        c = parse_openrouter_model(data)
        assert c.context_length == 32000

    def test_no_context_length_anywhere_is_none(self) -> None:
        data = {"id": "vendor/model", "canonical_slug": "vendor/model"}
        c = parse_openrouter_model(data)
        assert c.context_length is None

    def test_parsed_model_never_carries_observed_reliability(self) -> None:
        """OpenRouter listings don't encode operational reliability — only a
        local override (or direct experience) can move this off "unknown"."""
        data = {"id": "deepseek/deepseek-r1", "canonical_slug": "deepseek/deepseek-r1"}
        c = parse_openrouter_model(data)
        assert c.json_tool_reliability == "unknown"

    def test_returns_a_model_capabilities_instance(self) -> None:
        c = parse_openrouter_model({"id": "x", "canonical_slug": "x"})
        assert isinstance(c, ModelCapabilities)
