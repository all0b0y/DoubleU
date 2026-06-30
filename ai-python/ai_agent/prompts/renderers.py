"""Render a :class:`PromptSpec` into the system-prompt text for one model family.

Every family renders the SAME ``<prompt_contract>`` XML envelope — Claude's
prompting guidance (clear tag-delimited sections) generalises to every model
shape, not just XML-native ones, so the envelope is not a per-provider
special case.  Family only changes the envelope's *density*:

* Reasoning/"thinking" models (qwen3, minimax, deepseek-R) get the envelope
  with no ``<examples>`` and an extra trailing constraint that forbids
  revealing the reasoning trace.  Asking these models to "think step by
  step" or forcing tool_choice fights their native reasoning mode (400s —
  see ``doubleu-thinking-models-promptedoutput``), so neither appears here.
* GPT-style and Claude-style agentic models get the full envelope, including
  ``<examples>`` when the spec carries any (owner-approved style samples).
  Today both shapes render identically; the family split exists so each can
  diverge later (e.g. agentic-specific tool workflow framing) without
  touching the other.

Section labels are a structural choice baked into the prompt's shape, not a
per-deployment behavioural toggle — so unlike the family-resolution patterns,
they live in code.  ``PROMPT_PRIMARY_LANGUAGE`` (default ``"ru"``) selects
which fixed language the reasoning footer uses; tag names themselves are
language-neutral.
"""

from __future__ import annotations

import os

from .families import ModelFamily
from .spec import PromptSpec

_REASONING_FOOTER: dict[str, str] = {
    "ru": "Верни только результат, без раскрытия рассуждений.",
    "en": "Return only the result, without revealing your reasoning.",
}


def _reasoning_footer() -> str:
    lang = os.getenv("PROMPT_PRIMARY_LANGUAGE", "").strip().lower() or "ru"
    return _REASONING_FOOTER.get(lang, _REASONING_FOOTER["ru"])


def render(spec: PromptSpec, family: ModelFamily) -> str:
    """Render ``spec`` into the unified ``<prompt_contract>`` XML envelope.

    ``family`` only changes density (examples included or not, the reasoning
    no-CoT constraint) — see the module docstring.
    """
    if family is ModelFamily.REASONING:
        return _render_reasoning(spec)
    if family is ModelFamily.AGENTIC:
        return _render_agentic(spec)
    return _render_gpt(spec)


def _tag(name: str, inner: str) -> str:
    return f"<{name}>\n{inner}\n</{name}>" if inner else f"<{name}></{name}>"


def _item_list(item_tag: str, values: tuple[str, ...]) -> str:
    return "\n".join(f"<{item_tag}>{value}</{item_tag}>" for value in values)


def _output_format_tag(spec: PromptSpec) -> str:
    inner = f"<text_format>{spec.output_format.text_format}</text_format>"
    if spec.output_format.rules:
        inner += f"\n<rules>\n{spec.output_format.rules}\n</rules>"
    return _tag("output_format", inner)


def _envelope(
    spec: PromptSpec, *, extra_constraints: tuple[str, ...] = (), include_examples: bool
) -> str:
    """Assemble the shared ``<prompt_contract>`` skeleton.

    Every section tag is always present (empty tag when there is nothing to
    say) so a model sees a stable structure turn over turn; only
    ``<examples>`` is omitted entirely rather than emitted empty, since an
    empty examples block invites the model to wonder what belongs there.
    """
    sections = [
        _tag("role", spec.role),
        _tag("task", spec.task),
        _tag("trusted_context", _item_list("item", spec.context)),
        _tag("constraints", _item_list("constraint", spec.constraints + extra_constraints)),
        _tag("tool_policy", spec.tool_policy),
        _output_format_tag(spec),
    ]
    if include_examples and spec.examples:
        sections.append(_tag("examples", _item_list("example", spec.examples)))
    sections.append(_tag("success_criteria", _item_list("criterion", spec.success_criteria)))
    return "<prompt_contract>\n" + "\n".join(sections) + "\n</prompt_contract>"


def _render_gpt(spec: PromptSpec) -> str:
    """Full envelope, examples included, no chain-of-thought request."""
    return _envelope(spec, include_examples=True)


def _render_reasoning(spec: PromptSpec) -> str:
    """Minimal envelope: no examples, plus an explicit no-reasoning-trace constraint."""
    return _envelope(spec, extra_constraints=(_reasoning_footer(),), include_examples=False)


def _render_agentic(spec: PromptSpec) -> str:
    """Full envelope; ``<tool_policy>`` is the section tool-heavy models lean on most."""
    return _envelope(spec, include_examples=True)
