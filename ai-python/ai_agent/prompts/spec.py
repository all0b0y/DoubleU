"""Prompt Contract: the single intermediate format every agent prompt builds.

Replaces ad-hoc string concatenation (the old ``_compose_base_instruction``
"blob" in :mod:`ai_agent.pipelines.draft`) with an explicit Role/Task/Context/
Constraints/Output/Examples/Success contract.  A :class:`PromptSpec` is
family-agnostic: the same instance renders differently for GPT/reasoning/
agentic models (see :mod:`.renderers`) without the builder having to know
which family it is talking to.

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
    # Prohibitions/rules: no fabrication, no impersonation, output-format
    # mode, tool-usage rules (ask_questions, message-split, scheduler time).
    constraints: tuple[str, ...] = field(default_factory=tuple)
    # The reply's format contract (e.g. plain text vs Telegram Rich Markdown).
    output_format: str = ""
    # Few-shot examples (e.g. owner-approved/rejected past replies).
    examples: tuple[str, ...] = field(default_factory=tuple)
    # Criteria the renderer surfaces as "Verification"/"Критерии успеха".
    success_criteria: tuple[str, ...] = field(default_factory=tuple)
    # Agentic-only tool-usage framing; other families fold tool rules into
    # constraints instead (see renderers.py).
    tool_policy: str = ""
