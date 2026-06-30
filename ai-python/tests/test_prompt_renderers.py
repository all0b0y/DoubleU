"""Behavioural tests for :func:`ai_agent.prompts.renderers.render`.

Every family renders the SAME ``<prompt_contract>`` XML envelope — these
tests check the structural markers the plan calls for: every section tag
(``role``/``task``/``trusted_context``/``constraints``/``tool_policy``/
``output_format``/``success_criteria``) is always present, GPT and agentic
include ``<examples>`` and full content, reasoning omits ``<examples>`` and
adds an explicit no-chain-of-thought constraint instead. Content presence is
checked alongside structure so a renderer can't satisfy the markers while
silently dropping spec fields.
"""

from __future__ import annotations

from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.renderers import render
from ai_agent.prompts.spec import OutputFormat, PromptSpec

_FULL_SPEC = PromptSpec(
    role="role-sentinel",
    task="task-sentinel",
    context=("context-sentinel",),
    constraints=("constraint-sentinel",),
    output_format=OutputFormat(text_format="telegram_rich_markdown", rules="rules-sentinel"),
    examples=("example-sentinel",),
    success_criteria=("success-sentinel",),
    tool_policy="tool-policy-sentinel",
)

_MINIMAL_SPEC = PromptSpec(role="role-sentinel", task="task-sentinel")

_ENVELOPE_TAGS = (
    "<role>", "<task>", "<trusted_context>", "<constraints>",
    "<tool_policy>", "<output_format>", "<success_criteria>",
)


def _clear_lang_env(monkeypatch) -> None:
    monkeypatch.delenv("PROMPT_PRIMARY_LANGUAGE", raising=False)


def test_gpt_renderer_has_full_envelope_and_all_content() -> None:
    text = render(_FULL_SPEC, ModelFamily.GPT)
    assert text.startswith("<prompt_contract>")
    assert text.endswith("</prompt_contract>")
    for tag in _ENVELOPE_TAGS:
        assert tag in text
    assert "<examples>" in text
    for sentinel in ("role-sentinel", "task-sentinel", "context-sentinel",
                      "constraint-sentinel", "rules-sentinel", "example-sentinel",
                      "success-sentinel", "tool-policy-sentinel"):
        assert sentinel in text
    assert "<text_format>telegram_rich_markdown</text_format>" in text


def test_gpt_renderer_omits_examples_tag_when_spec_has_none() -> None:
    text = render(_MINIMAL_SPEC, ModelFamily.GPT)
    assert "<examples>" not in text
    for tag in _ENVELOPE_TAGS:
        assert tag in text
    # Optional sections render as an empty, stably-present tag.
    assert "<trusted_context></trusted_context>" in text
    assert "<constraints></constraints>" in text
    assert "<tool_policy></tool_policy>" in text
    assert "<text_format>plain_text</text_format>" in text


def test_reasoning_renderer_omits_examples_and_adds_no_cot_constraint(monkeypatch) -> None:
    _clear_lang_env(monkeypatch)
    text = render(_FULL_SPEC, ModelFamily.REASONING)
    for tag in _ENVELOPE_TAGS:
        assert tag in text
    # Examples are deliberately dropped for reasoning models, even though the
    # spec carries one — examples-bloat is the thing being avoided.
    assert "<examples>" not in text
    assert "example-sentinel" not in text
    # No chain-of-thought request; an explicit suppression instead.
    assert "думай" not in text.lower()
    assert "шаг за шагом" not in text.lower()
    assert "Верни только результат, без раскрытия рассуждений." in text
    # The footer is rendered as a <constraint> entry, inside the same envelope.
    assert "<constraint>Верни только результат, без раскрытия рассуждений.</constraint>" in text
    # Role/context/task content still reaches the model.
    for sentinel in ("role-sentinel", "task-sentinel", "context-sentinel",
                      "constraint-sentinel", "success-sentinel", "tool-policy-sentinel"):
        assert sentinel in text


