# DoubleU — Engineering Backlog

---

## Inefficiency Backlog

- ~~`applyMigrations` re-executes all migration files on every startup.~~ **✅ Done** — `schema_migrations` tracking table added; each SQL file runs exactly once.
- `ContextScore` recomputes on every mode selection check — could be cached per chat with a short TTL.
- `RecentMessages` in in-memory mode iterates all messages on every call — O(n). Switch to sorted structure for large in-memory datasets.
- ~~**Synchronous LLM fact extraction in the hot reply path.**~~ **✅ Done** — extraction moved to async goroutine after draft dispatch; regex `TypedMemoryExtractor` is the synchronous fallback.
- ~~**Unbounded goroutine fan-out in background summaries.**~~ **✅ Done** — `maxSummaryConcurrency = 8` semaphore added to both `runDailySummariesForDate` and `closeStaleEpisodes`.
- ~~**Recency-only retrieval instead of semantic recall.**~~ **✅ Done** — `SemanticFactsByVector` / `GlobalSemanticFactsByVector` / `EpisodeSummariesByVector` replace `created_at DESC`; query vector is embedded at retrieval time with 2-second timeout fallback.

---

## Bug Backlog

- ~~**[CRITICAL] Semantic memory self-poisoning loop.**~~ **✅ Done** — Python `memory_used` now returns only newly extracted facts; Go adds `contextMarkerPrefixes` guard; regression test added (`test_memory_used_does_not_echo_input_context`).
- ~~Draft `OwnerMessageID` is not persisted to PostgreSQL.~~ **✅ Done** — `drafts` table added; `SaveDraft` / `UpdateDraft` / `Draft` / `DraftByMessage` are DB-backed with in-memory cache fallback.
- ~~**[CRITICAL] `drafts.suggested_options` NOT NULL violation — drafts never persist to Postgres.**~~ **✅ Done** — `nilToEmpty` helper added; `SaveDraft` / `UpdateDraft` wrap `SuggestedOptions` and `ActionHistory` so `pq.Array(nil)` → `'{}'::text[]` is no longer sent as SQL NULL.
- ~~**[CRITICAL] `contact_profiles.topics / display_names / do_not_discuss` NOT NULL violation — profiles never persist.**~~ **✅ Done** — `UpsertContactProfile` now wraps `DisplayNames` / `Topics` / `DoNotDiscuss` (and `user_settings.SecondaryLanguages`) through the same `nilToEmpty` helper.
- ~~**[CRITICAL] `upsert_semantic_fact` stored proc broken — no semantic facts can be saved.**~~ **✅ Done** — migration `002_fix_upsert_semantic_fact.sql` replaces `array_length(fe.embedding, 1) > 0` with `vector_dims(fe.embedding::vector) > 0`; migration `003_embeddings_qwen_4096.sql` rewrites the function for the new `vector(4096)` column shape; both pick up automatically via `applyMigrations`.
- ~~**[CRITICAL] `/ai/embed-texts` always returns `model_used: "error"` — no embeddings produced.**~~ **✅ Done** —
  1. `embed_texts` now logs upstream failures via `logger.warning("embed_texts failed", ...)` instead of silently swallowing them.
  2. Default embedding model switched to `qwen/qwen3-embedding-8b` in [schemas.py](../ai-python/ai_agent/schemas.py), [service.go](../backend-go/internal/core/service.go); migration 003 updates the `embedding_model` column default.
  3. `embed_texts` now prefers `OPENROUTER_BASE_URL` / `OPENROUTER_API_KEY` when configured, falling back to OpenAI envs only when OpenRouter is absent; `HTTP-Referer` / `X-Title` headers are passed through when running against OpenRouter.
  4. Migration `003_embeddings_qwen_4096.sql` truncates `fact_embeddings`, NULLs `episode_summaries.embedding`, drops obsolete HNSW indexes, and resizes both columns to `vector(4096)`. Sequential cosine scan only — `halfvec(4096) + HNSW` is left for the scale-up phase.
  5. The embedding worker re-runs every 5 minutes (`StartEmbeddingWorker`) so backfill happens automatically after the migration applies.
  6. Regression tests added: `test_embed_texts_logs_when_upstream_call_fails` and `test_embed_texts_prefers_openrouter_base_url`.

