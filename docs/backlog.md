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
