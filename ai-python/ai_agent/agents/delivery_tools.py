"""Internal message-splitting tool for the writer agent.

``send_message`` lets the writer split one turn into several ordered Telegram
messages instead of one wall of text. It only records intent in
:class:`AgentDeps`; the Go backend performs all contact-visible delivery, in
order, before the terminal reply/questionnaire.

This tool is intentionally NOT part of ``TOOL_CATALOG``: it is an agent-only
capability, always available to the writer, never surfaced in the role UI or
mirrored to Go as a user-toggleable tool.
"""

from __future__ import annotations

from pydantic_ai import ModelRetry, RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps
from .results import OutboundMessage

delivery_toolset: FunctionToolset[AgentDeps] = FunctionToolset()

# Upper bound on how many separate messages one turn may be split into, so a
# misbehaving model cannot flood the contact with dozens of sends.
_MAX_MESSAGES = 4
_MAX_TEXT_LEN = 4000


@delivery_toolset.tool
async def send_message(ctx: RunContext[AgentDeps], text: str) -> str:
    """Deliver one message to the contact now, then keep going.

    Call once per message. Whenever your turn mixes different kinds of content —
    e.g. an explanation and THEN a question — send them as SEPARATE messages:
    send_message for the prose, then ask_questions for a buttons-question (or
    another send_message for an open question). Never glue different parts into
    one message, and never call this for a single short reply that belongs in
    your normal final text. The messages are delivered in the order you call
    this, before your final reply. This records a deferred message; it sends
    nothing itself, and each call queues a NEW message (no dedup) — never call
    it twice with the same text.
    """
    clean = text.strip()
    if not clean:
        raise ModelRetry("send_message text must not be empty")
    if len(clean) > _MAX_TEXT_LEN:
        raise ModelRetry(f"send_message text must be at most {_MAX_TEXT_LEN} characters")
    if len(ctx.deps.outbound_messages) >= _MAX_MESSAGES:
        raise ModelRetry(
            f"already queued {_MAX_MESSAGES} messages this turn — finish with your reply"
        )
    ctx.deps.outbound_messages.append(OutboundMessage(text=clean))
    return f"queued message #{len(ctx.deps.outbound_messages)} for delivery"
