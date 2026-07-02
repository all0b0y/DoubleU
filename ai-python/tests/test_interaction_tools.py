from __future__ import annotations

import asyncio
from types import SimpleNamespace

from pydantic_ai import ModelRetry

from ai_agent.agents.deps import AgentDeps
from ai_agent.agents.interaction_tools import ask_questions, request_confirmation


def _ctx() -> SimpleNamespace:
    return SimpleNamespace(deps=AgentDeps(tenant_id="tenant:user:1", chat_id="chat-1"))


def test_ask_questions_records_normalised_pool() -> None:
    ctx = _ctx()
    result = asyncio.run(ask_questions(ctx, [
        {"prompt": "  Когда удобно?  ", "options": [" Сегодня ", "Завтра"]},
        {"prompt": "Какой бюджет?"},  # pure free-text question, no options
    ]))
    assert "questions=2" in result
    plan = ctx.deps.response_plan
    assert plan is not None and plan.shape == "questions"
    assert len(plan.questions) == 2
    assert plan.questions[0].prompt == "Когда удобно?"
    assert plan.questions[0].options == ("Сегодня", "Завтра")
    assert plan.questions[1].options == ()


def test_ask_questions_rejects_empty_and_oversized_pools() -> None:
    # Bad calls raise ModelRetry (recoverable) so the framework re-prompts the
    # model rather than crashing the whole draft into error_fallback.
    for bad in ([], [{"prompt": "  "}], [{"prompt": f"q{i}"} for i in range(7)]):
        ctx = _ctx()
        try:
            asyncio.run(ask_questions(ctx, bad))
        except ModelRetry:
            assert ctx.deps.response_plan is None
        else:  # pragma: no cover - guard against silent acceptance
            raise AssertionError(f"expected rejection for {bad!r}")


def test_ask_questions_rejects_wordless_prompt() -> None:
    # A "question" with no letters or digits ("…", "—", "?") is a degenerate
    # placeholder the contact cannot answer; the backend would send it verbatim
    # as the question text. It must be rejected so the model retries with a real
    # worded question — covers the "…" confirmation regression.
    for bad in ("…", "...", "—", "?", "  -- "):
        ctx = _ctx()
        try:
            asyncio.run(ask_questions(ctx, [{"prompt": bad, "options": ["Да", "Нет"]}]))
        except ModelRetry:
            assert ctx.deps.response_plan is None
        else:  # pragma: no cover - guard against silent acceptance
            raise AssertionError(f"expected rejection for wordless prompt {bad!r}")


def test_ask_questions_accepts_bare_string_questions() -> None:
    # Reasoning models often pass questions as plain strings; treat each as a
    # prompt with no options instead of failing.
    ctx = _ctx()
    result = asyncio.run(ask_questions(ctx, ["Когда удобно?", "Какой бюджет?"]))
    assert "questions=2" in result
    plan = ctx.deps.response_plan
    assert plan is not None and len(plan.questions) == 2
    assert plan.questions[0].prompt == "Когда удобно?"
    assert plan.questions[0].options == ()


def test_ask_questions_rejects_duplicate_options() -> None:
    ctx = _ctx()
    try:
        asyncio.run(ask_questions(ctx, [{"prompt": "q", "options": ["a", "a"]}]))
    except ModelRetry:
        assert ctx.deps.response_plan is None
    else:  # pragma: no cover
        raise AssertionError("duplicate options must be rejected")


def test_request_confirmation_is_single_yes_no_question() -> None:
    ctx = _ctx()
    asyncio.run(request_confirmation(
        ctx,
        "Отправить сейчас?",
        confirm_label="Да, отправить",
        cancel_label="Не отправлять",
    ))
    plan = ctx.deps.response_plan
    assert plan is not None and len(plan.questions) == 1
    assert plan.questions[0].prompt == "Отправить сейчас?"
    assert plan.questions[0].options == ("Да, отправить", "Не отправлять")


def test_request_confirmation_requires_language_specific_labels() -> None:
    ctx = _ctx()
    try:
        asyncio.run(request_confirmation(ctx, "Отправить сейчас?"))
    except ModelRetry:
        assert ctx.deps.response_plan is None
    else:  # pragma: no cover
        raise AssertionError("request_confirmation must not invent hardcoded labels")
