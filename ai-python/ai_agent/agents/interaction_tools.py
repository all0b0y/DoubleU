"""Deferred contact-interaction tools.

These tools only describe a response form in :class:`AgentDeps`. They never
send Telegram messages; the Go backend owns all contact-visible effects. The
questionnaire is collected step-by-step on the Go side: questions are asked one
at a time (answerable by tapping an option or replying in free text), the
answers are buffered, and only after every question is answered are the Q&A
saved and a reply drafted.
"""

from __future__ import annotations

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
    """Ask the contact one or more questions with tappable option buttons.

    Pass an ordered pool of 1–6 questions; each question is a ``prompt`` plus an
    optional list of short ``options`` rendered as quick-reply buttons (the
    contact may also answer in their own words). Use this for ANY question that
    offers a choice — a clarification, a poll, or a quiz / multiple-choice
    question — so the contact taps a button and the answer binds to the question
    instead of arriving as ambiguous free text like "2". For a quiz, ask one
    question per turn and fold any reaction to the previous answer into the next
    prompt. Do NOT call this for a plain open question with no choices (write it
    into your final reply instead), and do not call it twice in the same turn —
    it replaces, not accumulates, the pending form. This records a deferred
    form; it does not send anything.
    """

    if not questions:
        raise ModelRetry("ask_questions requires at least one question")
    if len(questions) > _MAX_QUESTIONS:
        raise ModelRetry(f"ask_questions allows at most {_MAX_QUESTIONS} questions")
    built = tuple(_build_question(item) for item in questions)
    ctx.deps.response_plan = ResponsePlan(shape="questions", questions=built)
    return f"Подготовлена анкета из {len(built)} вопрос(ов) для обычного approval-пути."


@interaction_toolset.tool
async def request_confirmation(ctx: RunContext[AgentDeps], prompt: str) -> str:
    """Prepare a single yes/no confirmation question without sending it.

    Use when one explicit yes/no confirmation is enough before acting. If you
    need more than one question or non-yes/no options, call ``ask_questions``
    directly instead.
    """

    return await ask_questions(
        ctx, [{"prompt": prompt, "options": ["Подтвердить", "Отменить"]}]
    )
