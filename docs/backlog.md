# DoubleU — Engineering Backlog

---

## Bug Backlog

- **(rich formatting fallback, ПЕРЕОТКРЫТО 2026-06-20)** Раз-«фикс» от 2026-06-18 неверный: fallback в `sendRichRecipient`/`sendRecipient` (`backend-go/internal/core/service.go`) шлёт **сырой markdown как plain** (`parse_mode=""`), из-за чего собеседник видит `###`, `**`, `1.`. Нужна деградация markdown→Telegram HTML, а при отказе HTML — вычистка в plain; сырой markdown не слать никогда. План: `docs/rich_models_streaming_plan_2026-06-20.md` (Фаза A). Старый юнит-тест, ассертящий пустой `parse_mode`, переписать.
- **(rich formatting, 2026-06-20)** Модель ставит `#`/`###` в одну строку с текстом/эмодзи (`😊 ### …`) — markdown требует пустую строку перед заголовком, rich парсится криво. Промпт всегда требует форматирование при роли, но доставка решает про rich отдельно (рассинхрон). План: Фаза B (ужесточить промпт + `normalize_rich_markdown`).

- ~~**(rich formatting slice 2, 2026-06-18)** `DraftType="warning"` violated the `drafts.draft_type` CHECK.~~ → ✅ закрыто (2026-06-18): migration 109 extends the CHECK to include `warning`, with a rollback and smoke insert check.

- ~~**Stale contract seed against consolidated schema** — `infra/seed/001_contract_scenarios.sql` inserted into dropped/renamed memory and role tables.~~ → ✅ закрыто (2026-06-18): seed now uses `ai_roles`, `semantic_facts`, and `episode_summaries`; the smoke verifier checks `semantic_facts`.

---

## AI / Backend Architecture Backlog

**✅ ВЫПОЛНЕН (2026-06-11)** — все 15 задач ARCH-1…ARCH-15 реализованы, статус и остаточные
хвосты в **[ai_backend_architecture_backlog.md](ai_backend_architecture_backlog.md)**.
Итог: горячий путь — `embed + 1 структурированный LLM-вызов`
(reply/intent/options/confidence/topic в одном JSON, ARCH-4); critical-warning отделён от
черновика и недоставим собеседнику (owner_note + warning-карточка, ARCH-1); LLM-reranker
заменён косинусом в SQL (ARCH-5); вебхуки — шардированная очередь с мгновенным ACK и
ограниченным пулом async-LLM-задач (ARCH-7); контекст собирается параллельно (ARCH-8)
с токен-бюджетом 4000 (ARCH-15/MEM-9).

**Открытые cutover-замеры (не код):**
- Shadow-сравнение порядка фактов LLM-rerank vs cosine на ~50 реальных запросах → после него удалить `/ai/rerank-facts` + `FactReranker` (ARCH-5 п.3).
- `extraction_log` до/после pre-filter; решение про батчинг extraction по эпизодам (MEM-13).

---

## Inefficiency Backlog

- ~~`ContextScore` recomputes on every mode selection check~~ → ✅ TTL-кэш + инвалидация на write (ARCH-8).
- ~~`RecentMessages` in-memory O(n)~~ → ✅ per-chat индекс (ARCH-13).
- ~~`Repository.UpsertSemanticFact` unbounded in-memory slice growth~~ → ✅ аппенд на DB-пути убран (ARCH-13).
- **(rich formatting, 2026-06-18)** `richFormattingEnabled` runs a separate `UserSettings` lookup per rich reply in `sendRecipient`. Fold the toggle read into the settings already loaded on the hot path to avoid the extra query.

---

## Tech Debt

- ~~**(v5 roles)** `upsert_semantic_fact` stored proc dead `RAISE EXCEPTION` branch~~ → ✅ закрыто миграцией 013 (v3 процедуры без global-веток, MEM-1).
- ~~**(v5 roles)** `/ai/generate-draft` (`DraftRequest`) does not carry `ai_role`~~ → ✅ эндпоинт помечен deprecated в openapi (ARCH-14): regenerate идёт через process-message, Go-клиент standalone-вызов не делает.
- ~~**(v5 roles)** Role editing UX is `|`-delimited one-liner (`/role_create`); a guided multi-step dialog (pending-input flow like draft edit) would be friendlier for long descriptions.~~ → ✅ закрыто Mini App (2026-06-12): экран `/roles` + форма с 5 полями (`miniapp/screens/role-edit.js`), REST CRUD `/api/v1/roles`; бот-команда осталась как fallback.

