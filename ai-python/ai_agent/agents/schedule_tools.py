"""Deferred scheduler tools backed by PostgreSQL scheduled_jobs."""

from __future__ import annotations

import hashlib
import json
from datetime import datetime, timedelta, timezone
from typing import Literal

from pydantic_ai import RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps

schedule_toolset: FunctionToolset[AgentDeps] = FunctionToolset()

_MAX_SCHEDULE_AHEAD = timedelta(days=365)


def _parse_fire_at(raw: str) -> datetime | None:
    value = raw.strip()
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed.astimezone(timezone.utc)


@schedule_toolset.tool
async def create_reminder(
    ctx: RunContext[AgentDeps],
    fire_at_iso: str,
    text: str,
    target: Literal["owner", "contact"],
) -> str:
    """Create one future reminder for the owner or current-chat contact.

    ``fire_at_iso`` must include a timezone offset. Use the current time and
    owner timezone from the system instruction. This tool persists the job but
    never sends the reminder immediately.
    """

    if ctx.deps.no_memory or ctx.deps.private:
        return "error: scheduler is unavailable for this chat privacy mode"
    if ctx.deps.pool is None:
        return "error: scheduler storage is unavailable"
    fire_at = _parse_fire_at(fire_at_iso)
    if fire_at is None:
        return "error: fire_at_iso must be an ISO-8601 timestamp with timezone"
    now = ctx.deps.current_time.astimezone(timezone.utc)
    if fire_at <= now:
        return "error: fire_at must be strictly in the future"
    if fire_at > now + _MAX_SCHEDULE_AHEAD:
        return "error: fire_at must be no more than 1 year in the future"
    clean_text = text.strip()
    if not clean_text:
        return "error: reminder text must not be empty"

    idem_source = "\x00".join(
        (
            ctx.deps.tenant_id,
            ctx.deps.chat_id,
            ctx.deps.source_message_id,
            fire_at.isoformat(),
            target,
            clean_text,
        )
    )
    idempotency_key = "scheduler:" + hashlib.sha256(idem_source.encode()).hexdigest()
    payload = json.dumps({"text": clean_text}, ensure_ascii=False)
    row = await ctx.deps.pool.fetchrow(
        """INSERT INTO scheduled_jobs
               (tenant_id, chat_id, kind, target, fire_at, payload,
                status, created_by, idempotency_key)
           VALUES ($1, $2, 'reminder', $3, $4, $5::jsonb,
                   'pending', 'ai', $6)
           ON CONFLICT (idempotency_key) DO UPDATE
           SET idempotency_key = EXCLUDED.idempotency_key
           RETURNING id""",
        ctx.deps.tenant_id,
        ctx.deps.chat_id,
        target,
        fire_at,
        payload,
        idempotency_key,
    )
    return f"scheduled reminder #{int(row['id'])} for {fire_at.isoformat()}"
