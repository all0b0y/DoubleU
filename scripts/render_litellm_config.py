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
    """Inject concrete env-derived models immediately after ``model_list``."""
    if base_config.count(_MODEL_LIST_MARKER) != 1:
        raise ValueError("base LiteLLM config must contain exactly one model_list section")
    entries = []
    for name in models_from_env(environ):
        quoted = json.dumps(name)
        entries.append(
            "  - model_name: " + quoted + "\n"
            "    litellm_params:\n"
            "      model: " + quoted + "\n"
            "      api_key: os.environ/OPENROUTER_API_KEY\n"
        )
    return base_config.replace(_MODEL_LIST_MARKER, _MODEL_LIST_MARKER + "".join(entries), 1)


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
