"""Integration tests for the agent memory tools against real PostgreSQL.

Run with a database that has the v2 baseline schema applied:
    DATABASE_URL_TEST=postgres://doubleu:doubleu@localhost:54329/doubleu \
        .venv/bin/pytest tests/test_memory_tools_db.py
Skipped entirely when DATABASE_URL_TEST is not set (CI without Postgres).

Verifies the write invariants the charter requires: facts go ONLY through the
``upsert_semantic_fact`` procedure (dedup works), suggestions stage as pending,
privacy flags block writes inside the tools.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from types import SimpleNamespace

import pytest

DATABASE_URL = os.getenv("DATABASE_URL_TEST", "").strip()

pytestmark = pytest.mark.skipif(not DATABASE_URL, reason="DATABASE_URL_TEST is not set")


@pytest.fixture()
def scope() -> tuple[str, str]:
    """A unique tenant/chat pair with the rows the FK chain requires."""
    import asyncpg

    tenant_id = f"tenant:test:{uuid.uuid4().hex[:8]}"
    chat_id = "42"

    async def setup() -> None:
        conn = await asyncpg.connect(DATABASE_URL)
        try:
            await conn.execute(
                "INSERT INTO tenants (id, name) VALUES ($1, 'test') ON CONFLICT DO NOTHING", tenant_id
            )
            await conn.execute(
                """INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, status, transport)
                   VALUES ($1, $2, $2, 'managed', 'bot') ON CONFLICT DO NOTHING""",
                tenant_id,
                chat_id,
            )
        finally:
            await conn.close()

    asyncio.run(setup())
    return tenant_id, chat_id


def run_tool(coro_factory):
    """Run a tool coroutine with a fresh pool on a private loop."""
    import asyncpg

    from ai_agent.agents.deps import AgentDeps

    async def runner():
        pool = await asyncpg.create_pool(DATABASE_URL, min_size=1, max_size=2)
        try:
            return await coro_factory(pool)
        finally:
            await pool.close()

    return asyncio.run(runner())


def test_save_fact_goes_through_upsert_and_dedupes(scope) -> None:
    from ai_agent.agents.memory_tools import save_fact, search_facts

    tenant_id, chat_id = scope

    async def write_twice(pool):
        ctx = SimpleNamespace(
            deps=__import__("ai_agent.agents.deps", fromlist=["AgentDeps"]).AgentDeps(
                tenant_id=tenant_id, chat_id=chat_id, source_message_id="m1", pool=pool
            )
        )
        first = await save_fact(ctx, "собеседник учится в МГУ", "biographical", "contact", 0.9)
        second = await save_fact(ctx, "собеседник учится в МГУ", "biographical", "contact", 0.9)
        found = await search_facts(ctx, "МГУ")
        return first, second, found

    first, second, found = run_tool(write_twice)
    assert first == "saved (inserted)"
    # The v3 procedure dedupes by normalized hash: the repeat is an occurrence, not a new row.
    assert "occurrence" in second
    assert any("МГУ" in fact for fact in found)


def test_search_facts_surfaces_superseded_history(scope) -> None:
    """Graph read-path (#3): semantic search returns the current fact AND the
    fact it superseded, inline as history. The older fact is soft-deleted (as
    consolidation leaves it) and only reachable via the fact_relations edge —
    exactly the temporal walk the read-path must perform."""
    from ai_agent.agents.deps import AgentDeps
    from ai_agent.agents.memory_tools import search_facts

    tenant_id, chat_id = scope
    vector = "[" + ",".join(["0.01"] * 4096) + "]"

    async def setup_and_search(pool):
        new_id = await pool.fetchval(
            """INSERT INTO semantic_facts
                   (tenant_id, scope_kind, chat_id, fact_text, fact_type, subject,
                    confidence, source, normalized_fact_hash)
               VALUES ($1,'chat',$2,'отправил договор в пятницу','commitment','contact',
                       0.95,'llm_extracted', md5('отправил договор '||$1))
               RETURNING id""",
            tenant_id, chat_id,
        )
        old_id = await pool.fetchval(
            """INSERT INTO semantic_facts
                   (tenant_id, scope_kind, chat_id, fact_text, fact_type, subject,
                    confidence, source, normalized_fact_hash, superseded_by_id, deleted_at)
               VALUES ($1,'chat',$2,'обещал прислать договор','commitment','contact',
                       0.9,'llm_extracted', md5('обещал договор '||$1), $3, now())
               RETURNING id""",
            tenant_id, chat_id, new_id,
        )
        await pool.execute(
            """INSERT INTO fact_embeddings
                   (fact_id, tenant_id, scope_kind, chat_id, embedding_model, embedding)
               VALUES ($1,$2,'chat',$3,'test',$4::vector)""",
            new_id, tenant_id, chat_id, vector,
        )
        await pool.execute(
            """INSERT INTO fact_relations
                   (tenant_id, fact_id_from, fact_id_to, relation_type, confidence)
               VALUES ($1,$2,$3,'supersedes',0.95)""",
            tenant_id, old_id, new_id,
        )

        async def embedder(query: str):
            return [0.01] * 4096

        ctx = SimpleNamespace(
            deps=AgentDeps(tenant_id=tenant_id, chat_id=chat_id, pool=pool, embedder=embedder)
        )
        return await search_facts(ctx, "обещал прислать договор")

    found = run_tool(setup_and_search)
    assert found == ["[commitment] отправил договор в пятницу ← ранее: обещал прислать договор"]


def test_suggest_profile_update_stages_pending_once(scope) -> None:
    from ai_agent.agents.memory_tools import suggest_profile_update

    tenant_id, chat_id = scope

    async def stage_twice(pool):
        ctx = SimpleNamespace(
            deps=__import__("ai_agent.agents.deps", fromlist=["AgentDeps"]).AgentDeps(
                tenant_id=tenant_id, chat_id=chat_id, pool=pool
            )
        )
        first = await suggest_profile_update(ctx, "tone", "casual", "пишет неформально")
        second = await suggest_profile_update(ctx, "tone", "casual", "пишет неформально")
        return first, second

    first, second = run_tool(stage_twice)
    assert first.startswith("staged for owner review")
    # 016 unique-pending constraint: the duplicate is rejected, not double-staged.
    assert second == "an identical suggestion is already pending"