---

## Tech Debt

- No versioning scheme for migrations. Need a proper migration runner (e.g. goose, atlas) to track applied migrations and support rollback.
- The `SaveMessage` / `AppendState` ordering issue was worked around by checking `EventID` in-memory. The correct fix is to ensure `SaveMessage` always runs before any state/audit writes, with a refactor of `HandleTelegramWebhook` order.
- ~~`BotSender.SendMessageGetID` duplicates most of `BotSender.SendMessage`.~~ **✅ Done** — `sendMessageBody` / `sendMessageRaw` helpers extracted; both callers share the transport.
- ~~No connection pooling configuration for PostgreSQL.~~ **✅ Done** — `SetMaxOpenConns(25)` / `SetMaxIdleConns(5)` / `SetConnMaxLifetime(1h)` configured in `NewPostgresRepository`.
- ~~**Typed memory contract between Go and Python.**~~ **✅ Done** — `TypedExtractionRequest` / `TypedExtractionResponse` with structured `TypedFact` objects replace flat `[]string`; Go routes by `fact_type`/`subject` deterministically.
- **Single source of truth for the cross-service contract.** The enum/contract is hand-maintained in three places: Go [contracts/types.go](../backend-go/internal/contracts/types.go), Python [schemas.py](../ai-python/ai_agent/schemas.py), and SQL enums in [001_clean_schema.sql](../infra/migrations/001_clean_schema.sql). Drift is only caught by tests. Consider generating Go + Python types from one source (JSON Schema / protobuf) per the frozen-contract rule.
- ~~**Dead derived-state SQL functions.**~~ **✅ Done** — `GreetedToday`, `ChatPausedHours`, `OpenCommitments` wired in Go; Python no longer re-derives these from message history.
- ~~**Split [ai-python/ai_agent/service.py](../ai-python/ai_agent/service.py) into pipeline modules.**~~ **✅ Done** — `service.py` shrunk from 658 → 224 lines and is now a thin orchestrator. Per-endpoint logic moved under `ai_agent/pipelines/`: `draft.py` (`generate_draft` + `maybe_generate_response` + greeting/style helpers + `_build_feedback_hint`), `extraction.py` (`extract_facts_typed` + `_extract_facts_llm_typed` + `_normalize_typed_fact`), `memory.py` (`extract_memory` wrapper), `summarization.py` (`summarize_chat` + `_looks_actionable`), `embedding.py` (`embed_texts` with urllib/OpenRouter→OpenAI fallback), `risk.py` / `intent.py` (thin wrappers around `classifiers.py`), `decisions.py` (`decide` + `confidence_for` + `SEMI_AUTO_MIN` / `AUTO_MIN` thresholds). Dead `_extract_facts_llm` (semicolon-list legacy) removed. Existing tests (24/24 passing) updated to patch `pipelines.embedding.logger` instead of `service.logger`; docstring contract test now also covers every new pipeline module.

---

## Memory / RAG Implementation Gap (design vs implementation)

Items below are **in dependency order**. The self-poisoning bug fix is a prerequisite for all of them — ✅ Done.

