"""Shared dependency container injected into every agent run.

Carries the per-request scope (tenant/chat), the privacy flags that every
memory tool must enforce, and the optional asyncpg pool that unlocks direct
PostgreSQL memory access.  Privacy is enforced INSIDE the tools (second line
of defence after the Go backend) so no prompt can talk an agent into reading
or writing memory for a ``no_memory`` chat.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Awaitable, Callable, Sequence

import asyncpg

from .results import ResponsePlan


@dataclass
class AgentDeps:
    """Per-run dependencies for the agent toolset."""

    tenant_id: str
    chat_id: str
    privacy_flags: tuple[str, ...] = ()
    source_message_id: str = ""
    # System instruction assembled per request (role, profile, owner style);
    # surfaced to the agent through a dynamic-instructions hook.
    system_instruction: str = ""
    # pool is None when the service runs stateless (AI_MEMORY_SOURCE=go):
    # memory tools then degrade to empty results / no-op writes.
    pool: asyncpg.Pool | None = None
    # embedder returns the query embedding for semantic search, or None when
    # no embedding transport is configured (tools fall back to text search).
    embedder: Callable[[str], Awaitable[Sequence[float] | None]] | None = None
    # Lines the memory tools should treat as already-provided context
    # (prefetched by the Go backend) — used only for diagnostics/logging.
    prefetched_context: tuple[str, ...] = field(default_factory=tuple)
    # Interaction tools write a deferred contact-facing form here. The tool
    # itself never performs transport IO; core sends it through approval.
    response_plan: ResponsePlan | None = None
    # One stable clock value shared by the scheduler prompt and tool validation.
    current_time: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    @property
    def no_memory(self) -> bool:
        """True when the chat forbids reading or writing long-term memory."""
        return "no_memory" in self.privacy_flags

    @property
    def no_summary(self) -> bool:
        """True when the chat forbids summarization."""
        return "no_summary" in self.privacy_flags

    @property
    def private(self) -> bool:
        """True when autonomous scheduled persistence is forbidden."""
        return "private" in self.privacy_flags
