"""Deferred contact-interaction tools.

These tools only describe a response form in :class:`AgentDeps`. They never
send Telegram messages; the Go backend owns all contact-visible effects. The
questionnaire is collected step-by-step on the Go side: questions are asked one
at a time (answerable by tapping an option or replying in free text), the
answers are buffered, and only after every question is answered are the Q&A
saved and a reply drafted.
"""

from __future__ import annotations

import re
from typing import Any

from pydantic_ai import ModelRetry, RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps
from .results import Question, ResponsePlan

interaction_toolset: FunctionToolset[AgentDeps] = FunctionToolset()

# Mirrors ResponsePlan/Question field bounds so a bad tool call fails loudly
# inside the tool rather than at pydantic validation of the response.
_MAX_QUESTIONS = 6
_MAX_OPTIONS = 8


def _build_question(raw: Any) -> Question:
    """Validate and normalise one question coming from the model.

    Tolerant of the shapes reasoning models commonly emit: a bare string is
    accepted as a prompt with no options. On unrecoverable input it raises
    :class:`ModelRetry` so pydantic-ai feeds the error back to the model for a
    corrected call instead of crashing the whole draft into error_fallback.
    """
    if isinstance(raw, Question):
        prompt, options = raw.prompt, list(raw.options)
    elif isinstance(raw, dict):
        prompt = str(raw.get("prompt", ""))
        options = list(raw.get("options", []) or [])
    elif isinstance(raw, str):
        prompt, options = raw, []
    else:
        raise ModelRetry("each question must be a string or an object with prompt and options")

    clean_prompt = prompt.strip()
    if not clean_prompt:
        raise ModelRetry("question prompt must not be empty")
    # A "question" with no letters or digits (e.g. "…", "—", "?") is a
    # degenerate placeholder the contact cannot answer; the Go side would send
    # it verbatim as the question text. Reject at the contract boundary so the
    # model retries with a real worded question instead.
    if not re.search(r"\w", clean_prompt, flags=re.UNICODE):
        raise ModelRetry(
            "question prompt must be a real worded question, not punctuation or an ellipsis"
        )
    clean_options = tuple(str(option).strip() for option in options if str(option).strip())
    if len(clean_options) > _MAX_OPTIONS:
        raise ModelRetry(f"a question allows at most {_MAX_OPTIONS} options")
    if len(set(clean_options)) != len(clean_options):
        raise ModelRetry("question options must be unique")
    return Question(prompt=clean_prompt, options=clean_options)


@interaction_toolset.tool
async def ask_questions(
    ctx: RunContext[AgentDeps], questions: list[Any]
) -> str:
    """Ask the contact one or more blocking questions as a deferred form.

    Pass an ordered pool of 1–6 questions; each question is a ``prompt`` plus an
    optional list of short ``options`` rendered as quick-reply buttons. Empty
    options mean the contact answers in free text. Use this only when the next
    useful reply depends on the answer, or when a choice must be captured
    unambiguously. Do NOT call it for a casual follow-up after a complete answer,
    and do not call it twice in the same turn — it replaces, not accumulates, the
    pending form. This records a deferred form; it does not send anything.
    """

    if not questions:
        raise ModelRetry("ask_questions requires at least one question")
    if len(questions) > _MAX_QUESTIONS:
        raise ModelRetry(f"ask_questions allows at most {_MAX_QUESTIONS} questions")
    built = tuple(_build_question(item) for item in questions)
    ctx.deps.response_plan = ResponsePlan(shape="questions", questions=built)
    return f"response_plan_recorded questions={len(built)}"


@interaction_toolset.tool
async def request_confirmation(
    ctx: RunContext[AgentDeps],
    prompt: str,
    confirm_label: str = "",
    cancel_label: str = "",
) -> str:
    """Prepare a single yes/no confirmation question without sending it.

    Use when one explicit yes/no confirmation is enough before acting. Provide
    both button labels in the same language as ``prompt``. If you need more than
    one question or non-yes/no options, call ``ask_questions`` directly instead.
    """

    confirm = confirm_label.strip()
    cancel = cancel_label.strip()
    if not confirm or not cancel:
        raise ModelRetry(
            "request_confirmation requires confirm_label and cancel_label in the prompt language"
        )
    if confirm == cancel:
        raise ModelRetry("request_confirmation labels must be different")
    return await ask_questions(
        ctx, [{"prompt": prompt, "options": [confirm, cancel]}]
    )