1. ~~**Real embeddings + pgvector.**~~ **✅ Done** — `text-embedding-3-small` (1536-dim) via `/ai/embed-texts`; `fact_embeddings.embedding` migrated to `vector(1536)` with HNSW cosine index; background `StartEmbeddingWorker` runs every 5 minutes.
2. ~~**Semantic retrieval to replace `created_at DESC`.**~~ **✅ Done** — `SemanticFactsByVector` / `GlobalSemanticFactsByVector` / `EpisodeSummariesByVector` use `ORDER BY embedding <=> $query::vector`; query vector embedded in `conversationContext` with 2-second timeout; graceful recency fallback.
3. ~~**Retrieval weighting + hard-include (design §8.2).**~~ **✅ Done** — taboo + health facts always included unconditionally; commitment (÷1.5) and preference (÷1.2) effective-distance boosts applied in ORDER BY clause.
4. **Conflict resolution chat vs global (design §5.6).** When a chat fact and a global fact of the same `fact_type`/`subject` have cosine > 0.85, the chat fact should win and the global fact dropped from the prompt. Currently both are emitted without resolution.
5. ~~**Typed extraction + deterministic scope assignment (design §5.4).**~~ **✅ Done** — Python `ExtractFactsTyped` returns structured `TypedFact` with `fact_type`/`subject`/`scope_kind`; Go `saveTypedFacts` routes deterministically: `owner health/biographical` → `owner_global`; health → `pending_global_health_facts`; else → `scope_kind=chat`.
6. ~~**`upsert_semantic_fact` stored proc with race-safe dedup.**~~ **✅ Done** — 3-tier dedup stored proc: exact hash → cosine ≥0.97 (occurrence) → cosine 0.85–0.97 (supersede) → insert; `pg_advisory_xact_lock` prevents races.
7. **Health confirmation flow.** Wire `pending_global_health_facts` + `accept_pending_health_fact` (design §9.3) and the owner-facing accept/reject UI. The table exists in schema but the flow is unused.
8. ~~**Episode model alignment.**~~ **✅ Done** — Episodes close on silence > 6h (`"silence_gap"`) OR 40 messages (`"message_limit"`); `closure_reason` now reflects the actual trigger; daily archiver uses `"daily_summary"`.

---

## Repo Hygiene / Garbage

- ~~Add `.gitignore` entries for `__pycache__/` and `.pytest_cache/`.~~ **✅ Done** — `.gitignore` created.
- ~~Remove or rename `docs/MEMORY_ARCHITECTURE (1).md`.~~ **✅ Done** — renamed to `docs/MEMORY_ARCHITECTURE.md`.
- ~~Remove root scratch file `step.txt`.~~ **✅ Done** — deleted.
- `prototypeui/` — contains 6 dark-theme HTML prototype screens; not wired into the running app. Keep as design reference or delete when Iteration 2 mini-app replaces them.

### Implemented vs Designed — Memory Architecture

The document `docs/MEMORY_ARCHITECTURE.md` describes the full target design. Current implementation status:

| Design Component | Status |
|---|---|
| `semantic_facts` table + typed schema | ✅ Implemented |
| `upsert_semantic_fact` stored proc (3-tier dedup) | ✅ Implemented |
| `fact_embeddings` with pgvector + HNSW | ✅ Implemented |
| Background embedding worker | ✅ Implemented |
| Typed fact extraction (Python → Go) | ✅ Implemented |
| Deterministic scope routing | ✅ Implemented |
| Vector similarity retrieval | ✅ Implemented |
| §8.2 weighting + hard-include taboo/health | ✅ Implemented |
| `active_episodes` + episode lifecycle (6h/40msg) | ✅ Implemented |
| `episode_summaries` + LLM compression | ✅ Implemented |
| `pending_global_health_facts` queue | ✅ Stored proc only — no owner-facing confirmation UI |
| §5.6 chat-vs-global conflict resolution | ❌ Not implemented |
| §9.3 health confirmation flow (accept/reject UI) | ❌ Not implemented |

---

## Iteration 2 (not started)

- Mini App UI — web dashboard for draft review, memory viewer, chat management
- AI Roles Marketplace — persona presets per chat (Sales, Support, Personal assistant)
- Telegram Client API / MTProto — historical message access for tenants granting full access

---

## LLM Safety & Observability Pipeline (planned)

End-to-end safety + logging layer for every LLM call. Goal: nothing reaches a provider unscanned, nothing returns unvalidated, every call is traced with cost/latency/quality signals. Target topology:

