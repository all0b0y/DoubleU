"""Behavioural tests for :func:`ai_agent.prompts.renderers.render`.

One full :class:`PromptSpec` is rendered for each family and checked against
the structural markers the plan calls for: GPT gets explicit ``# Section``
headers and a closing ``# Verification`` list; reasoning gets a minimal brief
with no examples and an explicit no-chain-of-thought instruction; agentic
gets the XML role/objective/context/constraints/tool_policy/output_format
shape. Content presence is checked alongside structure so a renderer can't
satisfy the markers while silently dropping spec fields.
"""

from __future__ import annotations

from ai_agent.prompts.families import ModelFamily
from ai_agent.prompts.renderers import render
from ai_agent.prompts.spec import PromptSpec

_FULL_SPEC = PromptSpec(
    role="role-sentinel",
    task="task-sentinel",
    context=("context-sentinel",),
    constraints=("constraint-sentinel",),
    output_format="output-format-sentinel",
    examples=("example-sentinel",),
    success_criteria=("success-sentinel",),
    tool_policy="tool-policy-sentinel",
)

_MINIMAL_SPEC = PromptSpec(role="role-sentinel", task="task-sentinel")


def _clear_lang_env(monkeypatch) -> None:
    monkeypatch.delenv("PROMPT_PRIMARY_LANGUAGE", raising=False)


def test_gpt_renderer_has_explicit_sections_and_all_content(monkeypatch) -> None:
    text = render(_FULL_SPEC, ModelFamily.GPT)
    for header in ("# Role", "# Task", "# Context", "# Constraints",
                   "# Output Format", "# Examples", "# Verification"):
        assert header in text
    for sentinel in ("role-sentinel", "task-sentinel", "context-sentinel",
                      "constraint-sentinel", "output-format-sentinel",
                      "example-sentinel", "success-sentinel"):
        assert sentinel in text


def test_gpt_renderer_omits_empty_optional_sections() -> None:
    text = render(_MINIMAL_SPEC, ModelFamily.GPT)
    assert "# Role" in text and "# Task" in text
    for header in ("# Context", "# Constraints", "# Output Format", "# Examples", "# Verification"):
        assert header not in text


def test_reasoning_renderer_is_minimal_with_no_examples_or_cot(monkeypatch) -> None:
    _clear_lang_env(monkeypatch)
    text = render(_FULL_SPEC, ModelFamily.REASONING)
    assert "Критерии успеха" in text
    assert "Задача" in text
    assert "Ограничения" in text
    # No GPT-style "# Section" headers in the reasoning shape.
    assert "# " not in text
    # Examples are deliberately dropped for reasoning models.
    assert "example-sentinel" not in text
    # No chain-of-thought request; an explicit suppression instead.
    assert "думай" not in text.lower()
    assert "шаг за шагом" not in text.lower()
    assert "Верни только результат, без раскрытия рассуждений." in text
    # Role/context/task content still reaches the model.
    assert "role-sentinel" in text
    assert "task-sentinel" in text
    assert "context-sentinel" in text


def test_reasoning_renderer_respects_primary_language_env(monkeypatch) -> None:
    monkeypatch.setenv("PROMPT_PRIMARY_LANGUAGE", "en")
    text = render(_FULL_SPEC, ModelFamily.REASONING)
    assert "Success criteria" in text
    assert "Task:" in text
    assert "Return only the result, without revealing your reasoning." in text
    assert "Критерии успеха" not in text


def test_reasoning_renderer_omits_empty_sections() -> None:
    text = render(_MINIMAL_SPEC, ModelFamily.REASONING)
    assert "Ограничения" not in text
    assert "Критерии успеха" not in text
    assert "task-sentinel" in text
    assert "role-sentinel" in text  # role folds into the context block


def test_agentic_renderer_has_xml_shape_and_all_content() -> None:
    text = render(_FULL_SPEC, ModelFamily.AGENTIC)
    for tag in ("<role>", "<objective>", "<context>", "<constraints>",
                "<tool_policy>", "<output_format>"):
        assert tag in text
    for sentinel in ("role-sentinel", "task-sentinel", "context-sentinel",
                      "constraint-sentinel", "output-format-sentinel", "tool-policy-sentinel"):
        assert sentinel in text


def test_agentic_renderer_always_emits_tool_policy_tag_even_when_empty() -> None:
    text = render(_MINIMAL_SPEC, ModelFamily.AGENTIC)
    assert "<role>" in text
    assert "<tool_policy>" in text
    assert "<context>" not in text
    assert "<constraints>" not in text


def test_render_dispatches_by_family_default_is_gpt_shaped() -> None:
    """Sanity check that render() picks the right private renderer per family."""
    gpt_text = render(_MINIMAL_SPEC, ModelFamily.GPT)
    reasoning_text = render(_MINIMAL_SPEC, ModelFamily.REASONING)
    agentic_text = render(_MINIMAL_SPEC, ModelFamily.AGENTIC)
    assert gpt_text != reasoning_text != agentic_text
    assert "# Role" in gpt_text
    assert "<role>" in agentic_text
