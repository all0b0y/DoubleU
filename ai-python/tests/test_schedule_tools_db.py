"""Real-PostgreSQL integration for scheduler.create_reminder.

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


def test_create_reminder_writes_scheduled_job() -> None:
    import asyncpg

    from ai_agent.agents.deps import AgentDeps
    from ai_agent.agents.schedule_tools import create_reminder

    tenant_id = f"tenant:test:{uuid.uuid4().hex[:8]}"
    chat_id = "scheduler-chat"
    now = datetime.now(timezone.utc)

    async def scenario() -> None:
        pool = await asyncpg.create_pool(DATABASE_URL, min_size=1, max_size=2)
        try:
            await pool.execute(
                "INSERT INTO tenants (id, name) VALUES ($1, 'test') ON CONFLICT DO NOTHING",
                tenant_id,
            )
            await pool.execute(
                """INSERT INTO chats (tenant_id, chat_id, telegram_chat_id, status, transport)
                   VALUES ($1, $2, $2, 'managed', 'bot') ON CONFLICT DO NOTHING""",
                tenant_id,
                chat_id,
            )
            ctx = SimpleNamespace(deps=AgentDeps(
                tenant_id=tenant_id,
                chat_id=chat_id,
                source_message_id="m1",
                pool=pool,
                current_time=now,
            ))
            result = await create_reminder(
                ctx, (now + timedelta(minutes=10)).isoformat(), "Проверить договор", "owner"
            )
            assert result.startswith("scheduled reminder #")
            row = await pool.fetchrow(
                """SELECT tenant_id, chat_id, kind, target, payload->>'text' AS text,
                          status, created_by
                     FROM scheduled_jobs
                    WHERE tenant_id = $1 AND chat_id = $2""",
                tenant_id,
                chat_id,
            )
            assert dict(row) == {
                "tenant_id": tenant_id,
                "chat_id": chat_id,
                "kind": "reminder",
                "target": "owner",
                "text": "Проверить договор",
                "status": "pending",
                "created_by": "ai",
            }
        finally:
            await pool.close()

    asyncio.run(scenario())
