"""Behavioural tests for :class:`ai_agent.agents.dedup_toolset.DedupToolset`.

Exercises ``call_tool`` directly against a counting stub wrapped toolset —
this is the same shape pydantic-ai itself calls at runtime (see
``WrapperToolset.call_tool`` in the installed ``.venv``), so a
duck-typed stub with just an async ``call_tool`` is a faithful double without
needing a full ``AbstractToolset`` implementation.
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

from ai_agent.agents.dedup_toolset import DedupToolset, normalize_tool_args
from ai_agent.agents.deps import AgentDeps


class _CountingToolset:
    """Records every call it receives; returns a result keyed by call count."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, dict]] = []

    async def call_tool(self, name, tool_args, ctx, tool):
        self.calls.append((name, dict(tool_args)))
        return f"result-{len(self.calls)}"


def _ctx(deps: AgentDeps) -> SimpleNamespace:
    return SimpleNamespace(deps=deps)


def test_repeat_call_with_identical_args_is_served_from_cache() -> None:
    stub = _CountingToolset()
    dedup = DedupToolset(wrapped=stub)
    deps = AgentDeps(tenant_id="t", chat_id="c")
    ctx = _ctx(deps)

    first = asyncio.run(dedup.call_tool("search_facts", {"query": "москва"}, ctx, tool=None))
    second = asyncio.run(dedup.call_tool("search_facts", {"query": "москва"}, ctx, tool=None))

    assert first == "result-1"
    assert second == "result-1"  # cached — NOT "result-2"
    assert len(stub.calls) == 1


def test_different_args_are_not_deduped() -> None:
    stub = _CountingToolset()
    dedup = DedupToolset(wrapped=stub)
    deps = AgentDeps(tenant_id="t", chat_id="c")
    ctx = _ctx(deps)

    first = asyncio.run(dedup.call_tool("search_facts", {"query": "москва"}, ctx, tool=None))
    second = asyncio.run(dedup.call_tool("search_facts", {"query": "питер"}, ctx, tool=None))

    assert first == "result-1"
    assert second == "result-2"
    assert len(stub.calls) == 2


def test_different_tool_names_are_not_deduped_even_with_same_args() -> None:
    stub = _CountingToolset()
    dedup = DedupToolset(wrapped=stub)
    ctx = _ctx(AgentDeps(tenant_id="t", chat_id="c"))

    asyncio.run(dedup.call_tool("get_contact_profile", {}, ctx, tool=None))
    asyncio.run(dedup.call_tool("list_reminders", {}, ctx, tool=None))

    assert len(stub.calls) == 2


def test_cache_is_per_request_not_shared_across_deps() -> None:
    """The cache lives on AgentDeps, never on the toolset instance — two
    different chats sharing the SAME DedupToolset/module-level toolset must
    never see each other's cached results."""
    stub = _CountingToolset()
    dedup = DedupToolset(wrapped=stub)
    chat_a = _ctx(AgentDeps(tenant_id="t", chat_id="a"))
    chat_b = _ctx(AgentDeps(tenant_id="t", chat_id="b"))

    asyncio.run(dedup.call_tool("get_contact_profile", {}, chat_a, tool=None))
    asyncio.run(dedup.call_tool("get_contact_profile", {}, chat_b, tool=None))

    assert len(stub.calls) == 2  # no cross-chat cache hit


def test_key_order_does_not_defeat_deduplication() -> None:
    stub = _CountingToolset()
    dedup = DedupToolset(wrapped=stub)
    ctx = _ctx(AgentDeps(tenant_id="t", chat_id="c"))

    asyncio.run(dedup.call_tool("create_reminder", {"target": "owner", "text": "x"}, ctx, tool=None))
    asyncio.run(dedup.call_tool("create_reminder", {"text": "x", "target": "owner"}, ctx, tool=None))

    assert len(stub.calls) == 1


def test_normalize_tool_args_is_order_independent() -> None:
    assert normalize_tool_args({"a": 1, "b": 2}) == normalize_tool_args({"b": 2, "a": 1})


def test_a_failed_first_call_is_not_cached() -> None:
    """The result is only cached after the wrapped call resolves, so a
    transient failure never poisons the cache and a later retry still runs
    for real."""

    class _FailsOnce:
        def __init__(self) -> None:
            self.attempts = 0

        async def call_tool(self, name, tool_args, ctx, tool):
            self.attempts += 1
            if self.attempts == 1:
                raise RuntimeError("transient failure")
            return "ok"

    stub = _FailsOnce()
    dedup = DedupToolset(wrapped=stub)
    ctx = _ctx(AgentDeps(tenant_id="t", chat_id="c"))

    async def run() -> str:
        try:
            await dedup.call_tool("search_facts", {"query": "x"}, ctx, tool=None)
        except RuntimeError:
            pass
        return await dedup.call_tool("search_facts", {"query": "x"}, ctx, tool=None)

    result = asyncio.run(run())

    assert result == "ok"
    assert stub.attempts == 2  # the failed first attempt was NOT cached