def test_reasoning_renderer_respects_primary_language_env(monkeypatch) -> None:
    monkeypatch.setenv("PROMPT_PRIMARY_LANGUAGE", "en")
    text = render(_FULL_SPEC, ModelFamily.REASONING)
    assert "Return only the result, without revealing your reasoning." in text
    assert "Верни только результат" not in text


def test_reasoning_renderer_minimal_spec_still_carries_the_footer_constraint() -> None:
    text = render(_MINIMAL_SPEC, ModelFamily.REASONING)
    assert "<examples>" not in text
    assert "role-sentinel" in text
    assert "task-sentinel" in text
    # No user constraints, but the footer constraint is unconditional.
    assert "<constraint>Верни только результат, без раскрытия рассуждений.</constraint>" in text


def test_agentic_renderer_has_full_envelope_and_all_content() -> None:
    text = render(_FULL_SPEC, ModelFamily.AGENTIC)
    for tag in _ENVELOPE_TAGS:
        assert tag in text
    assert "<examples>" in text
    for sentinel in ("role-sentinel", "task-sentinel", "context-sentinel",
                      "constraint-sentinel", "tool-policy-sentinel", "rules-sentinel"):
        assert sentinel in text


def test_agentic_renderer_always_emits_tool_policy_tag_even_when_empty() -> None:
    text = render(_MINIMAL_SPEC, ModelFamily.AGENTIC)
    assert "<tool_policy></tool_policy>" in text
    assert "<trusted_context></trusted_context>" in text
    assert "<constraints></constraints>" in text


def test_gpt_and_agentic_render_the_same_envelope_today() -> None:
    """Documents the deliberate convergence: both render the full envelope.

    The family split exists so either can diverge later (e.g. agentic-specific
    tool-workflow framing) without touching the other — see the renderers.py
    module docstring — but as of this phase neither has a reason to differ.
    """
    assert render(_FULL_SPEC, ModelFamily.GPT) == render(_FULL_SPEC, ModelFamily.AGENTIC)
    assert render(_MINIMAL_SPEC, ModelFamily.GPT) == render(_MINIMAL_SPEC, ModelFamily.AGENTIC)


def test_render_dispatches_by_family_reasoning_differs_from_gpt() -> None:
    """Sanity check that render() actually picks a different shape for reasoning."""
    gpt_text = render(_FULL_SPEC, ModelFamily.GPT)
    reasoning_text = render(_FULL_SPEC, ModelFamily.REASONING)
    assert gpt_text != reasoning_text
    assert "<examples>" in gpt_text
    assert "<examples>" not in reasoning_text


class TestOutputFormatTag:
    """Rich mode must require Telegram Rich Markdown; plain must forbid markdown."""

    def test_rich_text_format_renders_telegram_rich_markdown_token(self) -> None:
        spec = PromptSpec(
            role="r", task="t",
            output_format=OutputFormat(
                text_format="telegram_rich_markdown", rules="Используй GFM для структурных ответов."
            ),
        )
        for family in (ModelFamily.GPT, ModelFamily.REASONING, ModelFamily.AGENTIC):
            text = render(spec, family)
            assert "<text_format>telegram_rich_markdown</text_format>" in text
            assert "<rules>" in text and "Используй GFM" in text

    def test_plain_text_format_renders_plain_token_and_no_rich_rules(self) -> None:
        spec = PromptSpec(
            role="r", task="t",
            output_format=OutputFormat(text_format="plain_text", rules="Обычный текст без разметки."),
        )
        for family in (ModelFamily.GPT, ModelFamily.REASONING, ModelFamily.AGENTIC):
            text = render(spec, family)
            assert "<text_format>plain_text</text_format>" in text
            assert "telegram_rich_markdown" not in text

    def test_default_output_format_is_plain_with_no_rules_tag(self) -> None:
        spec = PromptSpec(role="r", task="t")
        text = render(spec, ModelFamily.GPT)
        assert "<output_format>\n<text_format>plain_text</text_format>\n</output_format>" in text
        assert "<rules>" not in text