- **(bot ingestion, 2026-06-11)** `user_bots.token` хранится в БД открытым текстом (в логи не пишется, наружу не отдаётся). Нужно app-level шифрование ключом из env перед продом.
- **(bot ingestion, 2026-06-11)** Long polling (`getUpdates`) — решение для MVP/отладки. Для прода заменить на webhook per bot: транспорт инкапсулирован в `internal/botservice/poller.go`, точка замены одна (manager поверх UpdateSource), core не трогается.
- **(pydantic-ai, 2026-06-11)** HTTP-сервер ai-python остался на stdlib (`http_server.py`): миграция на FastAPI/uvicorn отложена осознанно — контракт и context-propagation покрыты тестами, агентный runtime мостится через выделенный event-loop-тред. Переходить на FastAPI имеет смысл вместе с переводом `schemas.py` (frozen dataclasses) на pydantic BaseModel — оба шага меняют сериализацию и должны идти под защитой `test_contracts.py`.
- **(pydantic-ai, 2026-06-11)** `search_facts` агентного toolset: embedding-ветка (cosine по `fact_embeddings`) требует прокинутый `deps.embedder` — сейчас runtime его не настраивает, и поиск работает по ILIKE-fallback. Подключить embedder через OpenRouter `/embeddings` (alias `embed-qwen`) при cutover памяти.
- **(pydantic-ai, 2026-06-11)** При `AI_MEMORY_SOURCE=python` LLM-extraction пишет факты из runtime (см. `_persist_extraction`), но `extraction_log` продолжает писать Go с пустыми `facts_created` — счётчики диагностики надо перенести в Python или возвращать created-IDs в ответе.

- **Promptfoo CI eval harness for prompt regressions.** **Offline-only** — does not touch the runtime safety/observability pipeline. Runs against `MOCK_LLM=true` for determinism + zero cost (mock gateway already produces stable outputs for known keywords). File structure:
  - `promptfooconfig.yaml` at repo root — providers per purpose pointing to `eval/providers/<purpose>.py` thin scripts; tests reference jsonl datasets.
  - `eval/datasets/{draft_golden,extract_golden,summarize_golden,injection_probes}.jsonl` — 30–50 hand-labeled examples per purpose; 20 known injection probes with expected `refused` / canned reply.
  - `eval/providers/{draft,extract,summarize,inject}.py` — load AI service in-process (MOCK), POST row to endpoint, return JSON.
  - `eval/baseline/main.json` — baseline scores from last `main` build, CI artifact.
  - `.github/workflows/prompt-eval.yml` — triggers on `pull_request` with `paths: [ai-python/ai_agent/pipelines/**, eval/**, ai-python/ai_agent/security.py]`. Installs `promptfoo`, runs eval, downloads `main` baseline, compares via `scripts/promptfoo-diff.py`. Fails PR if pass-rate delta < −2 pp OR any injection probe fails. Baseline refreshes on push to `main`.
  - Assertions: `is-valid-json` (all); `not-icontains: <EMAIL_` (draft, no placeholder leak); `output.facts.every(f => valid_fact_type)` (extract); ROUGE-L ≥ 0.5 + `not-icontains` PRIVATE/NO_SUMMARY (summarize); canned-reply equality OR `decision="ignore"`+`reason="blocked_injection"` (injection).
  - Future: optional live-provider eval gated on `eval:live` PR label (`deepseek-chat` at ~$0.05/PR) — needs budget cap + secret.

---

## Memory Architecture Backlog

