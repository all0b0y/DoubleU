# DoubleU — Engineering Backlog

---

## Inefficiency Backlog

- `ContextScore` recomputes on every mode selection check — could be cached per chat with a short TTL.
- `RecentMessages` in in-memory mode iterates all messages on every call — O(n). Switch to sorted structure for large in-memory datasets.

---

## Tech Debt

- **Promptfoo CI eval harness for prompt regressions.** **Offline-only** — does not touch the runtime safety/observability pipeline. Runs against `MOCK_LLM=true` for determinism + zero cost (mock gateway already produces stable outputs for known keywords). File structure:
  - `promptfooconfig.yaml` at repo root — providers per purpose pointing to `eval/providers/<purpose>.py` thin scripts; tests reference jsonl datasets.
  - `eval/datasets/{draft_golden,extract_golden,summarize_golden,injection_probes}.jsonl` — 30–50 hand-labeled examples per purpose; 20 known injection probes with expected `refused` / canned reply.
  - `eval/providers/{draft,extract,summarize,inject}.py` — load AI service in-process (MOCK), POST row to endpoint, return JSON.
  - `eval/baseline/main.json` — baseline scores from last `main` build, CI artifact.
  - `.github/workflows/prompt-eval.yml` — triggers on `pull_request` with `paths: [ai-python/ai_agent/pipelines/**, eval/**, ai-python/ai_agent/security.py]`. Installs `promptfoo`, runs eval, downloads `main` baseline, compares via `scripts/promptfoo-diff.py`. Fails PR if pass-rate delta < −2 pp OR any injection probe fails. Baseline refreshes on push to `main`.
  - Assertions: `is-valid-json` (all); `not-icontains: <EMAIL_` (draft, no placeholder leak); `output.facts.every(f => valid_fact_type)` (extract); ROUGE-L ≥ 0.5 + `not-icontains` PRIVATE/NO_SUMMARY (summarize); canned-reply equality OR `decision="ignore"`+`reason="blocked_injection"` (injection).
  - Future: optional live-provider eval gated on `eval:live` PR label (`deepseek-chat` at ~$0.05/PR) — needs budget cap + secret.

---

## Memory / RAG Implementation Gap

Items below are in dependency order.

1. **Conflict resolution chat vs global (design §5.6).** When a chat fact and a global fact of the same `fact_type`/`subject` have cosine > 0.85, the chat fact should win and the global fact dropped from the prompt. Currently both are emitted without resolution.
2. **Health confirmation flow.** Wire `pending_global_health_facts` + `accept_pending_health_fact` (design §9.3) and the owner-facing accept/reject UI. The table exists in schema but the flow is unused.
3. **Extraction is fact-starved — single-message context (evidence-based).** Production AI-service logs show `/ai/extract-facts-typed` runs healthily (~2 s, valid `[]`) but is invoked on **single short messages** with `memory_context_count: 0` (e.g. "Просто интересно"). Casual small talk has no durable facts in isolation, so almost everything correctly returns `[]` — only 1 fact extracted across the dataset. Now that `extraction_log` is populated (model/raw_output/created/skipped/duration), use it to measure before/after, then: (a) pass a small recent-conversation window into `TypedExtractionRequest` so multi-turn facts ("учусь на медика" spread over messages) become extractable — dedup via `normalized_fact_hash` makes re-extraction safe; (b) re-tune the `confidence ≥ 0.8` floor and the transience filter in `extraction.py` if the log shows over-filtering. Do NOT change extraction semantics blind — read `extraction_log` first.
4. **Episode `current_topic` always NULL — LLM topic tagging.** The prompt slot exists ([service.go conversationContext](../backend-go/internal/core/service.go)) but nothing populates `active_episodes.current_topic`. Deterministic tagging is too noisy; needs a cheap LLM topic label on episode update (or reuse the summarizer). `pending_owner_action` is already covered functionally by the unsent-drafts block in the prompt, so it is lower priority.

### Memory Architecture Status Gaps

| Design Component | Status |
|---|---|
| `pending_global_health_facts` queue | Stored proc only — no owner-facing confirmation UI |
| §5.6 chat-vs-global conflict resolution | Not implemented |
| §9.3 health confirmation flow (accept/reject UI) | Not implemented |

---

## Memory Retrieval Quality Roadmap — ✅ COMPLETED

All 6 steps plus the two "After" items have been implemented. Shadow-measure
before shipping to production (see verification section in the plan file).

