"""TaskPreflightDecision: deterministic pre-call routing (Phase 2.7).

Before the orchestrator/writer LLM call, decide cheaply (no LLM round-trip)
what this turn actually needs: reasoning on/off and at what effort, whether
the orchestrator pre-stage should run at all, which native-tool groups are in
scope, and what request parameters the resolved model/provider can safely
receive. The Langfuse audit in ``.claude/analyze.md`` showed the two-stage
path calling tools "just in case" (``get_contact_profile``/``search_facts``
on a pure general-knowledge question) — this module is the gate that stops
that: a turn with no detected memory/schedule/interaction/mcp signal skips
the orchestrator pre-stage entirely rather than sending every enabled tool
"on the off chance".

Deliberately rule-based, not an LLM call: the source plan allows an
LLM-preflight fallback only when the rule-based signal is ambiguous, and the
ambiguous case here already has a safe default (fall through to the writer's
own single-stage toolset — see ``agents.runtime._draft_async``), so no LLM
round-trip is needed to stay correct.

This module only NARROWS what the existing capability/feature-flag gates in
``agents.runtime`` already allow (``_two_stage_draft``,
``_orchestrator_tool_calling_supported``, ``role_has_enabled_tools``) — it
never widens them. ``allowed_tool_names`` describes the ORCHESTRATOR's
tool scope specifically; the writer's own (separately gated, always-on
``send_message``) delivery capability and its single-stage fallback toolset
are unaffected by this module — see ``agents.runtime`` for that wiring.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Literal

from ..contract_enums import PrivacyFlag
from ..prompts.families import ModelFamily, resolve_family
from ..schemas import AIProcessMessageRequest
from .model_capabilities import ModelCapabilities, capabilities_for
from .tool_catalog import TOOL_CATALOG_BY_KEY, TOOL_NAME_TO_KEY, enabled_tool_names

ReasoningEffort = Literal["none", "minimal", "low", "medium", "high", "max"]
ToolGroup = Literal["memory", "schedule", "interaction", "delivery", "mcp"]

ALL_TOOL_GROUPS: frozenset[ToolGroup] = frozenset(
    {"memory", "schedule", "interaction", "delivery", "mcp"}
)

# Effort tiers ordered low-to-high so a requested effort can be clamped down
# to the nearest tier a capability profile's supported_reasoning_efforts
# actually advertises (see _clamp_effort).
_EFFORT_ORDER: tuple[ReasoningEffort, ...] = ("none", "minimal", "low", "medium", "high", "max")

# ToolCatalogEntry.category -> the preflight tool group it belongs to.
# ToolCategory.INTEGRATION has no catalog entries yet (the outbound-webhook
# `integration` tool is a separate, not-yet-built backlog track — see
# CLAUDE.md "Iteration 2" / docs/backlog.md), so it is intentionally absent.
_CATEGORY_TO_GROUP: dict[str, ToolGroup] = {"read": "memory", "schedule": "schedule", "interaction": "interaction"}

# Keyword/phrase signals. Pattern-matched substrings, not a brand/vendor list
# (mirrors prompts.families.resolve_family's own no-hardcoded-names stance) —
# these are intent signals in the CONTACT's own message, not model names.
_SCHEDULE_SIGNAL = re.compile(
    r"напомн|напомин|будильник|поставь.{0,12}(встреч|событ)|"
    r"\bremind\b|\breminder\b|set an? (alarm|reminder)|schedule a (call|meeting|reminder)",
    re.IGNORECASE,
)
_MEMORY_SIGNAL = re.compile(
    r"\bпомнишь\b|я (говорил|писал|рассказывал)|вспомни|в памяти|что я (говорил|писал|рассказывал)|"
    r"do you remember|what did i (say|tell|mention)|recall (what|that|when)",
    re.IGNORECASE,
)
_PLANNING_SIGNAL = re.compile(
    r"составь план|разработай план|пошагов|сравни|проанализируй|выработай стратеги|"
    r"step[- ]by[- ]step plan|create a plan|analy[sz]e|compare (the|these|options)",
    re.IGNORECASE,
)
_IDENTITY_SIGNAL = re.compile(
    r"кто ты\b|что ты умеешь|что ты можешь|расскажи о себе|"
    r"\bwho are you\b|what can you do|what do you do\b",
    re.IGNORECASE,
)


@dataclass(frozen=True)
class TaskPreflightDecision:
    """Deterministic routing decision made before the main LLM call.

    Every field is meant to actually constrain the runtime (see module
    docstring) — never just logged: ``needs_orchestrator``/``tool_groups``/
    ``allowed_tool_names`` gate ``agents.runtime._orchestrate_async``'s
    toolset, ``needs_reasoning``/``reasoning_effort`` feed
    :func:`model_settings_for`, and ``reason`` is the short internal trace
    string — never shown to the contact/owner, never chain-of-thought.
    """

    needs_reasoning: bool
    reasoning_effort: ReasoningEffort
    needs_orchestrator: bool
    tool_groups: frozenset[ToolGroup]
    allowed_tool_names: frozenset[str]
    structured_output_required: bool
    selected_model_slug: str
    prompt_shape_family: ModelFamily
    reason: str = ""


def _available_groups(request: AIProcessMessageRequest) -> frozenset[ToolGroup]:
    """Tool groups the role/privacy combination makes available at all.

    Reuses ``tool_catalog.enabled_tool_names`` (the exact filter the real
    toolset applies) rather than re-deriving role/privacy rules here, so a
    role with no assigned role (catalog defaults) still resolves memory tools
    as available, and a ``no_memory``/``private`` chat still blocks schedule.
    """
    names = enabled_tool_names(request.ai_role, request.chat_settings.privacy_flags)
    groups: set[ToolGroup] = set()
    for name in names:
        key = TOOL_NAME_TO_KEY.get(name)
        if key is None:
            continue
        category = TOOL_CATALOG_BY_KEY[key].category.value
        group = _CATEGORY_TO_GROUP.get(category)
        if group is not None:
            groups.add(group)
    return frozenset(groups)


def _clamp_effort(effort: ReasoningEffort, capabilities: ModelCapabilities) -> ReasoningEffort:
    """Clamp a desired effort down to the nearest tier the model actually supports.

    ``"none"`` is never clamped — disabling reasoning is a capability check
    (``reasoning_mandatory``), not an effort-tier question. An empty
    ``supported_reasoning_efforts`` means the capability profile does not
    constrain effort (e.g. unknown/unparsed metadata), so the request passes
    through unclamped.
    """
    if effort in ("none",) or not capabilities.supported_reasoning_efforts:
        return effort
    if effort in capabilities.supported_reasoning_efforts:
        return effort
    idx = _EFFORT_ORDER.index(effort)
    for candidate in reversed(_EFFORT_ORDER[: idx + 1]):
        if candidate in capabilities.supported_reasoning_efforts:
            return candidate  # type: ignore[return-value]
    return capabilities.supported_reasoning_efforts[0]  # type: ignore[return-value]


def decide_preflight(
    request: AIProcessMessageRequest,
    *,
    orchestrator_slug: str,
    writer_slug: str,
) -> TaskPreflightDecision:
    """Decide the routing for one draft turn from message/role/privacy state.

    ``orchestrator_slug``/``writer_slug`` are the concrete model slugs that
    will actually serve each stage (same resolution
    ``agents.runtime._draft_async`` already applies via
    ``models.resolve_model_slug`` — passed in rather than re-resolved here so
    this stays a pure function of its inputs).
    """
    text = request.event.message_text or ""
    role = request.ai_role
    privacy_flags = request.chat_settings.privacy_flags
    reasons: list[str] = []

    prompt_shape_family = resolve_family(writer_slug)
    writer_caps = capabilities_for(writer_slug)
    orchestrator_caps = capabilities_for(orchestrator_slug)

    available = _available_groups(request)
    schedule_signal = bool(_SCHEDULE_SIGNAL.search(text))
    memory_signal = bool(_MEMORY_SIGNAL.search(text))
    planning_signal = bool(_PLANNING_SIGNAL.search(text))
    identity_signal = bool(_IDENTITY_SIGNAL.search(text))

    groups: set[ToolGroup] = set()
    if identity_signal:
        # Confident "trusted-context already answers this" signal overrides
        # whatever the role enables — see acceptance criterion #1 ("кто ты и
        # что умеешь" -> no read tools even when the role has memory tools on
        # by catalog default).
        reasons.append("identity/capability smalltalk overrides role-enabled tools")
    else:
        if schedule_signal and "schedule" in available:
            groups.add("schedule")
            reasons.append("schedule-intent keyword")
        if memory_signal and "memory" in available:
            groups.add("memory")
            reasons.append("memory-recall keyword")
        if "interaction" in available:
            # interaction.ask_questions is opt-in (default_enabled=False): an
            # owner who turned it on for this role already signalled the role
            # may need to clarify before answering. WHETHER this specific
            # turn needs a question is the orchestrator/writer's own
            # judgment call — unlike memory/schedule, that need has no
            # reliable lexical marker in the contact's message, so it is not
            # keyword-gated the way schedule/memory are.
            groups.add("interaction")
            reasons.append("role opts into interaction.ask_questions")

    mcp_allowed = (
        role is not None
        and bool(role.mcp_servers)
        and PrivacyFlag.NO_MEMORY not in privacy_flags
        and PrivacyFlag.PRIVATE not in privacy_flags
    )
    if mcp_allowed and groups:
        # MCP only rides along with an already-triggered context-gathering
        # turn — see module docstring: a role merely HAVING an mcp_servers
        # entry must not by itself wake the orchestrator for a question the
        # role's own builtin tools already answer (e.g. "кто ты").
        groups.add("mcp")
        reasons.append("role declares mcp_servers, rides along with this turn's context-gathering")

    # delivery (send_message) is the writer's always-on capability — never
    # gated by tool_settings — included here only for observability
    # completeness (see ALL_TOOL_GROUPS); it never widens needs_orchestrator.
    groups.add("delivery")

    needs_orchestrator = bool(groups - {"delivery"})

    # Provider/capability guard (2.5/2.6): a context-gathering stage whose
    # model cannot reliably emit tool-call JSON must not run tool-heavy
    # regardless of the keyword signals above.
    if needs_orchestrator and not (
        orchestrator_caps.supports_tool_choice
        and orchestrator_caps.json_tool_reliability != "blocked"
    ):
        reasons.append(
            f"orchestrator model family={orchestrator_caps.prompt_shape_family.value} "
            f"json_tool_reliability={orchestrator_caps.json_tool_reliability} — tool-heavy path disabled"
        )
        needs_orchestrator = False
        groups -= {"memory", "schedule", "interaction", "mcp"}

    # allowed_tool_names is the ORCHESTRATOR stage's scope only (memory/
    # schedule/interaction native tools). "send_message" is deliberately
    # never added here: it lives in the structurally separate, always-on
    # delivery_toolset (see agents.runtime._draft_async), which is never
    # filtered by this set — including it here would claim a gate that does
    # not actually exist for it.
    allowed_tool_names: set[str] = set()
    if needs_orchestrator:
        enabled_names = enabled_tool_names(role, privacy_flags)
        for name in enabled_names:
            key = TOOL_NAME_TO_KEY.get(name)
            if key is None:
                continue
            group = _CATEGORY_TO_GROUP.get(TOOL_CATALOG_BY_KEY[key].category.value)
            if group is not None and group in groups:
                allowed_tool_names.add(name)

    multi_group = len(groups - {"delivery"}) >= 2
    if identity_signal and not needs_orchestrator:
        needs_reasoning = False
        effort: ReasoningEffort = "none"
        reasons.append("identity/capability smalltalk — direct trusted-context answer")
    elif planning_signal or multi_group:
        needs_reasoning = True
        effort = "medium"
        reasons.append("planning/multi-step or multi-tool-group turn")
    elif "schedule" in groups:
        needs_reasoning = True
        effort = "low"
        reasons.append("scheduling needs careful but single-step time reasoning")
    elif needs_orchestrator:
        needs_reasoning = True
        effort = "low"
        reasons.append("context-gathering turn — light verification pass")
    else:
        needs_reasoning = False
        effort = "minimal"
        reasons.append("direct trusted-context answer, no tools")

    effort = _clamp_effort(effort, writer_caps)
    if writer_caps.reasoning_mandatory:
        needs_reasoning = True
        if effort == "none":
            effort = _clamp_effort("low", writer_caps)
        reasons.append("writer model always reasons (reasoning_mandatory) — cannot disable")
    elif not writer_caps.supports_reasoning:
        needs_reasoning = False
        effort = "none"
        reasons.append("writer model has no reasoning support — effort forced to none")

    return TaskPreflightDecision(
        needs_reasoning=needs_reasoning,
        reasoning_effort=effort,
        needs_orchestrator=needs_orchestrator,
        tool_groups=frozenset(groups),
        allowed_tool_names=frozenset(allowed_tool_names),
        structured_output_required=True,
        selected_model_slug=writer_slug,
        prompt_shape_family=prompt_shape_family,
        reason="; ".join(reasons)[:500],
    )


def model_settings_for(
    decision: TaskPreflightDecision, capabilities: ModelCapabilities
) -> dict[str, object]:
    """Build the provider-safe ``pydantic_ai.settings.ModelSettings`` dict.

    Only ever returns keys the resolved model/provider is known to accept —
    the request-parameter half of the 2.7 provider/capability guard. Uses
    pydantic-ai's own unified ``thinking`` setting (translated per-provider,
    including OpenRouter, by the model class itself — see
    ``OPENAI_REASONING_EFFORT_MAP`` in ``pydantic_ai.profiles.openai``)
    rather than hand-rolling a raw ``reasoning``/``reasoning_effort`` body,
    so this never has to track each provider's wire format itself.
    """
    settings: dict[str, object] = {}
    if capabilities.supports_reasoning:
        if decision.reasoning_effort == "none":
            if not capabilities.reasoning_mandatory:
                settings["thinking"] = False
            # else: mandatory-reasoning model — leave unset, provider default applies.
        elif decision.needs_reasoning:
            settings["thinking"] = "high" if decision.reasoning_effort == "max" else decision.reasoning_effort
    if capabilities.supports_parallel_tool_calls:
        settings["parallel_tool_calls"] = len(decision.tool_groups - {"delivery"}) > 1
    return settings
