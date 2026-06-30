"""Real-PostgreSQL guard for scheduler.create_reminder statelessness.

Run with DATABASE_URL_TEST pointing at a schema with migration 114 applied.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

import pytest

DATABASE_URL = os.getenv("DATABASE_URL_TEST", "").strip()
pytestmark = pytest.mark.skipif(not DATABASE_URL, reason="DATABASE_URL_TEST is not set")


def test_create_reminder_does_not_write_scheduled_job_from_python() -> None:
    import asyncpg

    from ai_agent.agents.deps import AgentDeps
    from ai_agent.agents.schedule_tools import create_reminder

    tenant_id = f"tenant:test:{uuid.uuid4().hex[:8]}"
    chat_id = "scheduler-chat"
    now = datetime.now(timezone.utc)

    async def scenario() -> None:
        pool = await asyncpg.create_pool(DATABASE_URL, min_size=1, max_size=2)
        try:
            deps = AgentDeps(
                tenant_id=tenant_id,
                chat_id=chat_id,
                source_message_id="m1",
                pool=pool,
                current_time=now,
                owner_timezone="UTC",
            )
            ctx = SimpleNamespace(deps=deps)
            result = await create_reminder(
                ctx, (now + timedelta(minutes=10)).isoformat(), "Проверить договор", "owner"
            )
            assert result.startswith("scheduled reminder for ")
            assert len(deps.scheduled_jobs) == 1
            job = deps.scheduled_jobs[0]
            count = await pool.fetchval(
                "SELECT count(*) FROM scheduled_jobs WHERE idempotency_key = $1",
                job.idempotency_key,
            )
            assert count == 0
            assert job.payload == {"text": "Проверить договор"}
        finally:
            await pool.close()

    asyncio.run(scenario())
