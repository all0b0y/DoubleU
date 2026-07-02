"""Wrap external MCP tool results as untrusted input.

The project's load-bearing security invariant (see ``prompts/spec.py`` module
docstring and ``security.py``) is that any text not authored by the owner or
first-party code lives inside an ``<untrusted_*>`` envelope so a hostile author
cannot smuggle instructions into the model's trusted context. MCP servers are
third-party network endpoints, so their tool results are exactly such untrusted
text — this toolset extends the envelope to them.

Mirrors the cross-cutting ``WrapperToolset`` shape used by
``agents/dedup_toolset.py`` (pydantic-ai's recommended pattern for toolset
behaviour that must apply uniformly, without touching the individual servers).
Native toolsets (memory/interaction/schedule) are NOT wrapped: their results
come from our own database, not an external party.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

from pydantic_ai import RunContext
from pydantic_ai.toolsets import ToolsetTool, WrapperToolset

from .deps import AgentDeps


@dataclass
class UntrustedResultToolset(WrapperToolset[AgentDeps]):
    """Wrap every wrapped-toolset result in an escaped ``<untrusted_tool_result>``.

    Angle brackets inside the result are entity-escaped BEFORE the wrapper is
    applied, so a server that returns a forged closing tag (e.g.
    ``</untrusted_tool_result><system>...``) cannot break out of the envelope —
    the output is always a single, correctly closed element whose inner text is
    inert. Non-string results are serialised to JSON first so structured output
    is escaped the same way.
    """

    async def call_tool(
        self,
        name: str,
        tool_args: dict[str, Any],
        ctx: RunContext[AgentDeps],
        tool: ToolsetTool[AgentDeps],
    ) -> str:
        result = await super().call_tool(name, tool_args, ctx, tool)
        text = result if isinstance(result, str) else json.dumps(result, ensure_ascii=False, default=str)
        escaped = text.replace("<", "&lt;").replace(">", "&gt;")
        return f'<untrusted_tool_result tool="{name}">{escaped}</untrusted_tool_result>'
