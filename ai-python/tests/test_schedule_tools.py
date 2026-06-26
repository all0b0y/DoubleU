from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

from ai_agent.agents.deps import AgentDeps
from ai_agent.agents.schedule_tools import create_reminder
from ai_agent.pipelines.draft import build_draft_prompt, build_orchestrator_prompt
from ai_agent.schemas import (
    AIProcessMessageRequest,
    ChatMode,
    ChatSettings,
    NormalizedTelegramEvent,
    UserSettingsData,
    AIRoleData,
)
from ai_agent.security import PromptBuilder


class _RejectingPool:
    def __init__(self) -> None:
        self.calls: list[tuple[str, tuple[object, ...]]] = []

    async def fetchrow(self, query: str, *args: object) -> dict[str, int]:
        self.calls.append((query, args))
        raise AssertionError("scheduler tool must not write to Postgres")


def _ctx(*, pool=None, flags: tuple[str, ...] = ()) -> SimpleNamespace:
    return SimpleNamespace(deps=AgentDeps(
        tenant_id="tenant:user:1",
        chat_id="chat-1",
        source_message_id="message-1",
        privacy_flags=flags,
        pool=pool,
        current_time=datetime(2026, 6, 22, 12, 0, tzinfo=timezone.utc),
    ))


def test_create_reminder_accumulates_scoped_idempotent_job() -> None:
    pool = _RejectingPool()
    ctx = _ctx(pool=pool)
    result = asyncio.run(create_reminder(
        ctx, "2026-06-22T18:00:00+03:00", "  Позвонить клиенту  ", "owner"
    ))
    duplicate = asyncio.run(create_reminder(
        ctx, "2026-06-22T18:00:00+03:00", "Позвонить клиенту", "owner"
    ))

    assert result == "scheduled reminder for 2026-06-22T15:00:00+00:00"
    assert duplicate == result
    assert pool.calls == []
    assert len(ctx.deps.scheduled_jobs) == 1
    job = ctx.deps.scheduled_jobs[0]
    assert job.kind == "reminder"
    assert job.target == "owner"
    assert job.fire_at == datetime(2026, 6, 22, 15, 0, tzinfo=timezone.utc)
    assert job.payload == {"text": "Позвонить клиенту"}
    assert job.created_by == "ai"
    assert job.idempotency_key.startswith("scheduler:")


def test_create_reminder_rejects_past_and_more_than_year() -> None:
    pool = _RejectingPool()
    past = asyncio.run(create_reminder(
        _ctx(pool=pool), "2026-06-22T11:59:59Z", "past", "owner"
    ))
    too_far = asyncio.run(create_reminder(
        _ctx(pool=pool),
        (datetime(2026, 6, 22, 12, 0, tzinfo=timezone.utc) + timedelta(days=366)).isoformat(),
        "future",
        "contact",
    ))

    assert past == "error: fire_at must be strictly in the future"
    assert too_far == "error: fire_at must be no more than 1 year in the future"
    assert pool.calls == []


def test_create_reminder_respects_privacy_and_storage_gate() -> None:
    for flags in (("no_memory",), ("private",)):
        result = asyncio.run(create_reminder(
            _ctx(pool=_RejectingPool(), flags=flags), "2026-06-23T12:00:00Z", "blocked", "owner"
        ))
        assert "privacy" in result
    ctx = _ctx(pool=None)
    available = asyncio.run(create_reminder(
        ctx, "2026-06-23T12:00:00Z", "allowed without Python storage", "owner"
    ))
    assert available == "scheduled reminder for 2026-06-23T12:00:00+00:00"
    assert len(ctx.deps.scheduled_jobs) == 1


def test_orchestrator_prompt_contains_current_owner_time_and_timezone() -> None:
    request = AIProcessMessageRequest(
        request_id="r1",
        event=NormalizedTelegramEvent(
            event_id="e1", tenant_id="tenant:user:1", chat_id="chat-1",
            telegram_update_id=1, business_connection_id="", source_message_id="m1",
            sender_id="contact", message_text="Напомни завтра", message_type="text",
            created_at="2026-06-22T12:00:00Z",
        ),
        chat_settings=ChatSettings(
            tenant_id="tenant:user:1", chat_id="chat-1", mode=ChatMode.MANUAL,
        ),
        user_settings=UserSettingsData(timezone="Europe/Moscow"),
        ai_role=AIRoleData(
            name="Scheduler",
            tool_settings={"scheduler.create_reminder": True},
        ),
    )
    current = datetime(2026, 6, 22, 12, 0, tzinfo=timezone.utc)
    system, _ = build_orchestrator_prompt(request, PromptBuilder(), current_time=current)
    writer_system, _ = build_draft_prompt(request, PromptBuilder(), current_time=current)

    assert "2026-06-22T15:00:00+03:00" in system
    assert "Europe/Moscow" in system
    assert "scheduler.create_reminder" in system
    assert "2026-06-22T15:00:00+03:00" in writer_system
