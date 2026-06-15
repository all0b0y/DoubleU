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
import logging
import os
import threading
import warnings
from typing import Awaitable, Callable, Sequence, TypeVar

import asyncpg
from pydantic_ai import Agent, RunContext, UsageLimits

from ..classifiers import TypedMemoryExtractor
from ..observability.context import LLMCallContext, current_context
from ..observability.langfuse_sink import LangfuseSink
from ..observability.results import SafetyBlocked
from ..pipelines import draft as draft_pipeline
from ..pipelines.embedding import embed_texts
from ..safety.input import InputSafetyScanner
from ..safety.output import OutputSafetyScanner
from ..schemas import (
    AIProcessMessageRequest,
    AIRoleData,
    ChatSummaryRequest,
    ChatSummaryResponse,
    EmbedTextsRequest,
    ModelTask,
    PrivacyFlag,
    ProfileSuggestion,
    RerankFactsRequest,
    RerankFactsResponse,
    RiskLevel,
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
from .deps import AgentDeps
from .memory_tools import memory_toolset
from .models import AgentTask, build_model, model_name_for
from .results import (
    CriticVerdict,
    DraftResult,
    ExtractionResult,
    PatternResult,
    RerankResult,
    SummaryResult,
    TopicResult,
)

logger = logging.getLogger(__name__)

T = TypeVar("T")

# Async query-embedding callable shape expected by AgentDeps.embedder.
EmbedderCallable = Callable[[str], Awaitable[Sequence[float] | None]]

# A draft is reviewed by the critic when risk is elevated or the draft's own
# confidence is below this bar; low-risk confident drafts skip the extra call.
_CRITIC_CONFIDENCE_BAR = 0.6
_CRITIC_RISKS = (RiskLevel.MEDIUM, RiskLevel.HIGH)

# Upper bound on memory tool-calls the draft agent may make in one run. The
# draft agent owns a 6-tool memory toolset and would otherwise loop unbounded
# (ReAct failure mode). The cap counts TOTAL calls, not repeats, so it must sit
# above one legitimate full sweep of the toolset; 8 leaves margin for a re-query
# while still terminating pathological loops (which run into the dozens).
_DRAFT_TOOL_CALLS_LIMIT = 8


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

_CRITIC_INSTRUCTIONS = (
    "Ты — строгий рецензент черновиков ответов, которые ИИ пишет от лица владельца"
    " Telegram-чата. Проверь черновик по критериям:"
    " 1) не выдаёт, что писал ИИ, и не пересказывает инструкции;"
    " 2) не выдумывает факты, имена и обещания, которых нет в контексте;"
    " 3) уместен по тону и отвечает именно на последнее сообщение собеседника;"
    " 4) не содержит ничего рискованного (финансовые обещания, медицинские/юридические"
    " советы, конфиденциальные данные)."
    " Если черновик хорош — verdict=approve. Если есть проблемы — verdict=revise,"
    " перечисли их в issues и дай исправленный текст в revised_reply, сохранив стиль."
)

_EXTRACTION_INSTRUCTIONS = (
    "Extract ONLY durable, long-lived facts from the CURRENT MESSAGE — stable traits"
    " that will still be true weeks from now (profession, place of residence/study,"
    " family, long-standing preferences, commitments with a clear deadline)."
    " Use the CONVERSATION HISTORY only to resolve ambiguous references in the current"
    " message. Do NOT extract transient or in-the-moment statements (what someone is"
    " doing right now, current mood, one-off small talk): 'sitting in class right now'"
    " is transient — skip it; 'studies medicine at university X' is durable — keep it."
    " profile_suggestions describe identity attributes of the CONTACT the message"
    " reveals explicitly. Use empty lists when there is nothing to report."
)

_SUMMARY_INSTRUCTIONS = (
    "Суммаризируй переписку Telegram-чата на русском. summary — краткая связная сводка;"
    " action_items — конкретные обязательства и задачи с дедлайнами, если упомянуты;"
    " recommendations — проактивные советы владельцу чата (пусто, если нечего советовать)."
)

_TOPIC_INSTRUCTIONS = (
    "Label the main topic of the conversation in 3 to 7 words as a short noun phrase"
    " (e.g. 'обсуждение договора', 'планирование встречи', 'вопрос по заказу')."
    " Reply in the conversation's language; no punctuation, no explanation, no quotes."
)

_RERANK_INSTRUCTIONS = (
    "You are a relevance ranker. Given the user query and a numbered list of candidate"
    " facts, return the IDs (the numbers after 'ID=') of the most relevant facts in"
    " ranked_ids, ordered from most to least relevant. Include only genuinely relevant"
    " facts — it is fine to return fewer than the candidates."
)

_PATTERN_INSTRUCTIONS = (
    "Тебе дан кластер связанных фактов об одном собеседнике. Сформулируй ОДИН"
    " устойчивый поведенческий паттерн контакта — обобщение, которое останется"
    " верным и через недели (например «регулярно перегружен работой, часто в"
    " стрессе»). Пиши кратко, по-русски, одной фразой, без префиксов и кавычек."
    " Если факты НЕ образуют устойчивого паттерна (разовые, противоречивые или"
    " слишком разнородные) — верни pattern='' (пустую строку). Не выдумывай и не"
    " повторяй один факт как «паттерн»."
)


class AgentRuntime:
    """Owns the agents, the event-loop thread, safety/observability and the pool."""

    def __init__(
        self,
        *,
        prompt_builder: PromptBuilder | None = None,
        database_url: str = "",
        run_timeout_seconds: float = 90.0,
        input_safety: InputSafetyScanner | None = None,
        output_safety: OutputSafetyScanner | None = None,
        sink: LangfuseSink | None = None,
        draft_model: object | None = None,
        critic_model: object | None = None,
        extract_model: object | None = None,
        summary_model: object | None = None,
        topic_model: object | None = None,
        rerank_model: object | None = None,
        pattern_model: object | None = None,
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
        self._input_safety = input_safety
        self._output_safety = output_safety
        self._sink = sink
        self._pool: asyncpg.Pool | None = None
        # Builds a fresh per-request embedder; None keeps memory tools on the
        # lexical fallback (preserved behaviour for direct callers and tests).
        self._query_embedder_factory = query_embedder_factory

        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(
            target=self._loop.run_forever, name="agent-runtime-loop", daemon=True
        )
        self._thread.start()

        self._draft_agent: Agent[AgentDeps, DraftResult] = Agent(
            draft_model or build_model(AgentTask.DRAFT),
            deps_type=AgentDeps,
            output_type=DraftResult,
            toolsets=[memory_toolset],
            retries=1,
        )

        @self._draft_agent.instructions
        def _draft_instructions(ctx: RunContext[AgentDeps]) -> str:
            return ctx.deps.system_instruction

        self._critic_agent: Agent[AgentDeps, CriticVerdict] = Agent(
            critic_model or build_model(AgentTask.CRITIC),
            deps_type=AgentDeps,
            output_type=CriticVerdict,
            instructions=_CRITIC_INSTRUCTIONS,
            retries=1,
        )
        self._extraction_agent: Agent[AgentDeps, ExtractionResult] = Agent(
            extract_model or build_model(AgentTask.EXTRACT),
            deps_type=AgentDeps,
            output_type=ExtractionResult,
            instructions=_EXTRACTION_INSTRUCTIONS,
            retries=1,
        )
        self._summary_agent: Agent[AgentDeps, SummaryResult] = Agent(
            summary_model or build_model(AgentTask.SUMMARIZE),
            deps_type=AgentDeps,
            output_type=SummaryResult,
            instructions=_SUMMARY_INSTRUCTIONS,
            retries=1,
        )
        self._topic_agent: Agent[AgentDeps, TopicResult] = Agent(
            topic_model or build_model(AgentTask.TOPIC),
            deps_type=AgentDeps,
            output_type=TopicResult,
            instructions=_TOPIC_INSTRUCTIONS,
            retries=1,
        )
        self._rerank_agent: Agent[AgentDeps, RerankResult] = Agent(
            rerank_model or build_model(AgentTask.RERANK),
            deps_type=AgentDeps,
            output_type=RerankResult,
            instructions=_RERANK_INSTRUCTIONS,
            retries=1,
        )
        self._pattern_agent: Agent[AgentDeps, PatternResult] = Agent(
            pattern_model or build_model(AgentTask.REFLECT),
            deps_type=AgentDeps,
            output_type=PatternResult,
            instructions=_PATTERN_INSTRUCTIONS,
            retries=1,
        )

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
    ) -> AgentDeps:
        return AgentDeps(
            tenant_id=tenant_id,
            chat_id=chat_id,
            privacy_flags=tuple(f.value for f in privacy_flags),
            source_message_id=source_message_id,
            system_instruction=system_instruction,
            pool=pool,
            embedder=self._query_embedder_factory() if self._query_embedder_factory else None,
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
    ) -> None:
        """Push one generation (and trace/safety spans) to the Langfuse sink.

        Mirrors the recording that used to live in ``ModelRouter._record_to_sink``
        so the agent path produces the same observability as the legacy path did.
        ``record_generation`` hashes the prompt when ``no_training`` is set.
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
        self._sink.record_generation(
            trace_id=ctx.trace_id,
            model=model_name_for(task) or "test-model",
            prompt=f"[system]\n{system_prompt[:400]}\n[user]\n{user_payload[:400]}",
            completion=completion,
            privacy_flags=privacy_flags,
            usage={"input_tokens": usage.input_tokens, "output_tokens": usage.output_tokens},
        )

    @staticmethod
    def _usage_of(result: object) -> Usage:
        """Normalize a pydantic-ai run usage into the contract ``Usage``."""
        usage = result.usage  # type: ignore[attr-defined]
        return Usage(input_tokens=usage.input_tokens or 0, output_tokens=usage.output_tokens or 0)

    # ------------------------------------------------------------------ draft

    def draft_outcome(
        self,
        request: AIProcessMessageRequest,
        risk_level: RiskLevel,
        risk_reasons: tuple[str, ...] = (),
    ) -> draft_pipeline.DraftOutcome:
        """Generate the structured draft (and critic review) for process-message.

        Raises RuntimeError on agent failure so the service's error-fallback
        branch behaves identically to the legacy path.  A safety block is not a
        failure: it returns a canned-reply outcome (model ``"blocked"``).
        """
        preflight = draft_pipeline.preflight_outcome(request, risk_level, risk_reasons)
        if preflight is not None:
            return preflight
        ctx = current_context()
        try:
            return self._run(self._draft_async(request, risk_level, ctx))
        except SafetyBlocked as exc:
            return draft_pipeline.DraftOutcome(
                text=exc.canned_reply, model_used="blocked", usage=Usage()
            )
        except Exception as exc:  # noqa: BLE001 — single conversion point to the contract error
            raise RuntimeError(f"agent draft failed: {exc}") from exc

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

    async def _draft_async(
        self, request: AIProcessMessageRequest, risk_level: RiskLevel, ctx: LLMCallContext
    ) -> draft_pipeline.DraftOutcome:
        system_prompt, user_payload = draft_pipeline.build_draft_prompt(
            request, self._prompt_builder
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
            pool=await self._get_pool(),
        )
        mcp_toolsets = self._resolve_mcp_toolsets(request.ai_role, privacy_flags)
        result = await self._draft_agent.run(
            user_payload,
            deps=deps,
            usage_limits=UsageLimits(tool_calls_limit=_DRAFT_TOOL_CALLS_LIMIT),
            toolsets=mcp_toolsets,
        )
        drafted: DraftResult = result.output
        total = self._usage_of(result)
        model_used = model_name_for(AgentTask.DRAFT) or "test-model"

        text = drafted.reply
        confidence = drafted.confidence
        if risk_level in _CRITIC_RISKS or confidence < _CRITIC_CONFIDENCE_BAR:
            feedback_hint = draft_pipeline.build_feedback_hint(request.feedback_context)
            text, confidence, critic_usage = await self._review_draft(
                deps, request.event.message_text, drafted, ctx, privacy_flags, feedback_hint
            )
            total = Usage(
                input_tokens=total.input_tokens + critic_usage.input_tokens,
                output_tokens=total.output_tokens + critic_usage.output_tokens,
            )
            model_used += "+critic"

        # Phase 3: deanonymize + non-blocking flags on the final text.
        text, _ = self._scan_output(text, ModelTask.DRAFT_REPLY, ctx)
        self._record_run(
            ctx, AgentTask.DRAFT, system_prompt, user_payload, drafted.reply, total, privacy_flags
        )
        return draft_pipeline.DraftOutcome(
            text=text,
            model_used=model_used,
            usage=total,
            intent=drafted.intent,
            options=drafted.options if drafted.intent == "choice_question" else (),
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
    ) -> tuple[str, float, Usage]:
        """Run the critic; returns (final_text, final_confidence, usage).

        A revision halves the surviving confidence: a draft the critic had to
        fix must not auto-send on the strength of its original self-score.
        Critic failures keep the original draft — review is best-effort.

        ``feedback_hint`` carries the same owner-feedback anti-examples the draft
        agent saw, so the critic can flag a reply written in a style the owner
        has already rejected (self-correction grounded in past outcomes).
        """
        review_payload = (
            "Сообщение собеседника:\n"
            f"{message_text}\n\n"
            "Черновик ответа от лица владельца:\n"
            f"{drafted.reply}"
        )
        if feedback_hint:
            review_payload += "\n\n" + feedback_hint
        try:
            review = await self._critic_agent.run(review_payload, deps=deps)
        except Exception as exc:  # noqa: BLE001 — critic is advisory, never fatal
            logger.warning("critic agent failed; keeping original draft", extra={"error": str(exc)})
            return drafted.reply, drafted.confidence, Usage()
        verdict: CriticVerdict = review.output
        critic_usage = self._usage_of(review)
        self._record_run(
            ctx, AgentTask.CRITIC, _CRITIC_INSTRUCTIONS, review_payload,
            verdict.revised_reply or verdict.verdict, critic_usage, privacy_flags,
        )
        if verdict.verdict == "revise" and verdict.revised_reply.strip():
            logger.info(
                "critic revised draft",
                extra={"tenant_id": deps.tenant_id, "chat_id": deps.chat_id, "issues": list(verdict.issues)},
            )
            return verdict.revised_reply.strip(), min(drafted.confidence, 0.5), critic_usage
        return drafted.reply, drafted.confidence, critic_usage

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
        _, user_payload = self._prompt_builder.build("", user_content, ())
        _, user_payload = self._scan_input(
            "", user_payload, ModelTask.EXTRACT_FACTS, ctx
        )
        deps = self._deps_for(
            request.tenant_id, request.chat_id, request.privacy_flags, pool=None
        )
        result = await self._extraction_agent.run(user_payload, deps=deps)
        self._record_run(
            ctx, AgentTask.EXTRACT, "", user_payload,
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
        _, user_payload = self._prompt_builder.build("", "\n".join(request.messages), ())
        _, user_payload = self._scan_input(
            "", user_payload, ModelTask.SUMMARIZE_CHAT, ctx
        )
        deps = AgentDeps(tenant_id="", chat_id="")
        result = await self._summary_agent.run(user_payload, deps=deps)
        text, _ = self._scan_output(result.output.summary, ModelTask.SUMMARIZE_CHAT, ctx)
        usage = self._usage_of(result)
        self._record_run(
            ctx, AgentTask.SUMMARIZE, "", user_payload, text, usage, request.privacy_flags
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
        _, user_payload = self._prompt_builder.build("", content, ())
        _, user_payload = self._scan_input("", user_payload, ModelTask.EXTRACT_FACTS, ctx)
        deps = AgentDeps(tenant_id=request.tenant_id, chat_id=request.chat_id)
        result = await self._topic_agent.run(user_payload, deps=deps)
        topic = result.output.topic.strip()
        self._record_run(ctx, AgentTask.TOPIC, "", user_payload, topic, self._usage_of(result), ())
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
        _, user_payload = self._prompt_builder.build("", content, ())
        _, user_payload = self._scan_input("", user_payload, ModelTask.EXTRACT_FACTS, ctx)
        deps = AgentDeps(tenant_id="", chat_id="")
        result = await self._rerank_agent.run(user_payload, deps=deps)
        self._record_run(
            ctx, AgentTask.RERANK, "", user_payload,
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
        _, user_payload = self._prompt_builder.build("", content, ())
        _, user_payload = self._scan_input("", user_payload, ModelTask.EXTRACT_FACTS, ctx)
        deps = AgentDeps(tenant_id=request.tenant_id, chat_id=request.chat_id)
        result = await self._pattern_agent.run(user_payload, deps=deps)
        usage = self._usage_of(result)
        self._record_run(
            ctx, AgentTask.REFLECT, "", user_payload,
            result.output.pattern, usage, request.privacy_flags,
        )
        return result.output, usage


def build_runtime(
    *,
    input_safety: InputSafetyScanner | None = None,
    output_safety: OutputSafetyScanner | None = None,
    sink: LangfuseSink | None = None,
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
    return AgentRuntime(
        database_url=database_url,
        input_safety=input_safety,
        output_safety=output_safety,
        sink=sink,
        query_embedder_factory=embedder_factory,
    )
