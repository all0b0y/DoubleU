"""Prompt Contract: the single intermediate format every agent prompt builds.

Replaces ad-hoc string concatenation (the old ``_compose_base_instruction``
blob, removed once every caller migrated to this contract) with an explicit
Role/Task/Context/Constraints/Output/Examples/Success contract.  A
:class:`PromptSpec` is family-agnostic: the same instance renders into the
same ``<prompt_contract>`` XML envelope for GPT/reasoning/agentic models (see
:mod:`.renderers`) — only the envelope's density (examples included or not,
an extra no-chain-of-thought constraint) varies by family.

``context`` is trusted-only by construction: callers populate it from
situational data the backend already trusts (role description, contact
profile, owner identity, greeting cadence, current time).  Untrusted text —
the contact's message, conversation memory — never belongs here; it stays in
the separate envelope :class:`ai_agent.security.PromptBuilder` wraps in
``<untrusted_*>`` tags. This split is the load-bearing security invariant of
the whole prompt-contract package, so it is documented on every field below
rather than left implicit.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal

# Telegram-facing reply format the model must follow. Mirrors
# ``ai_agent.pipelines.draft.TextFormat`` ("plain"/"rich") but spelled out for
# the model rather than the wire contract — see ``decide_text_format``.
TextFormatKind = Literal["plain_text", "telegram_rich_markdown"]


@dataclass(frozen=True)
class OutputFormat:
    """The reply-format contract rendered as ``<output_format>``.

    ``rules`` is free text (e.g. which GFM elements are allowed, heading
    spacing rules); empty for tasks that produce no contact-facing text
    (orchestrator, critic, extraction, ...), where only the conservative
    ``text_format`` default matters.
    """

    text_format: TextFormatKind = "plain_text"
    rules: str = ""


@dataclass(frozen=True)
class PromptSpec:
    """Task contract for one agent call, rendered per :class:`families.ModelFamily`.

    Field semantics (mirrors the Role/Task/Context/Constraints/Output/
    Examples/Success structure the renderers project per family):
    """

    # Who the model is for this turn (persona/frame) — e.g. "speak as the
    # owner" vs "speak as the assigned bot persona".
    role: str
    # What to do this turn — the single concrete instruction.
    task: str
    # Trusted situational input: role description, contact profile, owner
    # identity, greeting cadence, current time.  NEVER untrusted text.
    context: tuple[str, ...] = field(default_factory=tuple)
    # Prohibitions/rules: no fabrication, no impersonation, tool-usage rules
    # (ask_questions, message-split, scheduler time). Output-format mode lives
    # in ``output_format`` below, not here.
    constraints: tuple[str, ...] = field(default_factory=tuple)
    # The reply's format contract (plain text vs Telegram Rich Markdown).
    output_format: OutputFormat = field(default_factory=OutputFormat)
    # Few-shot examples (e.g. owner-approved/rejected past replies).
    examples: tuple[str, ...] = field(default_factory=tuple)
    # Criteria the renderer surfaces as "Verification"/"Критерии успеха".
    success_criteria: tuple[str, ...] = field(default_factory=tuple)
    # Agentic-only tool-usage framing; other families fold tool rules into
    # constraints instead (see renderers.py).
    tool_policy: str = ""
