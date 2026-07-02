"""Integration tests for the load_skill tool against real PostgreSQL (Г4).

Skipped entirely when DATABASE_URL_TEST is not set (CI without Postgres).
Verifies the deny-by-default invariant: a skill loads only when its name is in
the role's enabled set AND the row is enabled and visible to the tenant.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from types import SimpleNamespace

import pytest

DATABASE_URL = os.getenv("DATABASE_URL_TEST", "").strip()

pytestmark = pytest.mark.skipif(not DATABASE_URL, reason="DATABASE_URL_TEST is not set")


def _run_tool(coro_factory):
    import asyncpg

    async def runner():
        pool = await asyncpg.create_pool(DATABASE_URL, min_size=1, max_size=2)
        try:
            return await coro_factory(pool)
        finally:
            await pool.close()

    return asyncio.run(runner())


def _ctx(pool, tenant_id: str, enabled) -> SimpleNamespace:
    from ai_agent.agents.deps import AgentDeps

    return SimpleNamespace(
        deps=AgentDeps(
            tenant_id=tenant_id, chat_id="0", pool=pool, enabled_skill_names=frozenset(enabled)
        )
    )


@pytest.fixture()
def skills_scope():
    """A tenant with an own enabled skill, a disabled skill, plus a global one."""
    import asyncpg

    tenant_id = f"tenant:skills:{uuid.uuid4().hex[:8]}"
    own = f"reminders-{uuid.uuid4().hex[:6]}"
    glob = f"global-{uuid.uuid4().hex[:6]}"
    disabled = f"hidden-{uuid.uuid4().hex[:6]}"

    async def setup() -> None:
        conn = await asyncpg.connect(DATABASE_URL)
        try:
            await conn.execute("INSERT INTO tenants (id,name) VALUES ($1,'t') ON CONFLICT DO NOTHING", tenant_id)
            await conn.execute(
                "INSERT INTO skills (tenant_id,name,description,body) VALUES ($1,$2,'d','OWN BODY')", tenant_id, own
            )
            await conn.execute(
                "INSERT INTO skills (tenant_id,name,description,body) VALUES (NULL,$1,'d','GLOBAL BODY')", glob
            )
            await conn.execute(
                "INSERT INTO skills (tenant_id,name,description,body,enabled) VALUES ($1,$2,'d','HIDDEN BODY',false)",
                tenant_id, disabled,
            )
        finally:
            await conn.close()

    asyncio.run(setup())
    yield tenant_id, own, glob, disabled

    async def teardown() -> None:
        conn = await asyncpg.connect(DATABASE_URL)
        try:
            await conn.execute("DELETE FROM skills WHERE tenant_id=$1", tenant_id)
            await conn.execute("DELETE FROM skills WHERE name=$1", glob)
            await conn.execute("DELETE FROM tenants WHERE id=$1", tenant_id)
        finally:
            await conn.close()

    asyncio.run(teardown())


def test_load_enabled_skill_returns_body(skills_scope) -> None:
    from ai_agent.agents.skill_tools import load_skill

    tenant_id, own, glob, _ = skills_scope

    async def go(pool):
        return await load_skill(_ctx(pool, tenant_id, [own, glob]), own)

    assert _run_tool(go) == f'<skill name="{own}">OWN BODY</skill>'


def test_unenabled_skill_is_refused(skills_scope) -> None:
    from ai_agent.agents.skill_tools import load_skill

    tenant_id, own, glob, _ = skills_scope

    async def go(pool):
        # `own` exists but is NOT in the role's enabled set → deny by default.
        return await load_skill(_ctx(pool, tenant_id, [glob]), own)

    assert _run_tool(go) == "навык не найден"


def test_global_skill_visible_to_any_tenant(skills_scope) -> None:
    from ai_agent.agents.skill_tools import load_skill

    _, _, glob, _ = skills_scope

    async def go(pool):
        return await load_skill(_ctx(pool, "tenant:other:zzz", [glob]), glob)

    assert _run_tool(go) == f'<skill name="{glob}">GLOBAL BODY</skill>'


def test_disabled_skill_not_loadable(skills_scope) -> None:
    from ai_agent.agents.skill_tools import load_skill

    tenant_id, _, _, disabled = skills_scope

    async def go(pool):
        # Even a stale grant cannot load a disabled row — the SQL filters enabled.
        return await load_skill(_ctx(pool, tenant_id, [disabled]), disabled)

    assert _run_tool(go) == "навык не найден"
