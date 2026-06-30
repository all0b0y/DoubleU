#!/usr/bin/env python3
"""Render LiteLLM's concrete model allow-list from application env settings.

The application accepts bare OpenRouter slugs (``provider/model``), while the
LiteLLM proxy requires every callable model to exist in ``model_list``. This
renderer keeps the env as the single source of truth and injects exact
``openrouter/provider/model`` entries into the static base config at startup.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
from collections.abc import Mapping


MODEL_ENV_NAMES = (
    "OPENROUTER_MODEL",
    "OPENROUTER_FALLBACK_MODEL",
    "MODEL_DRAFT",
    "MODEL_CRITIC",
    "MODEL_CHEAP",
    "MODEL_TIER_FAST",
    "MODEL_TIER_BALANCED",
    "MODEL_TIER_SMART",
)

_MODEL_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]*$")
_MODEL_LIST_MARKER = "model_list:\n"
_FALLBACKS_MARKER = "  fallbacks:\n"
# Stable, non-thinking, tool-capable backup every env model falls back to when its
# OpenRouter sub-providers rate-limit (429) or reject (400) the request — already a
# model_name in the static base config. Without this the orchestrator/writer raw
# slugs (e.g. openrouter/deepseek/deepseek-v4-flash) get "No fallback model group
# found" and one bad provider fails the whole turn.
_DEFAULT_FALLBACK = "draft-fallback"


def models_from_env(environ: Mapping[str, str]) -> tuple[str, ...]:
    """Return sorted, normalized provider models referenced by application env."""
    models: set[str] = set()
    for env_name in MODEL_ENV_NAMES:
        for raw_name in environ.get(env_name, "").split(","):
            name = raw_name.strip()
            if not name or "/" not in name:
                # Names without a slash are semantic aliases already declared
                # in the static config (for example ``draft-primary``).
                continue
            normalized = name if name.startswith("openrouter/") else f"openrouter/{name}"
            if not _MODEL_NAME_RE.fullmatch(normalized):
                raise ValueError(f"invalid model name in {env_name}: {name!r}")
            models.add(normalized)
    return tuple(sorted(models))


def render_config(base_config: str, environ: Mapping[str, str]) -> str:
    """Inject env-derived models and their fallbacks into the base config.

    Each concrete model is added to ``model_list`` and given a router fallback to
    ``_DEFAULT_FALLBACK`` so a 429/400 from one OpenRouter sub-provider routes to a
    stable backup instead of failing the turn.
    """
    if base_config.count(_MODEL_LIST_MARKER) != 1:
        raise ValueError("base LiteLLM config must contain exactly one model_list section")
    if base_config.count(_FALLBACKS_MARKER) != 1:
        raise ValueError("base LiteLLM config must contain exactly one router fallbacks section")
    env_models = models_from_env(environ)
    entries = []
    fallbacks = []
    for name in env_models:
        quoted = json.dumps(name)
        entries.append(
            "  - model_name: " + quoted + "\n"
            "    litellm_params:\n"
            "      model: " + quoted + "\n"
            "      api_key: os.environ/OPENROUTER_API_KEY\n"
        )
        # The backup must differ from the model itself (no self-loop). _DEFAULT_FALLBACK
        # is an alias, never an env slug, so this always holds here.
        fallbacks.append("    - " + quoted + ': ["' + _DEFAULT_FALLBACK + '"]\n')
    rendered = base_config.replace(_MODEL_LIST_MARKER, _MODEL_LIST_MARKER + "".join(entries), 1)
    return rendered.replace(_FALLBACKS_MARKER, _FALLBACKS_MARKER + "".join(fallbacks), 1)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", required=True, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    args = parser.parse_args()
    rendered = render_config(args.base.read_text(encoding="utf-8"), os.environ)
    args.output.write_text(rendered, encoding="utf-8")
    print(f"rendered LiteLLM config with {len(models_from_env(os.environ))} env models")


if __name__ == "__main__":
    main()
