from __future__ import annotations

import asyncio
from types import SimpleNamespace

from pydantic_ai import ModelRetry

from ai_agent.agents.deps import AgentDeps
from ai_agent.agents.delivery_tools import _MAX_MESSAGES, send_message


def _ctx() -> SimpleNamespace:
    return SimpleNamespace(deps=AgentDeps(tenant_id="tenant:user:1", chat_id="chat-1"))


def test_send_message_appends_in_order() -> None:
    ctx = _ctx()
    asyncio.run(send_message(ctx, "  Сначала лекция.  "))
    asyncio.run(send_message(ctx, "Теперь вопрос?"))
    texts = [m.text for m in ctx.deps.outbound_messages]
    assert texts == ["Сначала лекция.", "Теперь вопрос?"]


def test_send_message_rejects_empty_with_model_retry() -> None:
    ctx = _ctx()
    try:
        asyncio.run(send_message(ctx, "   "))
    except ModelRetry:
        assert ctx.deps.outbound_messages == []
    else:  # pragma: no cover
        raise AssertionError("empty text must be rejected")


def test_send_message_caps_count() -> None:
    ctx = _ctx()
    for i in range(_MAX_MESSAGES):
        asyncio.run(send_message(ctx, f"msg {i}"))
    try:
        asyncio.run(send_message(ctx, "one too many"))
    except ModelRetry:
        assert len(ctx.deps.outbound_messages) == _MAX_MESSAGES
    else:  # pragma: no cover
        raise AssertionError("exceeding the cap must be rejected")
