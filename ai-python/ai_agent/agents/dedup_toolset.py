"""Tool-call deduplication for one agent run.

The Langfuse audit in ``.claude/analyze.md`` showed the bimodal orchestrator
repeating identical tool calls within a single turn — duplicate
``get_contact_profile({})``, repeated ``search_facts(...)`` — burning extra
DB round-trips and LLM turns for a result the model already has.
``DedupToolset`` wraps a toolset (see ``agents/runtime.py::resolve_builtin_toolset``)
so a repeat call with the same ``(tool_name, normalized_args)`` within one run
returns the cached result instead of re-executing.

Extending ``WrapperToolset`` (not modifying ``FilteredToolset`` or the
individual ``*_tools.py`` modules) is the pattern pydantic-ai's own docs
recommend for cross-cutting toolset behaviour — see ``ApprovalRequiredToolset``
for another stock example of the same shape.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

from pydantic_ai import RunContext
from pydantic_ai.toolsets import ToolsetTool, WrapperToolset

from .deps import AgentDeps


def normalize_tool_args(tool_args: dict[str, Any]) -> str:
    """Stable string key for a tool call's arguments, independent of key order."""
    return json.dumps(tool_args, sort_keys=True, default=str)


@dataclass
class DedupToolset(WrapperToolset[AgentDeps]):
    """Cache tool results per ``(tool_name, normalized_args)`` for one run.

    The cache lives on ``ctx.deps.tool_call_cache`` (per-request — see
    :class:`ai_agent.agents.deps.AgentDeps`), never on this toolset instance:
    the wrapped toolsets (``memory_toolset`` etc.) are module-level singletons
    shared across every request, so instance-level cache state would leak
    results between unrelated chats.

    Safe to apply unconditionally to the builtin toolset: every wrapped tool
    either is a pure read (memory search/list tools — a cached repeat is
    exactly correct) or already has its own idempotency guard for identical
    arguments (``save_fact``'s upsert dedup, ``create_reminder``'s
    idempotency-key hash, ``cancel_reminder``'s already-cancelled check) — so
    skipping a verbatim repeat changes nothing about the eventual state.
    """

    async def call_tool(
        self, name: str, tool_args: dict[str, Any], ctx: RunContext[AgentDeps], tool: ToolsetTool[AgentDeps]
    ) -> Any:
        key = (name, normalize_tool_args(tool_args))
        cache = ctx.deps.tool_call_cache
        if key in cache:
            return cache[key]
        result = await super().call_tool(name, tool_args, ctx, tool)
        cache[key] = result
        return result