```
Client / Bot / Web
  → Go Backend (HandleTelegramWebhook)
  → Input Safety Layer  (PII redact · prompt-injection scan · moderation)
  → LLM Gateway         (LiteLLM/Helicone/Portkey · rate · budget · routing)
  → LLM Provider        (OpenRouter / OpenAI / DeepSeek / local)
  → Output Safety Layer (toxicity · jailbreak · schema validation)
  → Observability       (Langfuse / Phoenix / Opik / OpenLLMetry)
```

All stages must:
- be **idempotent** (re-running on the same `event_id` yields the same trace ID, not a new one);
- respect the privacy flags `no_memory` / `no_training` / `no_summary` / `private` at every stage (e.g. `no_training=true` ⇒ never send to Langfuse user-prompt field, only redacted hash);
- never block the hot reply path on the observability sink — observability is fire-and-forget with bounded queue.

### Phase 0 — Foundations (prereq for everything else)

- [ ] **Single `LLMCall` envelope** in `contracts/` — a frozen struct that wraps every outbound LLM request: `tenant_id`, `chat_id`, `event_id`, `purpose` (`draft|extract|summarize|embed|moderate`), `privacy_flags`, `model_hint`, `prompt_parts[]`, `schema_id`. Both Go and Python must construct/parse it. Forbid raw `openai.ChatCompletion.create(...)` calls outside the gateway client.
- [ ] **`trace_id` propagation.** Generate at the Go entrypoint, pass through HTTP headers (`X-DoubleU-Trace-ID`) to Python, attach to every span and DB write (`audit_events.trace_id` column — needs migration).
- [ ] **Config flags** (`SAFETY_INPUT_ENABLED`, `SAFETY_OUTPUT_ENABLED`, `LLM_GATEWAY`, `OBSERVABILITY_SINK`) so each stage can be ramped independently behind feature flags.

### Phase 1 — Input Safety Layer

Sits between `ConversationContext` assembly and the gateway call. Implemented as a Python middleware in `ai-python/ai_agent/safety/input.py` plus a thin Go pre-check for fail-fast cases.

- [ ] **PII / secrets redaction.** Replace emails, phones, card numbers, IBANs, JWTs, AWS/OpenAI keys with deterministic placeholders (`<EMAIL_1>`, `<CARD_1>`) before the prompt is built. Keep a per-call mapping table in memory so the output layer can re-inflate or strip placeholders. Use Microsoft Presidio or `presidio-analyzer` for detection; fall back to regex pack for the hot path.
- [ ] **Prompt-injection scan.** Classifier on incoming user text **before** it joins the system prompt. Candidates: `protectai/deberta-v3-base-prompt-injection-v2` (local, <50ms) or LLM-as-judge call to a small fast model. On positive hit: refuse, log to `audit_events` with `decision=blocked_injection`, return safe canned reply.
- [ ] **Moderation gate.** Run OpenAI/Mistral moderation (or local `unitary/toxic-bert`) on the user message; classify into `sexual`, `violence`, `self-harm`, `hate`. Map to backend policy (refuse / soften / escalate to owner). Already partly present in `internal/core/service.go` — formalize as a separate stage.
- [ ] **Wrap untrusted input in XML tags** per CLAUDE.md — enforced by the safety layer, not the prompt author.

### Phase 2 — LLM Gateway

One process / sidecar in front of every provider. Recommended primary: **LiteLLM Proxy** (OSS, self-hostable, native OpenAI-compatible interface so existing `AsyncOpenAI` clients keep working). Helicone or Portkey can be added as the observability proxy in front of LiteLLM if managed dashboards are desired.

