"""Deferred scheduler tools collected for Go-owned scheduled_jobs persistence."""

from __future__ import annotations

import hashlib
from datetime import datetime, timedelta, timezone
from typing import Literal

from pydantic_ai import RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps
from .results import ScheduledJob

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

    Call only when the message clearly asks for a reminder at a specific
    future time. ``fire_at_iso`` must include a timezone offset. Use the
    current time and owner timezone from the system instruction — if the
    named time has already passed today and the rollover is ambiguous, ask a
    clarifying question (``ask_questions``) instead of guessing a time. This
    tool records the job for the Go backend to persist; it never sends the
    reminder immediately.
    """

    if ctx.deps.no_memory or ctx.deps.private:
        return "error: scheduler is unavailable for this chat privacy mode"
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
    if not any(job.idempotency_key == idempotency_key for job in ctx.deps.scheduled_jobs):
        ctx.deps.scheduled_jobs.append(
            ScheduledJob(
                kind="reminder",
                target=target,
                fire_at=fire_at,
                payload={"text": clean_text},
                created_by="ai",
                idempotency_key=idempotency_key,
            )
        )
    return f"scheduled reminder for {fire_at.isoformat()}"


@schedule_toolset.tool
async def list_reminders(ctx: RunContext[AgentDeps]) -> str:
    """List the pending reminders already scheduled for this chat.

    Call before ``cancel_reminder`` to find its numeric id, or when you need
    to reference an existing reminder. Returns each reminder's numeric id, fire
    time and text. The ids come from the Go-owned scheduled_jobs rows supplied
    on the request; this tool performs no DB access.
    """

    if ctx.deps.no_memory or ctx.deps.private:
        return "error: scheduler is unavailable for this chat privacy mode"
    jobs = ctx.deps.existing_jobs
    if not jobs:
        return "Активных напоминаний нет."
    lines = []
    for job in jobs:
        text = (job.text or "").strip()
        suffix = f" — {text}" if text else ""
        lines.append(f"#{job.id} [{job.target}] {job.fire_at}{suffix}")
    return "\n".join(lines)


@schedule_toolset.tool
async def cancel_reminder(ctx: RunContext[AgentDeps], reminder_id: int) -> str:
    """Cancel one pending reminder by its numeric id (from ``list_reminders``).

    Only call with an id you actually have from ``list_reminders`` — never
    guess a number. Records the cancellation for the Go backend to apply
    within this chat's scope; it never touches another chat's jobs.
    Re-cancelling the same id is a no-op so concurrent runs stay idempotent.
    """

    if ctx.deps.no_memory or ctx.deps.private:
        return "error: scheduler is unavailable for this chat privacy mode"
    if not any(job.id == reminder_id for job in ctx.deps.existing_jobs):
        return f"error: no pending reminder with id {reminder_id}"
    if reminder_id not in ctx.deps.cancelled_job_ids:
        ctx.deps.cancelled_job_ids.append(reminder_id)
    return f"reminder #{reminder_id} cancelled"
