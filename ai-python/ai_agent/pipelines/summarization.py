"""Chat summarization pipeline with privacy gating."""

from __future__ import annotations

from ..llm import ModelRouter
from ..schemas import (
    ChatSummaryRequest,
    ChatSummaryResponse,
    ModelTask,
    PrivacyFlag,
    Usage,
)
from ..security import PromptBuilder


# Action-item heuristics: any of these tokens flag a message as an action
# candidate.  Kept as a frozenset so the test can compare against it cheaply.
_ACTION_TOKENS = frozenset(
    {"please", "send", "need", "todo", "action", "prepare", "schedule"}
)


def summarize_chat(
    router: ModelRouter,
    prompt_builder: PromptBuilder,
    request: ChatSummaryRequest,
) -> ChatSummaryResponse:
    """Summarize ``request.messages`` while honoring ``NO_SUMMARY``."""
    if PrivacyFlag.NO_SUMMARY in request.privacy_flags:
        return ChatSummaryResponse(
            summary="",
            action_items=(),
            model_used="none",
            usage=Usage(),
            blocked_by_privacy=True,
        )
    combined = "\n".join(request.messages)
    system_prompt, user_payload = prompt_builder.build(
        "Summarize the chat and identify action items.",
        combined,
        (),
    )
    gateway_response = router.complete(ModelTask.SUMMARIZE_CHAT, system_prompt, user_payload)
    action_items = tuple(message for message in request.messages if _looks_actionable(message))
    return ChatSummaryResponse(
        summary=gateway_response.text,
        action_items=action_items,
        model_used=gateway_response.model_used,
        usage=gateway_response.usage,
    )


def _looks_actionable(message: str) -> bool:
    """Heuristic: does a message contain a verb suggesting an action item?"""
    lowered = message.lower()
    return any(word in lowered for word in _ACTION_TOKENS)
