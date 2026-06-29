"""Render a :class:`PromptSpec` into the system-prompt text for one model family.

Each family gets a structure adapted from the sources surveyed in the prompt-
contract plan, not a verbatim copy:

* GPT-style chat models read explicit ``# Section`` headers and a closing
  verification checklist; no chain-of-thought is requested.
* Reasoning/"thinking" models (qwen3, minimax, deepseek-R) get the opposite —
  a minimal brief (task/context/constraints/success/format) and an explicit
  instruction to return only the result, never the reasoning trace. Asking
  these models to "think step by step" or forcing tool_choice fights their
  native reasoning mode (400s — see ``doubleu-thinking-models-promptedoutput``).
* Claude-style agentic models get an XML role/objective/context/constraints/
  tool_policy/output_format shape, the dialect they are most reliably steered
  by.

Section labels are a structural choice baked into the prompt's shape, not a
per-deployment behavioural toggle — so unlike the family-resolution patterns,
they live in code.  ``PROMPT_PRIMARY_LANGUAGE`` (default ``"ru"``) selects
which fixed label set the reasoning renderer uses; GPT's ``# Section``
headers stay English (the conventional ASCII-header dialect for that family)
and the agentic renderer's XML tag names are language-neutral either way.
"""

from __future__ import annotations

import os

from .families import ModelFamily
from .spec import PromptSpec

_REASONING_LABELS: dict[str, dict[str, str]] = {
    "ru": {
        "task": "Задача",
        "context": "Контекст",
        "constraints": "Ограничения",
        "success": "Критерии успеха",
        "format": "Формат ответа",
        "footer": "Верни только результат, без раскрытия рассуждений.",
    },
    "en": {
        "task": "Task",
        "context": "Context",
        "constraints": "Constraints",
        "success": "Success criteria",
        "format": "Output format",
        "footer": "Return only the result, without revealing your reasoning.",
    },
}


def _reasoning_labels() -> dict[str, str]:
    lang = os.getenv("PROMPT_PRIMARY_LANGUAGE", "").strip().lower() or "ru"
    return _REASONING_LABELS.get(lang, _REASONING_LABELS["ru"])


def _bullets(items: tuple[str, ...]) -> str:
    return "\n".join(f"- {item}" for item in items if item)


def render(spec: PromptSpec, family: ModelFamily) -> str:
    """Render ``spec`` into system-prompt text shaped for ``family``."""
    if family is ModelFamily.REASONING:
        return _render_reasoning(spec)
    if family is ModelFamily.AGENTIC:
        return _render_agentic(spec)
    return _render_gpt(spec)


def _render_gpt(spec: PromptSpec) -> str:
    """Explicit ``# Section`` headers + a closing verification list, no CoT."""
    sections: list[str] = [f"# Role\n{spec.role}", f"# Task\n{spec.task}"]
    if spec.context:
        sections.append("# Context\n" + _bullets(spec.context))
    if spec.constraints:
        sections.append("# Constraints\n" + _bullets(spec.constraints))
    if spec.output_format:
        sections.append(f"# Output Format\n{spec.output_format}")
    if spec.examples:
        sections.append("# Examples\n" + _bullets(spec.examples))
    if spec.success_criteria:
        sections.append("# Verification\n" + _bullets(spec.success_criteria))
    return "\n\n".join(sections)


def _render_reasoning(spec: PromptSpec) -> str:
    """Minimal brief: task/context/constraints/success/format, no examples/CoT."""
    labels = _reasoning_labels()
    context_lines = "\n".join(line for line in (spec.role, *spec.context) if line)
    sections: list[str] = [f"{labels['task']}: {spec.task}"]
    if context_lines:
        sections.append(f"{labels['context']}:\n{context_lines}")
    if spec.constraints:
        sections.append(f"{labels['constraints']}:\n" + _bullets(spec.constraints))
    if spec.success_criteria:
        sections.append(f"{labels['success']}:\n" + _bullets(spec.success_criteria))
    if spec.output_format:
        sections.append(f"{labels['format']}: {spec.output_format}")
    sections.append(labels["footer"])
    return "\n\n".join(sections)


def _render_agentic(spec: PromptSpec) -> str:
    """XML role/objective/context/constraints/tool_policy/output_format shape."""
    sections: list[str] = [
        f"<role>\n{spec.role}\n</role>",
        f"<objective>\n{spec.task}\n</objective>",
    ]
    if spec.context:
        sections.append("<context>\n" + _bullets(spec.context) + "\n</context>")
    if spec.constraints:
        sections.append("<constraints>\n" + _bullets(spec.constraints) + "\n</constraints>")
    # Always present (even empty) so agentic models get a stable anchor for
    # tool-usage policy; the constraints section carries the actual rules
    # when tool_policy itself is just a short pointer to them.
    sections.append(f"<tool_policy>\n{spec.tool_policy}\n</tool_policy>")
    if spec.output_format:
        sections.append(f"<output_format>\n{spec.output_format}\n</output_format>")
    if spec.examples:
        sections.append("<examples>\n" + _bullets(spec.examples) + "\n</examples>")
    if spec.success_criteria:
        sections.append(
            "<success_criteria>\n" + _bullets(spec.success_criteria) + "\n</success_criteria>"
        )
    return "\n\n".join(sections)
