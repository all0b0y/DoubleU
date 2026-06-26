"""Deferred contact-interaction tools.

These tools only describe a response form in :class:`AgentDeps`. They never
send Telegram messages; the normal decision and owner-approval pipeline owns
all contact-visible effects.
"""

from __future__ import annotations

from pydantic_ai import RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps
from .results import ResponsePlan

interaction_toolset: FunctionToolset[AgentDeps] = FunctionToolset()


def _clean_options(options: list[str]) -> tuple[str, ...]:
    cleaned = tuple(option.strip() for option in options if option.strip())
    if len(cleaned) < 2 or len(cleaned) > 4:
        raise ValueError("interaction buttons require 2-4 non-empty options")
    if len(set(cleaned)) != len(cleaned):
        raise ValueError("interaction button options must be unique")
    return cleaned


@interaction_toolset.tool
async def present_buttons(
    ctx: RunContext[AgentDeps], prompt: str, options: list[str]
) -> str:
    """Prepare a question with 2-4 short buttons for the contact to choose from.

    Use only when a button choice is materially clearer than an ordinary text
    reply. This records a deferred response form; it does not send anything.
    """

    clean_prompt = prompt.strip()
    if not clean_prompt:
        raise ValueError("interaction prompt must not be empty")
    clean_options = _clean_options(options)
    ctx.deps.response_plan = ResponsePlan(
        shape="buttons", prompt=clean_prompt, options=clean_options
    )
    return "Форма ответа с кнопками подготовлена для обычного approval-пути."


@interaction_toolset.tool
async def request_confirmation(ctx: RunContext[AgentDeps], prompt: str) -> str:
    """Prepare a yes/no confirmation form without sending it immediately."""

    return await present_buttons(ctx, prompt, ["Подтвердить", "Отменить"])