**✅ ВЫПОЛНЕН (2026-06-11)** — задачи MEM-1…MEM-13 реализованы (миграции 013–016), статус и
сознательно отложенное (export, GRANT-вариант, EXPLAIN-проверка HNSW, батчинг extraction) в
**[memory_architecture_backlog.md](memory_architecture_backlog.md)**. Ключевое: семантический дедуп
теперь использует embedding кандидата и атомарный supersede (миграция 013); deletion contract §10.2 —
`/forget`, `/forget_all` с подтверждениями; suggestions flow §4.5 — extraction → карточки accept/reject;
token budget §8.1 — 4000 токенов с детерминированным урезанием; halfvec-HNSW ANN (миграция 015);
`MEMORY_ARCHITECTURE.md` обновлён до v6.

---

## Memory / RAG Implementation Gap

Items below are in dependency order.

1. ~~**Conflict resolution chat vs global (design §5.6).**~~ Closed by v5: global scope removed (migration 011), межчатовых конфликтов не существует.
2. ~~**Health confirmation flow.**~~ Closed by v5: health facts are chat-scoped, `pending_global_health_facts` dropped (migration 011).
3. **Extraction is fact-starved — частично закрыто.** Пункт (a) выполнен: окно последних 5 сообщений передаётся в `TypedExtractionRequest.recent_messages` (переиспользуется фетч контекста, ARCH-8); короткие фактологически пустые сообщения отсеиваются pre-filter'ом до LLM (`shouldExtractFacts`, MEM-13). Остаётся пункт (b): по `extraction_log` (до/после) перенастроить порог `confidence ≥ 0.8` и transience-фильтр в `extraction.py`, если лог покажет over-filtering. Do NOT change extraction semantics blind — read `extraction_log` first.
4. ~~**Episode `current_topic` always NULL.**~~ ✅ Закрыто ARCH-4: topic приходит в структурированном ответе `/ai/process-message` и пишется в `active_episodes.current_topic` без отдельного LLM-вызова; `/ai/tag-topic` остался для эпизодных воркеров.

### Memory Architecture Status Gaps

| Design Component | Status |
|---|---|
| `pending_global_health_facts` queue | ✅ Obsolete — dropped in v5 (migration 011) |
| §5.6 chat-vs-global conflict resolution | ✅ Obsolete — global scope removed in v5 |
| §9.3 health confirmation flow (accept/reject UI) | ✅ Obsolete — health facts chat-scoped in v5 |
| §13 AI roles (`ai_roles` + role-first prompt) | ✅ Implemented in v5 (migration 011, бот-команды, role-first промпт) |

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

- ~~Mini App UI — web dashboard for draft review, memory viewer, chat management.~~ → ✅ реализовано (2026-06-12/13): экраны чатов (две секции Боты/Личные), ролей, настроек, «Я», подключения бота.
- AI Roles Marketplace — persona presets per chat. ⚠️ Частично: 2 готовых пресета роли (`ROLE_TEMPLATES` в `miniapp/screens/roles.js`) уже в проде; полноценный маркетплейс с шарингом — позже.
- Telegram Client API / MTProto — historical message access for tenants granting full access.
- **(2026-06-13)** Личные (business) чаты в Mini App намеренно заблокированы (`chat-item-locked`, ingestion mode `bot`): приложение их не обрабатывает, фокус на ботах. Разблокировка — переключить вкладку «Личные» из read-only в рабочий режим, когда LS-флоу будет готов.
- **(2026-06-13)** Role context (`ai_roles.context_notes`, migration 101): файлы читаются в браузере и кладутся в текстовое поле context. Возможное улучшение — реальная загрузка бинарных файлов (PDF/docx) с извлечением текста на бэкенде вместо client-side `file.text()`.

---

## LLM Safety & Observability Pipeline

**Open items:**

- `llm-guard==0.3.*` and `langfuse==2.*` still commented out in `requirements.txt` → real ML scanners are not running, only regex fallback.
- `Dockerfile.ai-python` missing prewarm step for llm-guard transformers.
- `docs/litellm-runbook.md` — runbook for adding model aliases, rotating keys, inspecting spend.
- `docs/dev-observability.md` — Phoenix RAG debug workflow.
- `infra/langfuse/alerts.json` — alert webhook config (block_rate / cost / latency / error_rate).
- ~~`ai-python/ai_agent/pipelines/embedding.py` — embed path still hits OpenRouter directly~~ → перенесено в ARCH-14 ([ai_backend_architecture_backlog.md](ai_backend_architecture_backlog.md)).
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
