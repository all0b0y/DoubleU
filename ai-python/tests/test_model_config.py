"""Cross-file checks for env-driven LiteLLM model configuration."""

from __future__ import annotations

import pathlib
import importlib.util

from pydantic_ai.models.test import TestModel

from ai_agent.agents import models as agent_models


ROOT_DIR = pathlib.Path(__file__).resolve().parents[2]
RENDERER_PATH = ROOT_DIR / "scripts/render_litellm_config.py"
spec = importlib.util.spec_from_file_location("render_litellm_config", RENDERER_PATH)
assert spec and spec.loader
renderer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(renderer)


def _model_env_from_example() -> dict[str, str]:
    values: dict[str, str] = {}
    for line in (ROOT_DIR / ".env.example").read_text(encoding="utf-8").splitlines():
        if "=" not in line:
            continue
        name, value = line.split("=", 1)
        if name in renderer.MODEL_ENV_NAMES:
            values[name] = value
    return values


def test_litellm_config_is_rendered_from_model_env() -> None:
    """Every provider model in env must become an exact LiteLLM model entry."""
    base = (ROOT_DIR / "infra/litellm/config.yaml").read_text(encoding="utf-8")
    environ = _model_env_from_example()
    rendered = renderer.render_config(base, environ)
    for model in renderer.models_from_env(environ):
        assert f'- model_name: "{model}"' in rendered
        assert f'      model: "{model}"' in rendered


def test_litellm_renderer_deduplicates_and_preserves_aliases() -> None:
    environ = {
        "MODEL_DRAFT": "draft-primary",
        "MODEL_TIER_FAST": "deepseek/model-a, openrouter/deepseek/model-a",
    }
    assert renderer.models_from_env(environ) == ("openrouter/deepseek/model-a",)


def test_build_named_model_promotes_selected_model(monkeypatch) -> None:
    built: list[str] = []

    monkeypatch.setenv("MOCK_LLM", "false")
    monkeypatch.delenv("LLM_GATEWAY", raising=False)
    monkeypatch.setenv("MODEL_TIER_SMART", "model-a, model-b, model-c")
    monkeypatch.setattr(
        agent_models,
        "_build_openrouter_model",
        lambda slug: built.append(slug) or TestModel(),
    )

    assert agent_models.build_named_model("smart", "model-b") is not None

    assert built == ["model-b", "model-a", "model-c"]
    assert agent_models.model_name_for_tier("smart", "model-b") == "model-b"


def test_build_named_model_degrades_invalid_name_to_tier_default(monkeypatch) -> None:
    built: list[str] = []

    monkeypatch.setenv("MOCK_LLM", "false")
    monkeypatch.delenv("LLM_GATEWAY", raising=False)
    monkeypatch.setenv("MODEL_TIER_FAST", "model-a, model-b")
    monkeypatch.setattr(
        agent_models,
        "_build_openrouter_model",
        lambda slug: built.append(slug) or TestModel(),
    )

    assert agent_models.build_named_model("fast", "stale-model") is not None

    assert built == ["model-a", "model-b"]
    assert agent_models.model_name_for_tier("fast", "stale-model") == "model-a"


def test_build_named_model_degrades_to_none_for_mock_llm(monkeypatch) -> None:
    monkeypatch.setenv("MOCK_LLM", "true")
    monkeypatch.setenv("MODEL_TIER_BALANCED", "model-a, model-b")

    assert agent_models.build_named_model("balanced", "model-b") is None