- [ ] **Deploy LiteLLM proxy** in `docker-compose.yml` alongside `ai-python`. Configure provider keys (OpenRouter, OpenAI, DeepSeek, local) in `config.yaml`. Python service points `OPENROUTER_BASE_URL` → `http://litellm:4000` instead of `openrouter.ai/api/v1`.
- [ ] **Rate limits** per `tenant_id` and per `purpose`. Cheap embed calls get a high limit; draft generation gets a tighter one. Enforce via LiteLLM router config.
- [ ] **Budget limits.** Daily / monthly USD cap per tenant. On 80% — log warning; on 100% — switch routing to cheap fallback model (`deepseek-chat`) instead of refusing.
- [ ] **Routing rules.** `purpose=draft` → `claude-sonnet-4-6`; `purpose=extract` → cheap model; `purpose=embed` → `qwen3-embedding-8b`; `purpose=moderate` → local. Override-able per chat via `user_settings`.
- [ ] **Fallback chain.** Primary → secondary → local. Retries with exponential backoff are owned by the gateway, NOT by `ai-python` — remove ad-hoc retries from Python once gateway is in.
- [ ] **Idempotency key.** Pass `event_id` as `X-Idempotency-Key`; the gateway dedups duplicate webhook replays to the provider.

### Phase 3 — Output Safety Layer

Mirror of Phase 1 but on the response side. Implemented in `ai-python/ai_agent/safety/output.py`.

- [ ] **Schema validation.** Every LLM response we expect to be JSON (typed extraction, draft suggestions, summary) must parse against a frozen Pydantic schema. On failure: one repair retry with a small model, then escalate to error path. No silent dropping.
- [ ] **Toxicity / jailbreak detector** on the model's output text (not just the input). Classifier on the draft before it is queued for owner review. If toxic: tag draft `flagged=true`, owner sees a warning chip in the UI.
- [ ] **Secret-leak guard.** Scan output for anything resembling an API key, internal URL, or the redacted placeholders from Phase 1 leaking back un-mapped. Drop / regenerate.
- [ ] **Privacy enforcement on output.** Strip any text that would violate `no_summary` / `private` flags before persisting to memory tables.

### Phase 4 — Observability

Primary sink: **Langfuse** (OSS, self-hostable, native trace/span model fits our `event_id` → multi-LLM-call topology). Secondary: **OpenLLMetry** OTel exporter for unified metrics in Grafana if we already run Tempo/Loki.

- [ ] **Deploy Langfuse** as a container in `docker-compose.yml` (Postgres + ClickHouse + worker). Bind `LANGFUSE_HOST` / `PUBLIC_KEY` / `SECRET_KEY` in `.env`.
- [ ] **Instrument Python** — wrap every gateway call with `langfuse.observe()` decorator (or OTel `@trace`). One trace per `event_id`; each LLM call is a span; safety stages are sibling spans with `pass/block` attributes.
- [ ] **Instrument Go** — emit a parent span in `HandleTelegramWebhook` and propagate `trace_id` to Python via header so spans stitch into one trace across services.
- [ ] **Cost + token metrics.** LiteLLM already emits these; pipe to Langfuse so dashboards show $/tenant, $/chat, $/purpose.
- [ ] **Quality signals.** Owner accept/edit/reject of a draft is a score event sent to Langfuse against the originating trace — closes the loop for offline eval.
- [ ] **Alerts.** Langfuse webhook → Slack/Telegram on `block_rate > X%`, `cost_per_hour > $Y`, `p95_latency > 3s`, `error_rate > 1%`.

### Phase 5 — Migration & cutover

- [ ] **Migrate existing call sites** one at a time, in this order: `embed_texts` → typed extraction → summary → draft generation. Each migration is a separate PR.
- [ ] **Shadow mode first.** New pipeline runs in parallel with the old path; outputs are compared, divergences logged. Cut over when divergence < 0.5%.
- [ ] **Kill the old path.** Delete direct `AsyncOpenAI` clients from `ai-python`; the only allowed LLM client is `gateway_client.py`.

### Out of scope for this initiative (separate backlog items)

- Adversarial red-teaming harness (belongs under Tech Debt / QA).
- Fine-tuning a custom moderation classifier on owner-labelled drafts.
- Data Loss Prevention beyond the regex+Presidio pack (e.g. ML-based DLP).

### Dependencies

- Phase 0 blocks Phase 1–4 (envelope + trace propagation are prereqs).
- Phase 2 (gateway) must land before Phase 4 observability becomes useful — otherwise every direct provider call bypasses both.
- Phase 5 migration cannot start until 1–4 are at least feature-flagged and tested end-to-end against a staging chat.
