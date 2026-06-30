"""Agent runtime: the pydantic-ai execution layer behind the AI service.

Bridges the synchronous HTTP service to the async agents through one
dedicated event-loop thread (also home to the asyncpg pool, which must live
on a single loop).  Every LLM-bound endpoint (draft, typed extraction,
summarization, topic tagging, fact reranking) runs here — there is no longer a
hand-rolled ModelRouter path; the agents own all prompting and the output
schema is enforced natively by pydantic-ai.

Hot-path shape: the draft agent answers with native structured output
(:class:`DraftResult`); when the risk is medium+ or the draft's own
confidence is low, the critic agent reviews the draft and may revise it
before the backend's DecisionEngine sees it.  Policy gates (disabled/silent,
critical risk) stay in :func:`pipelines.draft.preflight_outcome`.

Safety and observability, which used to live inside ``ModelRouter.complete``,
are re-homed here: the Phase 1 ``InputSafetyScanner`` runs on the prompt before
every ``agent.run`` (and can short-circuit to a canned reply), the Phase 3
``OutputSafetyScanner`` runs on the generated text, and the ``LangfuseSink``
records one generation per model call.  Because the agents execute on a
separate event-loop thread, the per-request :class:`LLMCallContext` is captured
in the calling (HTTP handler) thread and threaded explicitly into the
coroutines so the scanners read/write the same context object the handler sees.
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import threading
import warnings
from datetime import datetime, timezone
from typing import Awaitable, Callable, Sequence, TypeVar

import asyncpg
from pydantic_ai import Agent, PromptedOutput, RunContext, UsageLimits
from pydantic_ai.toolsets import CombinedToolset, FilteredToolset

from ..classifiers import TypedMemoryExtractor
from ..observability.context import LLMCallContext, current_context
from ..observability.langfuse_sink import LangfuseSink
from ..observability.results import SafetyBlocked
from ..pipelines import draft as draft_pipeline
from ..pipelines.embedding import embed_texts
from ..prompts import library as prompt_library
from ..prompts.families import resolve_family
from ..prompts.renderers import render as render_prompt
from ..safety.input import InputSafetyScanner
from ..safety.output import OutputSafetyScanner
from ..schemas import (
    AIProcessMessageRequest,
    AIRoleCustomFieldData,
    AIRoleData,
    ChatSummaryRequest,
    ChatSummaryResponse,
    DraftRoleRequest,
    DraftRoleResponse,
    EmbedTextsRequest,
    ModelTask,
    OutboundMessageData,
    PrivacyFlag,
    ProfileSuggestion,
    RerankFactsRequest,
    RerankFactsResponse,
    RiskLevel,
    QuestionData,
    ResponsePlanData,
    ScheduledJobData,
    SynthesizePatternRequest,
    SynthesizePatternResponse,
    TopicTaggingRequest,
    TopicTaggingResponse,
    TypedExtractionRequest,
    TypedExtractionResponse,
    TypedFact,
    Usage,
)
from ..security import PromptBuilder
from .dedup_toolset import DedupToolset
from .deps import AgentDeps
from .delivery_tools import delivery_toolset
from .interaction_tools import interaction_toolset
from .memory_tools import memory_toolset
from .model_capabilities import CAPABILITY_PROFILE_VERSION, capabilities_for, provider_for
from .models import (
    AgentTask,
    ModelTier,
    build_model,
    build_named_model,
    build_tier_model,
    model_name_for,
    model_name_for_tier,
    resolve_model_slug,
)
from .preflight import TaskPreflightDecision, decide_preflight, model_settings_for
from .schedule_tools import schedule_toolset
from .tool_catalog import TOOL_CATALOG, enabled_tool_names
from .results import (
    CriticVerdict,
    DraftResult,
    ExtractionResult,
    OrchestratorResult,
    PatternResult,
    RerankResult,
    RoleDraft,
    ScheduledJob,
    SummaryResult,
    TopicResult,
)

logger = logging.getLogger(__name__)

T = TypeVar("T")

# Async query-embedding callable shape expected by AgentDeps.embedder.
EmbedderCallable = Callable[[str], Awaitable[Sequence[float] | None]]


def resolve_builtin_toolset(
    ai_role: AIRoleData | None,
    privacy_flags: tuple[PrivacyFlag, ...] = (),
    *,
    allowed_names: frozenset[str] | None = None,
) -> DedupToolset:
    """Scope native tools to role overrides while preserving catalog defaults.

    ``allowed_names``, when given, further restricts the role-enabled set —
    used by the orchestrator stage to apply ``TaskPreflightDecision.
    allowed_tool_names`` (2.7) on top of the role/privacy gate, so a role
    that enables six memory tools still only exposes the ones this turn's
    preflight decided are actually relevant. ``None`` (the writer's
    single-stage fallback path) keeps the full role-enabled set.

    Wrapped in :class:`DedupToolset` so a repeat call with the same
    ``(tool_name, args)`` within one run (orchestrator or single-stage writer)
    returns the cached result instead of hitting the DB again — see its
    module docstring for why this is safe unconditionally.
    """
    names = enabled_tool_names(ai_role, privacy_flags)
    if allowed_names is not None:
        names = names & allowed_names
    filtered = FilteredToolset(
        CombinedToolset([memory_toolset, interaction_toolset, schedule_toolset]),
        lambda ctx, tool_def, enabled_names=names: tool_def.name in enabled_names,
    )
    return DedupToolset(wrapped=filtered)


def role_has_enabled_tools(
    ai_role: AIRoleData | None,
    privacy_flags: tuple[PrivacyFlag, ...] = (),
) -> bool:
    """Return whether a role enables any catalog or MCP tool."""
    if ai_role is None:
        return False
    settings = ai_role.tool_settings
    scheduler_blocked = any(
        flag in (PrivacyFlag.NO_MEMORY, PrivacyFlag.PRIVATE) for flag in privacy_flags
    )
    builtin_enabled = any(
        settings.get(entry.key, entry.default_enabled)
        and not (scheduler_blocked and entry.category.value == "schedule")
        for entry in TOOL_CATALOG
    )
    return builtin_enabled or bool(ai_role.mcp_servers)


def _scheduled_job_data(jobs: list[ScheduledJob]) -> tuple[ScheduledJobData, ...]:
    """Convert scheduler tool accumulators into the process-message wire shape."""
    return tuple(
        ScheduledJobData(
            kind=job.kind,
            target=job.target,
            fire_at=job.fire_at.isoformat(),
            payload=dict(job.payload),
            created_by=job.created_by,
            idempotency_key=job.idempotency_key,
        )
        for job in jobs
    )


# A draft is reviewed by the critic when risk is elevated or the draft's own
# confidence is below this bar; low-risk confident drafts skip the extra call.
_CRITIC_CONFIDENCE_BAR = 0.6
_CRITIC_RISKS = (RiskLevel.MEDIUM, RiskLevel.HIGH)

# Upper bound on memory tool-calls the draft agent may make in one run. The
# draft agent owns a 6-tool memory toolset and would otherwise loop unbounded
# (ReAct failure mode). The cap counts TOTAL calls, not repeats, so it must sit
# above one legitimate full sweep of the toolset. A memory-heavy turn (large
# context + scheduler/interaction tools) legitimately needs more than one sweep,
# so the default is 12 — high enough that normal turns never trip it, low enough
# to still kill a runaway loop (which runs into the dozens). Env-tunable.
_DRAFT_TOOL_CALLS_LIMIT = int(os.getenv("DRAFT_TOOL_CALLS_LIMIT", "12").strip() or "12")

# Tighter ceiling for the orchestrator specifically (2.5 call budget): its job
# is bounded by design — gather only what the writer is missing — so a
# runaway loop should be killed well before the writer's 12-call backstop. The
# source plan's target is request-shape-dependent (<=3 for a simple role/
# capability question, <=5 for a memory/history-heavy one); without a
# deterministic way to tell those apart yet (that classification is
# TaskPreflightDecision, phase 2.7, not implemented), 5 is used as a single
# safe ceiling — the upper end of the plan's range, not a forced minimum: a
# simple turn that finishes in 1-2 calls is never artificially padded out.
_ORCHESTRATOR_TOOL_CALLS_LIMIT = int(
    os.getenv("ORCHESTRATOR_TOOL_CALLS_LIMIT", "5").strip() or "5"
)

# Two-stage default: the orchestrator owns memory/schedule/interaction tools
# (see _orchestrate_async); the writer gets only its always-on delivery
# capability, not a fallback copy of the same toolset. A Langfuse audit
# (.claude/analyze.md) showed the fallback path masking orchestration bugs —
# the writer would quietly re-fetch context the orchestrator already gathered
# instead of the bug surfacing as a missing-context failure. Set this to
# enable the old "writer keeps the role's tools as a backup path" behaviour
# for a deployment that needs it; single-stage drafting (no orchestrator run)
# is never affected — the writer is the only tool-caller there regardless.
_WRITER_FALLBACK_TOOLS_ENABLED = os.getenv("WRITER_FALLBACK_TOOLS_ENABLED", "").strip().lower() in (
    "1", "true", "yes", "on",
)

# How many times draft_outcome re-runs the agent before giving up. A draft can
# fail transiently — provider 5xx/rate-limit, a malformed structured output, or
# the tool-call cap — and a fresh attempt usually succeeds. After this many
# failures the service returns error_fallback and the Go backend delivers a
# user-facing "temporary problem" notice so the contact is never left in silence.
_DRAFT_MAX_ATTEMPTS = max(1, int(os.getenv("DRAFT_MAX_ATTEMPTS", "3").strip() or "3"))

def _build_query_embedder() -> EmbedderCallable:
    """Build a per-run, memoized async embedder over the sync embed transport.

    A fresh closure (and cache) is created for each agent run so repeated
    ``search_facts`` tool-calls within one run reuse a single embed call and the
    cache never outlives the request. Embedding is best-effort: any failure (no
    credentials, transport error, empty vector) yields ``None`` so the memory
    tool transparently falls back to lexical (ILIKE) search.
    """
    cache: dict[str, Sequence[float] | None] = {}

    async def embed(query: str) -> Sequence[float] | None:
        key = query.strip()
        if not key:
            return None
        if key in cache:
            return cache[key]
        try:
            # embed_texts is sync (urllib); run off the event loop so the agent
            # loop is never blocked on the network round-trip.
            resp = await asyncio.to_thread(embed_texts, EmbedTextsRequest(texts=(key,)))
            vector = resp.embeddings[0] if resp.embeddings else ()
            result: Sequence[float] | None = tuple(vector) if vector else None
        except Exception:  # noqa: BLE001 — embedding is best-effort, fall back to text search
            result = None
        cache[key] = result
        return result

    return embed

# Number of recent messages topic tagging includes as context.
_TOPIC_MAX_HISTORY = 5


class AgentRuntime:
    """Owns the agents, the event-loop thread, safety/observability and the pool."""

    def __init__(
        self,
        *,
        prompt_builder: PromptBuilder | None = None,
        database_url: str = "",
        run_timeout_seconds: float = 90.0,
        draft_max_attempts: int = _DRAFT_MAX_ATTEMPTS,
        input_safety: InputSafetyScanner | None = None,
        output_safety: OutputSafetyScanner | None = None,
        sink: LangfuseSink | None = None,
        two_stage_draft: bool = False,
        orchestrator_model: object | None = None,
        draft_model: object | None = None,
        critic_model: object | None = None,
        extract_model: object | None = None,
        summary_model: object | None = None,
        topic_model: object | None = None,
        rerank_model: object | None = None,
        pattern_model: object | None = None,
        role_draft_model: object | None = None,
        query_embedder_factory: Callable[[], EmbedderCallable] | None = None,
    ) -> None:
        """Build agents and start the loop thread.

        Models default to the env-configured factory; tests inject
        FunctionModel/TestModel instances for deterministic behaviour.  The
        safety scanners and the Langfuse sink are injected by the HTTP factory
        (``create_service_from_env``); when ``None`` the corresponding stage is
        a no-op, so direct callers and tests see plain agent behaviour.
        """
        self._prompt_builder = prompt_builder or PromptBuilder()
        self._database_url = database_url
        self._timeout = run_timeout_seconds
        self._draft_max_attempts = max(1, draft_max_attempts)
        self._input_safety = input_safety
        self._output_safety = output_safety
        self._sink = sink
        self._two_stage_draft = two_stage_draft
        self._pool: asyncpg.Pool | None = None
        # Builds a fresh per-request embedder; None keeps memory tools on the
        # lexical fallback (preserved behaviour for direct callers and tests).
        self._query_embedder_factory = query_embedder_factory

        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(
            target=self._loop.run_forever, name="agent-runtime-loop", daemon=True
        )
        self._thread.start()

        # retries=2 (not the default 1): a 2.5 Langfuse audit (.claude/analyze.md)
        # caught a model emitting malformed tool-call JSON ("{}\"\"") for a
        # no-arg tool TWICE in a row before a valid call — exactly 3 attempts,
        # which exceeds a 1-retry (2-attempt) budget and fails the whole
        # orchestrator run, forcing draft_outcome's outer retry to redo BOTH
        # stages from scratch. Same rationale already applied to the draft
        # agent below for the same "malformed JSON answer" failure mode.
        self._orchestrator_agent: Agent[AgentDeps, OrchestratorResult] = Agent(
            orchestrator_model or build_model(AgentTask.ORCHESTRATE),
            deps_type=AgentDeps,
            output_type=OrchestratorResult,
            retries=2,
        )

        # Every agent's instructions are rendered per-request (model family can
        # only be known once the request's slug is resolved), so each agent
        # gets a dynamic hook instead of a static `instructions=` string — see
        # the per-task `_x_async` methods below, which render the matching
        # `prompts.library` spec into `deps.system_instruction` before `run()`.
        @self._orchestrator_agent.instructions
        def _orchestrator_context(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        # PromptedOutput (not the default ToolOutput) is required because per-role
        # tier models can be reasoning/"thinking" models (qwen3, minimax), which
        # reject the forced `tool_choice=required` that ToolOutput sends to coerce
        # the final_result call. PromptedOutput instead asks for the JSON in-prompt
        # and parses it from plain text — no tool_choice, so it works on every
        # model. retries=2 keeps one malformed JSON answer from failing the request.
        self._draft_agent: Agent[AgentDeps, DraftResult] = Agent(
            draft_model or build_model(AgentTask.DRAFT),
            deps_type=AgentDeps,
            output_type=PromptedOutput(DraftResult),
            retries=2,
        )

        @self._draft_agent.instructions
        def _draft_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._critic_agent: Agent[AgentDeps, CriticVerdict] = Agent(
            critic_model or build_model(AgentTask.CRITIC),
            deps_type=AgentDeps,
            output_type=CriticVerdict,
            retries=1,
        )

        @self._critic_agent.instructions
        def _critic_context(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._warn_if_tool_output_unsupported("orchestrator", model_name_for(AgentTask.ORCHESTRATE))
        self._warn_if_tool_output_unsupported("critic", model_name_for(AgentTask.CRITIC))
        orchestrator_slug = model_name_for(AgentTask.ORCHESTRATE)
        self._orchestrator_tool_calling_supported = self._orchestrator_supports_tool_calling(
            orchestrator_slug
        )
        if not self._orchestrator_tool_calling_supported:
            logger.warning(
                "orchestrator model %r cannot run the tool-heavy path (incompatible family or "
                "MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES) — two-stage drafting falls "
                "back to single-stage for every request until this is reconfigured",
                orchestrator_slug,
            )

        self._extraction_agent: Agent[AgentDeps, ExtractionResult] = Agent(
            extract_model or build_model(AgentTask.EXTRACT),
            deps_type=AgentDeps,
            output_type=ExtractionResult,
            retries=1,
        )

        @self._extraction_agent.instructions
        def _extraction_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._summary_agent: Agent[AgentDeps, SummaryResult] = Agent(
            summary_model or build_model(AgentTask.SUMMARIZE),
            deps_type=AgentDeps,
            output_type=SummaryResult,
            retries=1,
        )

        @self._summary_agent.instructions
        def _summary_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._topic_agent: Agent[AgentDeps, TopicResult] = Agent(
            topic_model or build_model(AgentTask.TOPIC),
            deps_type=AgentDeps,
            output_type=TopicResult,
            retries=1,
        )

        @self._topic_agent.instructions
        def _topic_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._rerank_agent: Agent[AgentDeps, RerankResult] = Agent(
            rerank_model or build_model(AgentTask.RERANK),
            deps_type=AgentDeps,
            output_type=RerankResult,
            retries=1,
        )

        @self._rerank_agent.instructions
        def _rerank_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._pattern_agent: Agent[AgentDeps, PatternResult] = Agent(
            pattern_model or build_model(AgentTask.REFLECT),
            deps_type=AgentDeps,
            output_type=PatternResult,
            retries=1,
        )

        @self._pattern_agent.instructions
        def _pattern_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._role_draft_agent: Agent[AgentDeps, RoleDraft] = Agent(
            role_draft_model or build_model(AgentTask.ROLE_DRAFT),
            deps_type=AgentDeps,
            output_type=RoleDraft,
            retries=1,
        )

        @self._role_draft_agent.instructions
        def _role_draft_agent_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

    # ------------------------------------------------------------------ infra

    def _run(self, coro: Awaitable[T]) -> T:
        """Execute a coroutine on the runtime loop from sync handler threads."""
        future = asyncio.run_coroutine_threadsafe(coro, self._loop)
        return future.result(timeout=self._timeout)

    async def _get_pool(self) -> asyncpg.Pool | None:
        """Lazily create the asyncpg pool on the runtime loop (its home loop)."""
        if not self._database_url:
            return None
        if self._pool is None:
            self._pool = await asyncpg.create_pool(
                self._database_url, min_size=1, max_size=5, command_timeout=10
            )
        return self._pool

    def _deps_for(
        self,
        tenant_id: str,
        chat_id: str,
        privacy_flags: tuple[PrivacyFlag, ...],
        *,
        source_message_id: str = "",
        system_instruction: str = "",
        pool: asyncpg.Pool | None,
        current_time: datetime | None = None,
        existing_jobs: tuple = (),
    ) -> AgentDeps:
        return AgentDeps(
            tenant_id=tenant_id,
            chat_id=chat_id,
            privacy_flags=tuple(f.value for f in privacy_flags),
            source_message_id=source_message_id,
            system_instruction=system_instruction,
            pool=pool,
            embedder=self._query_embedder_factory() if self._query_embedder_factory else None,
            current_time=current_time or datetime.now(timezone.utc),
            existing_jobs=existing_jobs,
        )

    # ----------------------------------------------------------- safety/sink

    def _scan_input(
        self, system_prompt: str, user_payload: str, task: ModelTask, ctx: LLMCallContext
    ) -> tuple[str, str]:
        """Phase 1 gate before ``agent.run``; raises SafetyBlocked when blocked.

        No-op when no scanner is wired or the flag is off (the scanner's own
        ``enabled`` guard), so direct callers and tests are unaffected.
        """
        if self._input_safety is None:
            return system_prompt, user_payload
        return self._input_safety.scan(system_prompt, user_payload, task, ctx)

    def _scan_output(self, text: str, task: ModelTask, ctx: LLMCallContext) -> tuple[str, bool]:
        """Phase 3 gate on generated text; returns (text, flagged)."""
        if self._output_safety is None:
            return text, False
        return self._output_safety.scan(text, task, ctx)

    def _record_run(
        self,
        ctx: LLMCallContext,
        task: AgentTask,
        system_prompt: str,
        user_payload: str,
        completion: str,
        usage: Usage,
        privacy_flags: tuple[PrivacyFlag, ...],
        *,
        model_name_override: str = "",
        preflight: TaskPreflightDecision | None = None,
        request_model_settings: dict[str, object] | None = None,
        tool_call_count: int | None = None,
    ) -> None:
        """Push one generation (and trace/safety spans) to the Langfuse sink.

        Mirrors the recording that used to live in ``ModelRouter._record_to_sink``
        so the agent path produces the same observability as the legacy path did.
        ``record_generation`` hashes the prompt when ``no_training`` is set.

        2.6: also attaches capability/routing metadata (model slug, provider,
        prompt-shape family, capability profile version, a hash — never the
        raw text — of the rendered system prompt) so a trace can answer "what
        family/capabilities did the model that actually ran resolve to" and
        "did the rendered prompt change between two runs" without re-reading
        the full (possibly truncated) prompt field.

        2.7: ``preflight``, when given, attaches the ``TaskPreflightDecision``
        that gated this call (reasoning on/off + effort, orchestrator
        on/off, tool groups, the short non-CoT ``reason`` string) plus the
        actual ``request_model_settings`` sent to the model and a
        ``tool_call_count`` — so a trace can answer "why did this turn route
        the way it did" without re-deriving it from the prompt text.
        """
        if self._sink is None or not ctx.trace_id:
            return
        self._sink.record_trace(
            ctx.trace_id,
            {
                "name": ctx.purpose.value if ctx.purpose else "agent",
                "tenant_id": ctx.tenant_id or None,
                "purpose": ctx.purpose.value if ctx.purpose else None,
            },
        )
        if self._output_safety is not None:
            self._sink.record_span(ctx.trace_id, "output_safety", {"flagged": ctx.output_flagged})
        model_slug = model_name_override or model_name_for(task) or "test-model"
        metadata: dict[str, object] = {
            "task": task.value,
            "model_slug": model_slug,
            "provider": provider_for(model_slug),
            "prompt_shape_family": capabilities_for(model_slug).prompt_shape_family.value,
            "capability_profile_version": CAPABILITY_PROFILE_VERSION,
            "rendered_system_prompt_hash": hashlib.sha256(
                system_prompt.encode("utf-8")
            ).hexdigest(),
        }
        if preflight is not None:
            metadata.update(
                {
                    "preflight_needs_reasoning": preflight.needs_reasoning,
                    "preflight_reasoning_effort": preflight.reasoning_effort,
                    "preflight_needs_orchestrator": preflight.needs_orchestrator,
                    "preflight_tool_groups": sorted(preflight.tool_groups),
                    "preflight_allowed_tool_names": sorted(preflight.allowed_tool_names),
                    "preflight_selected_model_slug": preflight.selected_model_slug,
                    "preflight_prompt_shape_family": preflight.prompt_shape_family.value,
                    "preflight_reason": preflight.reason,
                }
            )
        if request_model_settings is not None:
            metadata["request_model_settings"] = dict(request_model_settings)
        if tool_call_count is not None:
            metadata["tool_call_count"] = tool_call_count
        self._sink.record_generation(
            trace_id=ctx.trace_id,
            model=model_slug,
            prompt=f"[system]\n{system_prompt[:400]}\n[user]\n{user_payload[:400]}",
            completion=completion,
            privacy_flags=privacy_flags,
            usage={"input_tokens": usage.input_tokens, "output_tokens": usage.output_tokens},
            metadata=metadata,
        )

    def record_decision(self, decision: str, intent: str, confidence: float) -> None:
        """Re-label the request trace with the FINAL policy decision.

        The trace is opened during the draft step under the LLM *purpose*, which
        is always ``"draft"`` (the generation step drafts text regardless of what
        happens to it). Without this, an auto-sent reply is indistinguishable
        from an owner draft in Langfuse — every interaction shows as ``draft``.
        Langfuse upserts traces by id, so re-emitting the trace with the decision
        as its name (and in metadata) corrects the label and adds the outcome,
        intent and confidence as filterable fields. No-op when the sink or
        trace_id is absent so direct callers and tests are unaffected.
        """
        if self._sink is None:
            return
        ctx = current_context()
        if not ctx.trace_id:
            return
        self._sink.record_trace(
            ctx.trace_id,
            {
                "name": decision,
                "tenant_id": ctx.tenant_id or None,
                "purpose": ctx.purpose.value if ctx.purpose else None,
                "decision": decision,
                "intent": intent,
                "confidence": confidence,
            },
        )

    @staticmethod
    def _usage_of(result: object) -> Usage:
        """Normalize a pydantic-ai run usage into the contract ``Usage``."""
        usage = result.usage  # type: ignore[attr-defined]
        return Usage(input_tokens=usage.input_tokens or 0, output_tokens=usage.output_tokens or 0)

    @staticmethod
    def _warn_if_tool_output_unsupported(agent_name: str, model_slug: str) -> None:
        """Guardrail for the orchestrator/critic ToolOutput agents (Prompt Contract phase 1).

        Both stay on ``ToolOutput`` regardless of model family — unlike the
        draft agent, which already moved to ``PromptedOutput`` (see
        ``doubleu-thinking-models-promptedoutput``). A reasoning-family model
        assigned to either would 400 on the forced ``tool_choice`` ToolOutput
        sends for the ``final_result`` call. For the orchestrator this is no
        longer just logged — see ``_orchestrator_supports_tool_calling``,
        which gates two-stage routing on the same check (2.5/2.6). The critic
        has no equivalent fallback path (it always runs when risk/confidence
        call for it), so a misconfigured critic model still only logs here.
        """
        capabilities = capabilities_for(model_slug)
        if not capabilities.supports_tool_choice:
            logger.warning(
                "%s model %r resolves to family=%s (output_mode=%s); ToolOutput sends a forced "
                "tool_choice this family is known to reject — see doubleu-thinking-models-promptedoutput",
                agent_name, model_slug, capabilities.prompt_shape_family.value, capabilities.output_mode,
            )

    @staticmethod
    def _orchestrator_supports_tool_calling(model_slug: str) -> bool:
        """Whether the orchestrator's resolved model can safely run the tool-heavy path.

        Two independent capability gates feed ``two_stage`` routing (2.5/2.6
        provider compatibility guard): (1) ``supports_tool_choice`` — the same
        check ``_warn_if_tool_output_unsupported`` logs, here it actually
        disables the orchestrator instead of just warning. (2)
        ``json_tool_reliability != "blocked"`` — for providers a Langfuse
        audit caught misbehaving (400/malformed tool JSON) under a slug that
        family-pattern resolution alone doesn't flag as reasoning (see
        ``.claude/analyze.md``), set via the env-driven
        ``MODEL_CAPABILITY_JSON_TOOL_RELIABILITY_OVERRIDES`` — no model names
        hardcoded in either gate.
        """
        capabilities = capabilities_for(model_slug)
        return capabilities.supports_tool_choice and capabilities.json_tool_reliability != "blocked"

    # ------------------------------------------------------------------ draft

    def draft_outcome(
        self,
        request: AIProcessMessageRequest,
        risk_level: RiskLevel,
        risk_reasons: tuple[str, ...] = (),
    ) -> draft_pipeline.DraftOutcome:
        """Generate the structured draft (and critic review) for process-message.

        Retries the agent run up to ``_draft_max_attempts`` (env
        ``DRAFT_MAX_ATTEMPTS``, default 3): a draft failure is usually transient
        — a provider 5xx/rate-limit, a malformed structured output, or the
        tool-call cap — and a fresh attempt typically succeeds. Only after every
        attempt fails does it raise RuntimeError so the service's error-fallback
        branch fires (and Go delivers a user-facing notice). A run timeout is NOT
        retried: the caller (Go) has its own deadline, so re-running a slow agent
        only piles up wasted work. A safety block is not a failure: it returns a
        canned-reply outcome (model ``"blocked"``) on the first attempt.
        """
        preflight = draft_pipeline.preflight_outcome(request, risk_level, risk_reasons)
        if preflight is not None:
            return preflight
        ctx = current_context()
        last_exc: Exception | None = None
        for attempt in range(1, self._draft_max_attempts + 1):
            try:
                return self._run(self._draft_async(request, risk_level, ctx))
            except SafetyBlocked as exc:
                return draft_pipeline.DraftOutcome(
                    text=exc.canned_reply, model_used="blocked", usage=Usage()
                )
            except TimeoutError as exc:
                # The run exceeded its wall-clock budget; the HTTP caller has
                # almost certainly given up, so retrying wastes a slow LLM call.
                raise RuntimeError(f"agent draft timed out: {exc}") from exc
            except Exception as exc:  # noqa: BLE001 — single conversion point to the contract error
                last_exc = exc
                logger.warning(
                    "draft attempt %d/%d failed: %s", attempt, self._draft_max_attempts, exc
                )
        raise RuntimeError(
            f"agent draft failed after {self._draft_max_attempts} attempts: {last_exc}"
        )

    def _resolve_mcp_toolsets(
        self, ai_role: AIRoleData | None, privacy_flags: tuple[PrivacyFlag, ...]
    ) -> list:
        """Build the per-request MCP toolsets a role is allowed to use (#7).

        Degrade-safe: returns ``[]`` (unchanged draft behaviour) when the role
        declares no servers, the ``mcp`` package is absent, or the chat is
        privacy-restricted — an MCP tool could exfiltrate chat content, so
        ``no_memory``/``private`` chats never get external tools. Each server is
        wrapped in a FilteredToolset enforcing the role's tool allow-list
        (deny-by-default when ``allowed_tools`` is set).
        """
        if ai_role is None or not ai_role.mcp_servers:
            return []
        if PrivacyFlag.NO_MEMORY in privacy_flags or PrivacyFlag.PRIVATE in privacy_flags:
            return []
        try:
            from pydantic_ai.mcp import MCPServerSSE, MCPServerStreamableHTTP
        except ImportError:
            logger.warning("ai_role declares MCP servers but the `mcp` package is not installed")
            return []
        from pydantic_ai.toolsets import FilteredToolset

        toolsets: list = []
        for spec in ai_role.mcp_servers:
            if not spec.url:
                continue
            headers = {"Authorization": spec.auth_header} if spec.auth_header else None
            # MCPServerSSE/StreamableHTTP take url+headers directly with only the
            # `mcp` package. The non-deprecated MCPToolset needs an extra
            # `fastmcp` dependency, not worth pulling for this seam — migrate when
            # real servers onboard (backlog). Silence the v2 deprecation locally.
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", DeprecationWarning)
                if spec.transport == "sse":
                    server = MCPServerSSE(url=spec.url, headers=headers)
                else:
                    server = MCPServerStreamableHTTP(url=spec.url, headers=headers)
            if spec.allowed_tools:
                allowed = frozenset(spec.allowed_tools)
                server = FilteredToolset(
                    server, lambda ctx, td, allowed=allowed: td.name in allowed
                )
            toolsets.append(server)
        return toolsets

    async def _orchestrate_async(
        self,
        request: AIProcessMessageRequest,
        ctx: LLMCallContext,
        pool: asyncpg.Pool | None,
        decision: TaskPreflightDecision,
    ) -> tuple[OrchestratorResult, Usage, tuple[ScheduledJobData, ...], tuple[int, ...]]:
        """Run the tool-owning context stage without producing a contact reply.

        2.7: ``decision`` (already gated ``needs_orchestrator=True`` by the
        caller) narrows this stage's toolset to
        ``decision.allowed_tool_names`` and skips MCP entirely unless
        ``"mcp"`` is in ``decision.tool_groups`` — the orchestrator only ever
        sees the tools the preflight decided this turn actually needs, not
        every tool the role enables.
        """
        orchestrator_slug = model_name_for(AgentTask.ORCHESTRATE)
        current_time = datetime.now(timezone.utc)
        family = resolve_family(orchestrator_slug)
        rendered = render_prompt(
            prompt_library.orchestrator_spec(request, current_time=current_time), family
        )
        system_prompt, user_payload = self._prompt_builder.build(
            rendered, request.event.message_text
        )
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.DRAFT_REPLY, ctx
        )
        privacy_flags = request.chat_settings.privacy_flags
        deps = self._deps_for(
            request.event.tenant_id,
            request.event.chat_id,
            privacy_flags,
            source_message_id=request.event.source_message_id,
            system_instruction=system_prompt,
            pool=pool,
            current_time=current_time,
            existing_jobs=request.existing_scheduled_jobs,
        )
        toolsets = [
            resolve_builtin_toolset(
                request.ai_role, privacy_flags, allowed_names=decision.allowed_tool_names
            ),
            *(
                self._resolve_mcp_toolsets(request.ai_role, privacy_flags)
                if "mcp" in decision.tool_groups
                else []
            ),
        ]
        run_kwargs: dict = dict(
            deps=deps,
            usage_limits=UsageLimits(tool_calls_limit=_ORCHESTRATOR_TOOL_CALLS_LIMIT),
            toolsets=toolsets,
        )
        orchestrator_model_settings = model_settings_for(decision, capabilities_for(orchestrator_slug))
        if orchestrator_model_settings:
            run_kwargs["model_settings"] = orchestrator_model_settings
        result = await self._orchestrator_agent.run(user_payload, **run_kwargs)
        # A response form is authoritative only when a tool actually wrote it
        # into deps. Structured model output alone cannot fabricate buttons.
        orchestrated: OrchestratorResult = result.output.model_copy(
            update={"response_plan": deps.response_plan}
        )
        scheduled_jobs = _scheduled_job_data(deps.scheduled_jobs)
        cancelled_job_ids = tuple(deps.cancelled_job_ids)
        usage = self._usage_of(result)
        completion = "\n".join(
            [
                *orchestrated.gathered_context,
                f"suggested_intent: {orchestrated.suggested_intent}",
                f"notes: {orchestrated.notes}",
                f"response_plan: {orchestrated.response_plan}",
                f"scheduled_jobs: {len(scheduled_jobs)}",
                f"cancelled_jobs: {len(cancelled_job_ids)}",
            ]
        )
        self._record_run(
            ctx,
            AgentTask.ORCHESTRATE,
            system_prompt,
            user_payload,
            completion,
            usage,
            privacy_flags,
            preflight=decision,
            request_model_settings=orchestrator_model_settings,
            tool_call_count=len(deps.tool_call_cache),
        )
        return orchestrated, usage, scheduled_jobs, cancelled_job_ids

    @staticmethod
    def _orchestrator_bundle(result: OrchestratorResult) -> tuple[str, ...]:
        bundle = list(result.gathered_context)
        if result.suggested_intent:
            bundle.append("Предполагаемый интент: " + result.suggested_intent)
        if result.notes:
            bundle.append("Пометки писателю: " + result.notes)
        if result.response_plan is not None:
            bundle.append("Форма ответа уже задана interaction-инструментом.")
        return tuple(bundle)

    @staticmethod
    def _writer_model_slug(request: AIProcessMessageRequest) -> str:
        """Resolve the concrete model slug that will actually serve the writer call.

        Shared by ``_build_draft_prompt_contract`` (prompt family resolution)
        and ``_draft_async`` (2.7 ``TaskPreflightDecision``) so both derive
        the family/capability profile from the exact same role tier/
        model_name precedence — never two slightly different resolutions of
        "which model is this".
        """
        role = request.ai_role
        return resolve_model_slug(
            AgentTask.DRAFT,
            tier=role.model_tier if role is not None else "",
            model_name=role.model_name if role is not None else "",
        )

    def _build_draft_prompt_contract(
        self,
        request: AIProcessMessageRequest,
        *,
        orchestrator_bundle: tuple[str, ...] | None,
        current_time: datetime | None,
    ) -> tuple[str, str]:
        """Build the writer's (system_prompt, user_payload) pair via the Prompt Contract.

        Honours the same privacy gate and memory split ``_split_memory_context``
        always applied, rendered into the system prompt instead of one
        concatenated blob. The model family is resolved from the concrete slug
        that will actually serve the writer call (the per-role tier override
        when the role declares one, else the flat DRAFT model), mirroring the
        precedence already applied when building the model object further
        down in ``_draft_async``.
        """
        raw_memory = (
            ()
            if PrivacyFlag.NO_MEMORY in request.chat_settings.privacy_flags
            else request.memory_context
        )
        feedback_hint = draft_pipeline.build_feedback_hint(request.feedback_context)
        owner_instruction, current_draft, owner_style_facts, clean_memory = (
            draft_pipeline._split_memory_context(raw_memory)
        )
        writer_slug = self._writer_model_slug(request)
        family = resolve_family(writer_slug)
        spec = prompt_library.draft_spec(
            request,
            clean_memory=clean_memory,
            owner_style_facts=owner_style_facts,
            current_draft=current_draft,
            owner_instruction=owner_instruction,
            feedback_hint=feedback_hint,
            two_stage=orchestrator_bundle is not None,
            current_time=current_time,
        )
        rendered = render_prompt(spec, family)
        prompt_context = list(clean_memory)
        if orchestrator_bundle is not None:
            bundle = "\n".join(orchestrator_bundle) or "Инструменты не вернули дополнительного контекста."
            prompt_context.append("[СОБРАННЫЙ КОНТЕКСТ ОРКЕСТРАТОРА]\n" + bundle)
        return self._prompt_builder.build(rendered, request.event.message_text, tuple(prompt_context))

    async def _draft_async(
        self, request: AIProcessMessageRequest, risk_level: RiskLevel, ctx: LLMCallContext
    ) -> draft_pipeline.DraftOutcome:
        # 2.7: deterministic, no-LLM preflight — decides whether the
        # orchestrator pre-stage runs at all, which tool groups it may use,
        # and the reasoning effort/request params for both stages. Only ever
        # NARROWS the other two-stage gates below, never widens them.
        writer_slug = self._writer_model_slug(request)
        orchestrator_slug = model_name_for(AgentTask.ORCHESTRATE)
        decision = decide_preflight(
            request, orchestrator_slug=orchestrator_slug, writer_slug=writer_slug
        )
        two_stage = (
            self._two_stage_draft
            and self._orchestrator_tool_calling_supported
            and role_has_enabled_tools(request.ai_role, request.chat_settings.privacy_flags)
            and decision.needs_orchestrator
        )
        pool = await self._get_pool()
        orchestrator_usage = Usage()
        orchestrator_bundle: tuple[str, ...] | None = None
        orchestrated: OrchestratorResult | None = None
        orchestrator_scheduled_jobs: tuple[ScheduledJobData, ...] = ()
        orchestrator_cancelled_jobs: tuple[int, ...] = ()
        if two_stage:
            (
                orchestrated,
                orchestrator_usage,
                orchestrator_scheduled_jobs,
                orchestrator_cancelled_jobs,
            ) = await self._orchestrate_async(
                request,
                ctx,
                pool,
                decision,
            )
            orchestrator_bundle = self._orchestrator_bundle(orchestrated)
        current_time = datetime.now(timezone.utc)
        system_prompt, user_payload = self._build_draft_prompt_contract(
            request, orchestrator_bundle=orchestrator_bundle, current_time=current_time
        )
        # Phase 1: may rewrite (anonymize) or raise SafetyBlocked → canned reply.
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.DRAFT_REPLY, ctx
        )
        privacy_flags = request.chat_settings.privacy_flags
        deps = self._deps_for(
            request.event.tenant_id,
            request.event.chat_id,
            privacy_flags,
            source_message_id=request.event.source_message_id,
            system_instruction=system_prompt,
            pool=pool,
            current_time=current_time,
            existing_jobs=request.existing_scheduled_jobs,
        )
        # Strict split (default, see _WRITER_FALLBACK_TOOLS_ENABLED above): in
        # two-stage mode the orchestrator is the ONLY memory/schedule/interaction
        # tool-caller, so the writer gets just its always-on delivery capability
        # (lets it split a turn into several ordered messages — not in
        # TOOL_CATALOG, never shown in the role UI). Single-stage drafting has no
        # orchestrator run, so the writer must keep the full toolset regardless.
        if two_stage and not _WRITER_FALLBACK_TOOLS_ENABLED:
            writer_toolsets: list = [delivery_toolset]
        else:
            writer_toolsets = [
                resolve_builtin_toolset(request.ai_role, privacy_flags),
                *self._resolve_mcp_toolsets(request.ai_role, privacy_flags),
                delivery_toolset,
            ]
        # Per-role tier override: build a tier-specific model when the role
        # declares one and the env list is configured; otherwise fall back to
        # the DRAFT task model.  The agent is not recreated — only the
        # per-request model is passed in.
        role_model_name = (
            request.ai_role.model_name.strip() if request.ai_role is not None else ""
        )
        tier_model = None
        tier_model_label = ""
        if request.ai_role is not None:
            if role_model_name:
                tier_model = build_named_model(
                    request.ai_role.model_tier,
                    role_model_name,
                )
            else:
                tier_model = build_tier_model(request.ai_role.model_tier)
            if tier_model is not None:
                # model_name_for_tier already resolves the concrete slug for
                # BOTH cases (an explicit role_model_name, or — when empty —
                # the tier's first configured slug), so this must run
                # unconditionally. It used to live only inside the
                # `if role_model_name:` branch above, so a role declaring
                # just model_tier (no model_name) left tier_model_label empty
                # and model_used fell back to the bare tier string ("smart")
                # instead of the concrete slug actually serving the call.
                tier_model_label = model_name_for_tier(
                    request.ai_role.model_tier,
                    role_model_name,
                )
        run_kwargs: dict = dict(
            deps=deps,
            usage_limits=UsageLimits(tool_calls_limit=_DRAFT_TOOL_CALLS_LIMIT),
            toolsets=writer_toolsets,
        )
        if tier_model is not None:
            run_kwargs["model"] = tier_model
        writer_model_settings = model_settings_for(decision, capabilities_for(decision.selected_model_slug))
        if writer_model_settings:
            run_kwargs["model_settings"] = writer_model_settings
        result = await self._draft_agent.run(user_payload, **run_kwargs)
        drafted: DraftResult = result.output
        # Hybrid: both stages may schedule jobs; dedupe by the content-based
        # idempotency_key so a reminder produced in both stages is sent to Go once.
        _by_key: dict[str, ScheduledJobData] = {}
        for job in (*orchestrator_scheduled_jobs, *_scheduled_job_data(deps.scheduled_jobs)):
            _by_key.setdefault(job.idempotency_key, job)
        scheduled_jobs = tuple(_by_key.values())
        # Cancellations may be recorded in either stage's deps; dedupe while
        # preserving order so Go applies each CancelJob at most once.
        cancelled_job_ids = tuple(
            dict.fromkeys((*orchestrator_cancelled_jobs, *deps.cancelled_job_ids))
        )
        # Hybrid: the orchestrator's plan wins, but if it set none the writer may
        # have produced one via its fallback ask_questions call — keep that.
        response_plan = (
            orchestrated.response_plan
            if (orchestrated is not None and orchestrated.response_plan is not None)
            else deps.response_plan
        )
        if response_plan is not None:
            # The questionnaire is collected by the Go backend one question at a
            # time; the per-question options live in response_plan, not the draft.
            # Keep the writer's reply as a short intro; mark the intent so the
            # decision engine treats this as an interactive turn.
            drafted = drafted.model_copy(update={"intent": "choice_question", "options": ()})
        draft_usage = self._usage_of(result)
        if two_stage and request.ai_role is not None:
            router_name = model_name_for(AgentTask.ORCHESTRATE) or "test-model"
            writer_name = (
                tier_model_label
                or request.ai_role.model_tier
                or model_name_for(AgentTask.DRAFT)
                or "test-model"
            )
            model_used = f"orchestrate({router_name})+draft({writer_name})"
        else:
            model_used = (
                tier_model_label
                or request.ai_role.model_tier
                if (tier_model is not None and request.ai_role is not None)
                else (model_name_for(AgentTask.DRAFT) or "test-model")
            )

        text = drafted.reply
        confidence = drafted.confidence
        if risk_level in _CRITIC_RISKS or confidence < _CRITIC_CONFIDENCE_BAR:
            speak_as_owner = request.ai_role is None or request.ai_role.speak_as_owner
            feedback_hint = (
                draft_pipeline.build_feedback_hint(request.feedback_context)
                if speak_as_owner else ""
            )
            text, confidence, critic_usage = await self._review_draft(
                deps, request.event.message_text, drafted, ctx, privacy_flags,
                feedback_hint, speak_as_owner=speak_as_owner,
            )
            draft_usage = Usage(
                input_tokens=draft_usage.input_tokens + critic_usage.input_tokens,
                output_tokens=draft_usage.output_tokens + critic_usage.output_tokens,
            )
            model_used += "+critic"

        # Phase 3: deanonymize + non-blocking flags on the final text.
        text, _ = self._scan_output(text, ModelTask.DRAFT_REPLY, ctx)
        self._record_run(
            ctx,
            AgentTask.DRAFT,
            system_prompt,
            user_payload,
            drafted.reply,
            draft_usage,
            privacy_flags,
            model_name_override=(
                (tier_model_label or request.ai_role.model_tier)
                if two_stage and request.ai_role
                else ""
            ),
            preflight=decision,
            request_model_settings=writer_model_settings,
            tool_call_count=len(deps.tool_call_cache),
        )
        total = Usage(
            input_tokens=orchestrator_usage.input_tokens + draft_usage.input_tokens,
            output_tokens=orchestrator_usage.output_tokens + draft_usage.output_tokens,
        )
        text_format = draft_pipeline.decide_text_format(request.ai_role, drafted.intent)
        if text_format == "rich":
            # Normalize only the final safe text — never raw LLM chunks.
            text = draft_pipeline.normalize_rich_markdown(text)
        # Prose messages the writer queued via send_message, delivered before the
        # final reply/questionnaire. They are contact-facing prose, so they take
        # the role's general formatting (rich when a role is assigned) and the
        # same rich-normalisation as the final text.
        msg_format = draft_pipeline.decide_text_format(request.ai_role, "general_message")
        outbound_messages = tuple(
            OutboundMessageData(
                text=(
                    draft_pipeline.normalize_rich_markdown(m.text)
                    if msg_format == "rich" else m.text
                ),
                text_format=msg_format,
            )
            for m in deps.outbound_messages
        )
        return draft_pipeline.DraftOutcome(
            text=text,
            model_used=model_used,
            usage=total,
            intent=drafted.intent,
            text_format=text_format,
            messages=outbound_messages,
            options=drafted.options if drafted.intent == "choice_question" else (),
            response_plan=(
                ResponsePlanData(
                    shape=response_plan.shape,
                    questions=tuple(
                        QuestionData(prompt=q.prompt, options=q.options)
                        for q in response_plan.questions
                    ),
                )
                if response_plan is not None
                else None
            ),
            scheduled_jobs=scheduled_jobs,
            cancelled_job_ids=cancelled_job_ids,
            confidence=confidence,
            topic=drafted.topic,
        )

    async def _review_draft(
        self,
        deps: AgentDeps,
        message_text: str,
        drafted: DraftResult,
        ctx: LLMCallContext,
        privacy_flags: tuple[PrivacyFlag, ...],
        feedback_hint: str = "",
        *,
        speak_as_owner: bool = True,
    ) -> tuple[str, float, Usage]:
        """Run the critic; returns (final_text, final_confidence, usage).

        A revision halves the surviving confidence: a draft the critic had to
        fix must not auto-send on the strength of its original self-score.
        Critic failures keep the original draft — review is best-effort.

        ``feedback_hint`` carries the same owner-feedback anti-examples the draft
        agent saw, so the critic can flag a reply written in a style the owner
        has already rejected (self-correction grounded in past outcomes); it is
        sanitized owner-curated style guidance (thumbs up/down on past AI text),
        not raw contact text, so it travels in ``critic_spec.examples`` —
        trusted, like the draft agent's own feedback hint. ``message_text`` (the
        contact's message) and ``drafted.reply`` (model-generated) are the only
        genuinely untrusted pieces here and go through ``PromptBuilder.build``
        below, never into ``PromptSpec.context``.
        """
        family = resolve_family(model_name_for(AgentTask.CRITIC))
        rendered = render_prompt(
            prompt_library.critic_spec(speak_as_owner=speak_as_owner, feedback_hint=feedback_hint),
            family,
        )
        review_user_content = (
            "Сообщение собеседника:\n"
            f"{message_text}\n\n"
            "Черновик ответа:\n"
            f"{drafted.reply}"
        )
        critic_instructions, review_payload = self._prompt_builder.build(rendered, review_user_content)
        # The critic reuses the draft's deps object (see _draft_async); its
        # system_instruction currently holds the WRITER's rendered prompt.
        # Overwriting it here is safe — nothing reads the old value after this
        # point in _draft_async — and lets the critic agent's dynamic
        # instructions hook pick up the critic's own rendered text.
        deps.system_instruction = critic_instructions
        try:
            review = await self._critic_agent.run(review_payload, deps=deps)
        except Exception as exc:  # noqa: BLE001 — critic is advisory, never fatal
            logger.warning("critic agent failed; keeping original draft", extra={"error": str(exc)})
            return drafted.reply, drafted.confidence, Usage()
        verdict: CriticVerdict = review.output
        critic_usage = self._usage_of(review)
        self._record_run(
            ctx, AgentTask.CRITIC, critic_instructions, review_payload,
            verdict.revised_reply or verdict.verdict, critic_usage, privacy_flags,
        )
        if verdict.verdict == "revise" and verdict.revised_reply.strip():
            logger.info(
                "critic revised draft",
                extra={"tenant_id": deps.tenant_id, "chat_id": deps.chat_id, "issues": list(verdict.issues)},
            )
            return verdict.revised_reply.strip(), min(drafted.confidence, 0.5), critic_usage
        return drafted.reply, drafted.confidence, critic_usage

    # ------------------------------------------------------------- role draft

    def draft_role(self, request: DraftRoleRequest) -> DraftRoleResponse:
        """Generate a structured, owner-editable role from free text."""
        ctx = current_context()
        try:
            return self._run(self._draft_role_async(request, ctx))
        except Exception as exc:  # noqa: BLE001 - convert agent failures at the contract boundary
            raise RuntimeError(f"role draft agent failed: {exc}") from exc

    async def _draft_role_async(
        self, request: DraftRoleRequest, ctx: LLMCallContext
    ) -> DraftRoleResponse:
        family = resolve_family(model_name_for(AgentTask.ROLE_DRAFT))
        rendered = render_prompt(prompt_library.role_draft_spec(), family)
        system_prompt, user_payload = self._prompt_builder.build(rendered, request.text)
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.DRAFT_REPLY, ctx
        )
        deps = self._deps_for(
            ctx.tenant_id,
            ctx.chat_id,
            (),
            system_instruction=system_prompt,
            pool=None,
        )
        result = await self._role_draft_agent.run(user_payload, deps=deps)
        drafted: RoleDraft = result.output
        usage = self._usage_of(result)
        self._record_run(
            ctx,
            AgentTask.ROLE_DRAFT,
            system_prompt,
            user_payload,
            drafted.model_dump_json(),
            usage,
            (),
        )
        return DraftRoleResponse(
            name=drafted.name.strip(),
            description=drafted.description.strip(),
            speaking_style=drafted.speaking_style.strip(),
            goals=drafted.goals.strip(),
            constraints=drafted.constraints.strip(),
            custom_fields=tuple(
                AIRoleCustomFieldData(
                    title=item.title.strip(),
                    description=item.description.strip(),
                    context=item.context.strip(),
                )
                for item in drafted.custom_fields
            ),
        )

    # ------------------------------------------------------------- extraction

    def extract_facts_typed(
        self, typed_extractor: TypedMemoryExtractor, request: TypedExtractionRequest
    ) -> TypedExtractionResponse:
        """Typed fact extraction through the extraction agent.

        Keeps the legacy merge contract: deterministic regex facts fill the
        gaps the LLM missed, LLM facts win collisions.  When the runtime owns
        memory (DATABASE_URL configured), merged facts are persisted directly
        through ``upsert_semantic_fact`` and suggestions are staged — the Go
        backend then skips its own write path (AI_MEMORY_SOURCE=python).
        """
        if PrivacyFlag.NO_MEMORY in request.privacy_flags:
            return TypedExtractionResponse(facts=(), blocked_by_privacy=True)
        ctx = current_context()
        deterministic = typed_extractor.extract(request.message_text, request.privacy_flags)
        try:
            extraction, raw_output = self._run(self._extract_async(request, ctx))
        except Exception as exc:  # noqa: BLE001 — degrade to deterministic-only facts
            logger.warning("extraction agent failed", extra={"error": str(exc)})
            extraction, raw_output = ExtractionResult(), f"ERROR: {exc}"

        llm_facts = tuple(
            TypedFact(
                fact_text=f.fact_text,
                fact_type=f.fact_type,
                subject=f.subject,
                scope_kind="chat",
                confidence=f.confidence,
            )
            for f in extraction.facts
        )
        merged = list(llm_facts)
        llm_texts = {f.fact_text.lower().strip() for f in llm_facts}
        for f in deterministic.facts:
            if f.fact_text.lower().strip() not in llm_texts:
                merged.append(f)
        suggestions = tuple(
            ProfileSuggestion(
                field_name=s.field_name,
                suggested_value=s.suggested_value,
                rationale=s.rationale,
            )
            for s in extraction.profile_suggestions
        )
        if self._database_url:
            try:
                self._run(self._persist_extraction(request, tuple(merged), suggestions))
            except Exception as exc:  # noqa: BLE001 — extraction result still returned to Go
                logger.warning("agent memory write failed", extra={"error": str(exc)})
        return TypedExtractionResponse(
            facts=tuple(merged),
            blocked_by_privacy=False,
            model_used=model_name_for(AgentTask.EXTRACT) or "test-model",
            raw_output=raw_output[:2000],
            llm_facts_count=len(llm_facts),
            deterministic_facts_count=len(deterministic.facts),
            profile_suggestions=suggestions,
        )

    async def _extract_async(
        self, request: TypedExtractionRequest, ctx: LLMCallContext
    ) -> tuple[ExtractionResult, str]:
        if request.recent_messages:
            history_block = "\n".join(f"- {m}" for m in request.recent_messages if m.strip())
            user_content = (
                "[CONVERSATION HISTORY — for context only, do not re-extract]\n"
                f"{history_block}\n\n"
                "[CURRENT MESSAGE — extract facts from this]\n"
                f"{request.message_text}"
            )
        else:
            user_content = request.message_text
        # The extraction input is untrusted contact text: wrap it the same way
        # the draft path does so instruction injection stays inert.
        family = resolve_family(model_name_for(AgentTask.EXTRACT))
        rendered = render_prompt(prompt_library.extract_spec(), family)
        system_prompt, user_payload = self._prompt_builder.build(rendered, user_content, ())
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.EXTRACT_FACTS, ctx
        )
        deps = self._deps_for(
            request.tenant_id, request.chat_id, request.privacy_flags,
            system_instruction=system_prompt, pool=None,
        )
        result = await self._extraction_agent.run(user_payload, deps=deps)
        self._record_run(
            ctx, AgentTask.EXTRACT, system_prompt, user_payload,
            result.output.model_dump_json(), self._usage_of(result), request.privacy_flags,
        )
        return result.output, result.output.model_dump_json()

    async def _persist_extraction(
        self,
        request: TypedExtractionRequest,
        facts: tuple[TypedFact, ...],
        suggestions: tuple[ProfileSuggestion, ...],
    ) -> None:
        """Write merged facts and suggestions through the canonical DB paths."""
        pool = await self._get_pool()
        if pool is None:
            return
        for fact in facts:
            await pool.fetchrow(
                "SELECT * FROM upsert_semantic_fact($1, 'chat', $2, $3, $4, $5, $6, 'llm_extracted', $7)",
                request.tenant_id,
                request.chat_id,
                fact.fact_text,
                fact.fact_type,
                fact.subject,
                fact.confidence,
                "",
            )
        for s in suggestions:
            await pool.execute(
                """INSERT INTO contact_profile_suggestions
                       (tenant_id, chat_id, field_name, suggested_value, rationale, status)
                   VALUES ($1, $2, $3, to_jsonb($4::text), $5, 'pending')
                   ON CONFLICT DO NOTHING""",
                request.tenant_id,
                request.chat_id,
                s.field_name,
                s.suggested_value,
                s.rationale,
            )

    # ------------------------------------------------------------- summarize

    def summarize_chat(self, request: ChatSummaryRequest) -> ChatSummaryResponse:
        """Summarize chat messages through the summary agent (no_summary-gated)."""
        if PrivacyFlag.NO_SUMMARY in request.privacy_flags:
            return ChatSummaryResponse(
                summary="", action_items=(), model_used="none", usage=Usage(), blocked_by_privacy=True
            )
        if not request.messages:
            return ChatSummaryResponse(
                summary="", action_items=(), model_used="none", usage=Usage()
            )
        ctx = current_context()
        try:
            summary, usage = self._run(self._summarize_async(request, ctx))
        except Exception as exc:  # noqa: BLE001 — summarization is non-critical
            logger.warning("summary agent failed", extra={"error": str(exc)})
            return ChatSummaryResponse(
                summary="", action_items=(), model_used="error", usage=Usage()
            )
        return ChatSummaryResponse(
            summary=summary.summary,
            action_items=summary.action_items,
            model_used=model_name_for(AgentTask.SUMMARIZE) or "test-model",
            usage=usage,
            recommendations=summary.recommendations,
        )

    async def _summarize_async(
        self, request: ChatSummaryRequest, ctx: LLMCallContext
    ) -> tuple[SummaryResult, Usage]:
        family = resolve_family(model_name_for(AgentTask.SUMMARIZE))
        rendered = render_prompt(prompt_library.summary_spec(), family)
        system_prompt, user_payload = self._prompt_builder.build(
            rendered, "\n".join(request.messages), ()
        )
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.SUMMARIZE_CHAT, ctx
        )
        deps = AgentDeps(tenant_id="", chat_id="", system_instruction=system_prompt)
        result = await self._summary_agent.run(user_payload, deps=deps)
        text, _ = self._scan_output(result.output.summary, ModelTask.SUMMARIZE_CHAT, ctx)
        usage = self._usage_of(result)
        self._record_run(
            ctx, AgentTask.SUMMARIZE, system_prompt, user_payload, text, usage, request.privacy_flags
        )
        return result.output, usage

    # ---------------------------------------------------------------- topic

    def tag_topic(self, request: TopicTaggingRequest) -> TopicTaggingResponse:
        """Return a 3-7 word topic label via the topic agent.

        Returns an empty label on any failure so the caller can skip the
        episode update without noise — topic tagging is best-effort.
        """
        if not request.message_text.strip():
            return TopicTaggingResponse(topic="")
        ctx = current_context()
        try:
            topic = self._run(self._topic_async(request, ctx))
        except Exception as exc:  # noqa: BLE001 — topic tagging is non-critical
            logger.warning("topic tagging failed", extra={"error": str(exc)})
            return TopicTaggingResponse(topic="", model_used="")
        return TopicTaggingResponse(
            topic=topic, model_used=model_name_for(AgentTask.TOPIC) or "test-model"
        )

    async def _topic_async(self, request: TopicTaggingRequest, ctx: LLMCallContext) -> str:
        history = [m for m in request.recent_messages[-_TOPIC_MAX_HISTORY:] if m.strip()]
        if history:
            context_block = "\n".join(f"- {m}" for m in history)
            content = f"[RECENT MESSAGES]\n{context_block}\n\n[LATEST MESSAGE]\n{request.message_text}"
        else:
            content = request.message_text
        family = resolve_family(model_name_for(AgentTask.TOPIC))
        rendered = render_prompt(prompt_library.topic_spec(), family)
        system_prompt, user_payload = self._prompt_builder.build(rendered, content, ())
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.EXTRACT_FACTS, ctx
        )
        deps = AgentDeps(
            tenant_id=request.tenant_id, chat_id=request.chat_id, system_instruction=system_prompt
        )
        result = await self._topic_agent.run(user_payload, deps=deps)
        topic = result.output.topic.strip()
        self._record_run(
            ctx, AgentTask.TOPIC, system_prompt, user_payload, topic, self._usage_of(result), ()
        )
        return topic

    # ---------------------------------------------------------------- rerank

    def rerank_facts(self, request: RerankFactsRequest) -> RerankFactsResponse:
        """Rerank candidate facts by relevance to ``request.query``.

        On any failure (agent error, empty result) returns the input IDs in
        their original order so the retrieval path always has something to work
        with — identical fallback to the legacy listwise reranker.
        """
        if not request.facts or not request.query.strip():
            return RerankFactsResponse(ranked_ids=())
        ctx = current_context()
        model_used = model_name_for(AgentTask.RERANK) or "test-model"
        try:
            ranked = self._run(self._rerank_async(request, ctx))
        except Exception as exc:  # noqa: BLE001 — fall back to original order
            logger.warning("reranking failed, using original order", extra={"error": str(exc)})
            return self._rerank_fallback(request, model_used)
        if not ranked:
            return self._rerank_fallback(request, model_used)
        return RerankFactsResponse(ranked_ids=ranked[: request.top_k], model_used=model_used)

    async def _rerank_async(
        self, request: RerankFactsRequest, ctx: LLMCallContext
    ) -> tuple[int, ...]:
        fact_lines = "\n".join(
            f"{i + 1}. [ID={f['id']}] {f['text']}" for i, f in enumerate(request.facts)
        )
        content = f"QUERY: {request.query}\n\nFACTS:\n{fact_lines}"
        family = resolve_family(model_name_for(AgentTask.RERANK))
        rendered = render_prompt(prompt_library.rerank_spec(), family)
        system_prompt, user_payload = self._prompt_builder.build(rendered, content, ())
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.EXTRACT_FACTS, ctx
        )
        deps = AgentDeps(tenant_id="", chat_id="", system_instruction=system_prompt)
        result = await self._rerank_agent.run(user_payload, deps=deps)
        self._record_run(
            ctx, AgentTask.RERANK, system_prompt, user_payload,
            str(result.output.ranked_ids), self._usage_of(result), (),
        )
        valid_ids = {int(f["id"]) for f in request.facts}
        return tuple(i for i in result.output.ranked_ids if i in valid_ids)

    @staticmethod
    def _rerank_fallback(request: RerankFactsRequest, model_used: str) -> RerankFactsResponse:
        """Return candidate facts in their original order, truncated to top_k."""
        ids = tuple(int(f["id"]) for f in request.facts)[: request.top_k]
        return RerankFactsResponse(ranked_ids=ids, model_used=model_used)

    # --------------------------------------------------------------- reflect

    def synthesize_pattern(self, request: SynthesizePatternRequest) -> SynthesizePatternResponse:
        """Synthesise one durable behaviour pattern from a fact cluster (#5).

        Stateless by design: the backend owns clustering and persistence; this
        only turns a cluster of fact texts into a single pattern phrase. The
        ``no_memory`` flag blocks it, and an empty ``pattern`` means the cluster
        carried no durable signal, so the backend writes nothing.
        """
        if PrivacyFlag.NO_MEMORY in request.privacy_flags:
            return SynthesizePatternResponse(
                pattern="", subject="contact", confidence=0.0,
                model_used="none", usage=Usage(), blocked_by_privacy=True,
            )
        if not request.cluster_facts:
            return SynthesizePatternResponse(
                pattern="", subject="contact", confidence=0.0, model_used="none", usage=Usage()
            )
        ctx = current_context()
        try:
            result, usage = self._run(self._synthesize_pattern_async(request, ctx))
        except Exception as exc:  # noqa: BLE001 — reflection is best-effort, never fatal
            logger.warning("pattern synthesis failed", extra={"error": str(exc)})
            return SynthesizePatternResponse(
                pattern="", subject="contact", confidence=0.0, model_used="error", usage=Usage()
            )
        return SynthesizePatternResponse(
            pattern=result.pattern.strip(),
            subject=result.subject,
            confidence=result.confidence,
            model_used=model_name_for(AgentTask.REFLECT) or "test-model",
            usage=usage,
        )

    async def _synthesize_pattern_async(
        self, request: SynthesizePatternRequest, ctx: LLMCallContext
    ) -> tuple[PatternResult, Usage]:
        facts_block = "\n".join(f"- {f}" for f in request.cluster_facts if f.strip())
        content = f"[КЛАСТЕР ФАКТОВ О СОБЕСЕДНИКЕ]\n{facts_block}"
        # Fact texts derive from untrusted contact messages: wrap them like the
        # extraction path so any embedded instruction stays inert.
        family = resolve_family(model_name_for(AgentTask.REFLECT))
        rendered = render_prompt(prompt_library.pattern_spec(), family)
        system_prompt, user_payload = self._prompt_builder.build(rendered, content, ())
        system_prompt, user_payload = self._scan_input(
            system_prompt, user_payload, ModelTask.EXTRACT_FACTS, ctx
        )
        deps = AgentDeps(
            tenant_id=request.tenant_id, chat_id=request.chat_id, system_instruction=system_prompt
        )
        result = await self._pattern_agent.run(user_payload, deps=deps)
        usage = self._usage_of(result)
        self._record_run(
            ctx, AgentTask.REFLECT, system_prompt, user_payload,
            result.output.pattern, usage, request.privacy_flags,
        )
        return result.output, usage


def build_runtime(
    *,
    input_safety: InputSafetyScanner | None = None,
    output_safety: OutputSafetyScanner | None = None,
    sink: LangfuseSink | None = None,
    two_stage_draft: bool = False,
) -> AgentRuntime:
    """Build the agent runtime — the single LLM execution path for the service.

    DATABASE_URL is honoured only when ``AI_MEMORY_SOURCE=python`` — the
    stateless contract (Go owns all memory IO) stays the default.  When
    ``OBSERVABILITY_SINK=logfire`` and the optional logfire package is
    installed, every agent run is also instrumented via logfire; the primary
    sink remains the injected :class:`LangfuseSink`.
    """
    if os.getenv("OBSERVABILITY_SINK", "").strip().lower() == "logfire":
        try:
            import logfire

            logfire.configure()
            logfire.instrument_pydantic_ai()
        except ImportError:
            logger.warning("OBSERVABILITY_SINK=logfire set but logfire is not installed")
    database_url = ""
    if os.getenv("AI_MEMORY_SOURCE", "go").strip().lower() == "python":
        database_url = os.getenv("DATABASE_URL", "").strip()
    # Wire the semantic query-embedder unless running in deterministic mock mode
    # (no embedding backend there). Memory tools then run real vector search and
    # only fall back to lexical ILIKE when an embed call yields nothing.
    mock_llm = os.getenv("MOCK_LLM", "true").strip().lower() in ("1", "true", "yes", "on")
    embedder_factory = None if mock_llm else _build_query_embedder
    # Two-stage drafting (orchestrator + writer) over a long answer can run for
    # over a minute; the default 90s silently fell to error_fallback. Env-tunable.
    run_timeout = float(os.getenv("AGENT_RUN_TIMEOUT_SECONDS", "150").strip() or "150")
    return AgentRuntime(
        database_url=database_url,
        input_safety=input_safety,
        output_safety=output_safety,
        sink=sink,
        two_stage_draft=two_stage_draft,
        query_embedder_factory=embedder_factory,
        run_timeout_seconds=run_timeout,
        draft_max_attempts=_DRAFT_MAX_ATTEMPTS,
    )