**Steps implemented:**
- ✅ Step 1: Real embeddings — Go embedding worker + episode summary embeddings ([`service.go`](../backend-go/internal/core/service.go))
- ✅ Step 2: Dedup stored proc — already in migrations 001/002; Go uses `UpsertSemanticFact`
- ✅ Step 3: Multi-turn extraction context window ([`extraction.py`](../ai-python/ai_agent/pipelines/extraction.py), `recent_messages` field)
- ✅ Step 4: Episodic summaries + embeddings + LLM topic tagging (`StartEpisodeCloser`, `StartDailySummaryScheduler` now started; [`topic.py`](../ai-python/ai_agent/pipelines/topic.py))
- ✅ Step 5: Cross-encoder reranking ([`reranking.py`](../ai-python/ai_agent/pipelines/reranking.py), `/ai/rerank-facts`, `rerankFacts()` in service.go)
- ✅ Step 6: BM25 hybrid search (migration 008, `SemanticFactsByBM25`, `rrfFuse` in service.go)
- ✅ After: v4 Temporal knowledge graph (`fact_relations` table in migration 009, `SaveFactRelation` in store.go)
- ✅ After: Sleep-time consolidation (`consolidate_facts()` in migration 010, `StartConsolidationWorker`)

**Open (shadow-measurement required before production cutover):**
- Run shadow harness on a fixed chat set before/after to confirm retrieval quality improvement.
- Measure `extraction_log` before/after multi-turn window to confirm fact yield improves.

### After steps 1–6 — deferred, do NOT start before retrieval works

These are architecturally *above* the current design and only make sense once the
base retrieval (steps 1–6) is functional and measured. Starting them earlier would
build on broken data.

- **v4 — Temporal knowledge graph (Zep/Graphiti, GraphRAG).** This is the one
  approach that is genuinely higher than the current flat-`semantic_facts` design
  and still fits the model. The fact types `commitment`, `event`, `relationship`
  (design §5.2) **evolve over time** ("promised to send the contract by Friday" →
  "sent it"); flat `semantic_facts` models this poorly, a graph with temporal edges
  models it well. Candidate for a v4 schema iteration, **only after** base retrieval
  (steps 1–6) is working and shadow-measured. Scope when picked up: model facts as
  nodes + time-bounded edges, supersession as edge invalidation rather than row
  delete, and a graph-aware read-path that resolves "current truth" at query time.

- **Sleep-time / memory consolidation (background reflection, Letta-style).**
  Periodically compress and merge facts in the background — the next logical step on
  top of the existing decay model (design §7.2). Distinct from step-2 write-time
  dedup: this is *offline* consolidation (merge near-duplicates that accumulated,
  collapse superseded chains, promote recurring chat facts, age out stale ones).
  Depends on steps 1–3 (a real, deduped semantic layer to reflect over) and pairs
  naturally with the v4 graph once it lands.

---

## Repo Hygiene / Garbage

- `prototypeui/` — contains 6 dark-theme HTML prototype screens; not wired into the running app. Keep as design reference or delete when Iteration 2 mini-app replaces them.

---

## Iteration 2

- Mini App UI — web dashboard for draft review, memory viewer, chat management.
- AI Roles Marketplace — persona presets per chat (Sales, Support, Personal assistant).
- Telegram Client API / MTProto — historical message access for tenants granting full access.

---

## LLM Safety & Observability Pipeline

**Open items:**

- `llm-guard==0.3.*` and `langfuse==2.*` still commented out in `requirements.txt` → real ML scanners are not running, only regex fallback.
- `Dockerfile.ai-python` missing prewarm step for llm-guard transformers.
- `docs/litellm-runbook.md` — runbook for adding model aliases, rotating keys, inspecting spend.
- `docs/dev-observability.md` — Phoenix RAG debug workflow.
- `infra/langfuse/alerts.json` — alert webhook config (block_rate / cost / latency / error_rate).
- `ai-python/ai_agent/pipelines/embedding.py` — embed path still hits OpenRouter directly; move it through LiteLLM during cutover cleanup.
- `Dockerfile.ai-python` — output scanner prewarm missing (same prewarm work as input scanners).
- `ai-python/requirements.txt` — `langfuse==2.*` still commented out; keep this open if SDK features are needed beyond the stdlib uploader.

### Phase 5 — Migration & Cutover

Actual per-purpose traffic comparison has not been run yet; follow `docs/cutover-runbook.md`.

- Run shadow mode for `embed` ≥7 days → `CUTOVER=true` for embed.
- Run shadow mode for `extract` → cutover.
- Run shadow mode for `summarize` → cutover.
- Run shadow mode for `draft`/`process_message` → cutover (last).
- Delete `OpenRouterLLMGateway` + dual-credential embed branch after all purposes cut over.

### Future / Separate Backlog

- Adversarial red-teaming harness (belongs under Tech Debt / QA).
- Fine-tuning a custom moderation classifier on owner-labelled drafts.
- Data Loss Prevention beyond the LLM Guard `Anonymize` / `Sensitive` scanners (e.g. domain-tuned ML DLP).
- Per-tenant LiteLLM virtual keys (hard isolation). Current plan uses one master key + headers for tenant attribution; switch grows Phase 2 by a tenant-provisioning hook in Go.
