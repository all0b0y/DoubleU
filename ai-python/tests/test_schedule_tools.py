from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

from ai_agent.agents.deps import AgentDeps
from ai_agent.agents.schedule_tools import cancel_reminder, create_reminder, list_reminders
from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.library import draft_spec, orchestrator_spec
from ai_agent.prompts.renderers import render as render_prompt
from ai_agent.schemas import (
    AIProcessMessageRequest,
    ChatMode,
    ChatSettings,
    ExistingScheduledJob,
    NormalizedTelegramEvent,
    UserSettingsData,
    AIRoleData,
)


class _RejectingPool:
    def __init__(self) -> None:
        self.calls: list[tuple[str, tuple[object, ...]]] = []

    async def fetchrow(self, query: str, *args: object) -> dict[str, int]:
        self.calls.append((query, args))
        raise AssertionError("scheduler tool must not write to Postgres")


def _ctx(
    *, pool=None, flags: tuple[str, ...] = (), existing=(), owner_timezone: str = "UTC"
) -> SimpleNamespace:
    return SimpleNamespace(deps=AgentDeps(
        tenant_id="tenant:user:1",
        chat_id="chat-1",
        source_message_id="message-1",
        privacy_flags=flags,
        pool=pool,
        current_time=datetime(2026, 6, 22, 12, 0, tzinfo=timezone.utc),
        existing_jobs=existing,
        owner_timezone=owner_timezone,
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


def test_create_reminder_refuses_when_owner_timezone_unknown() -> None:
    """A1: bot-only owners (timezone never synced) must not get a silent UTC guess."""
    ctx = _ctx(pool=_RejectingPool(), owner_timezone="")
    result = asyncio.run(create_reminder(
        ctx, "2026-06-22T18:00:00+03:00", "Позвонить клиенту", "owner"
    ))

    assert result.startswith("error: owner timezone unknown")
    assert "ask_questions" in result
    assert ctx.deps.scheduled_jobs == []


def test_create_reminder_works_as_before_with_known_timezone() -> None:
    """Known owner timezone (e.g. Europe/Moscow) schedules exactly as before A1."""
    ctx = _ctx(pool=_RejectingPool(), owner_timezone="Europe/Moscow")
    result = asyncio.run(create_reminder(
        ctx, "2026-06-22T18:00:00+03:00", "Позвонить клиенту", "owner"
    ))

    assert result == "scheduled reminder for 2026-06-22T15:00:00+00:00"
    assert len(ctx.deps.scheduled_jobs) == 1


def test_create_reminder_attaches_walltime_identity_for_dst_safe_dispatch() -> None:
    """D: the job carries the owner's wall-clock time + zone so Go can
    recompute the authoritative fire instant from current tzdata."""
    ctx = _ctx(pool=_RejectingPool(), owner_timezone="Europe/Moscow")
    asyncio.run(create_reminder(
        ctx, "2026-06-22T18:00:00+03:00", "Позвонить клиенту", "owner"
    ))

    job = ctx.deps.scheduled_jobs[0]
    assert job.local_time == "2026-06-22T18:00:00"
    assert job.tz_name == "Europe/Moscow"


def _existing(job_id: int, text: str = "позвонить") -> ExistingScheduledJob:
    return ExistingScheduledJob(
        id=job_id, kind="reminder", target="owner",
        fire_at="2026-06-23T12:00:00+00:00", text=text,
    )


def test_list_reminders_formats_pending_jobs() -> None:
    empty = asyncio.run(list_reminders(_ctx()))
    assert empty == "Активных напоминаний нет."

    listed = asyncio.run(list_reminders(_ctx(existing=(_existing(7), _existing(9, "встреча")))))
    assert "#7" in listed and "позвонить" in listed
    assert "#9" in listed and "встреча" in listed


def test_cancel_reminder_records_only_known_pending_ids() -> None:
    ctx = _ctx(existing=(_existing(7),))
    ok = asyncio.run(cancel_reminder(ctx, 7))
    assert ok == "reminder #7 cancelled"
    assert ctx.deps.cancelled_job_ids == [7]

    # Re-cancelling the same id stays idempotent.
    asyncio.run(cancel_reminder(ctx, 7))
    assert ctx.deps.cancelled_job_ids == [7]

    missing = asyncio.run(cancel_reminder(ctx, 42))
    assert "no pending reminder" in missing
    assert ctx.deps.cancelled_job_ids == [7]


def test_list_and_cancel_respect_privacy_gate() -> None:
    for flags in (("no_memory",), ("private",)):
        ctx = _ctx(flags=flags, existing=(_existing(7),))
        assert "privacy" in asyncio.run(list_reminders(ctx))
        assert "privacy" in asyncio.run(cancel_reminder(ctx, 7))
        assert ctx.deps.cancelled_job_ids == []


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
    system = render_prompt(orchestrator_spec(request, current_time=current), ModelFamily.GPT)
    writer_spec = draft_spec(
        request,
        clean_memory=[],
        owner_style_facts=[],
        current_draft=None,
        owner_instruction=None,
        feedback_hint="",
        current_time=current,
    )
    writer_system = render_prompt(writer_spec, ModelFamily.GPT)

    assert "2026-06-22T15:00:00+03:00" in system
    assert "Europe/Moscow" in system
    assert "scheduler.create_reminder" in system
    assert "2026-06-22T15:00:00+03:00" in writer_system
