# DoubleU — Engineering Backlog

> **Аудит 2026-06-22, повторно сверено 2026-06-26** (ветка `feat/data-1-migration-111-deduplicate`).
> Закрытые задачи перенесены в раздел [«Закрыто с прошлого аудита»](#-закрыто-с-прошлого-аудита-сверено-с-кодом-2026-06-22) со ссылкой на код/документ, где они описаны.
> Разрозненные «прогнать shadow-замер» пункты сведены в один трек [Pre-Production Gates](#-pre-production-gates--замеры-перед-боевым-cutover).
> Детальные промпты-задачи по AI/Memory живут в под-беклогах (см. [Reference](#под-беклоги-reference)) — здесь только открытые хвосты.

**Как читать таймлайн:** `🔴 Critical` → делать сейчас (безопасность/целостность данных, блокирует прод).
`🟠 Pre-Production` → обязательно до боевого запуска, но система работает и без этого (cutover + замеры).
`🟡 Non-Critical` → долг/качество/тулинг, по мере ресурса. `🧊 Iteration 2` → заморожено, не начинать без явной команды.

> 📋 **План реализации** Critical + Pre-Production (SEC / DATA / LITELLM / MEASURE) как самодостаточные
> задачи для агента-исполнителя: [`critical_preprod_plan_2026-06-22.md`](plans/critical_preprod_plan_2026-06-22.md).

---

## 🔴 Critical — безопасность, приватность, целостность данных

### SEC — Security & Privacy

- **`user_bots.token` хранится в БД открытым текстом.** В логи не пишется и наружу не отдаётся, но
  это bot-credential в plaintext. Нужно app-level шифрование ключом из env (envelope/AES-GCM) перед
  записью, расшифровка при чтении в поллере. Точка изменения: `backend-go/internal/store/store.go`
  (запись/чтение токена), ключ — из `backend-go/internal/config`.
  _Verified open 2026-06-26: шифрования в `botservice/`/`store.go` нет._

- **LLM Guard сканеры не запущены — только regex-fallback.** `llm-guard==0.3.*` и `langfuse==2.*`
  закомментированы в `ai-python/requirements.txt` (строки 11–12). На untrusted Telegram-вход
  (prompt-injection, PII) работает только regex из `security.py`. Перед продом включить реальные
  ML-сканеры на input **и** output пути.
  - `Dockerfile.ai-python` — добавить prewarm-шаг для llm-guard transformers (input **и** output
    сканеры, одинаковая работа).
  _Verified open 2026-06-26._

---

## 🟠 Pre-Production Gates — замеры перед боевым cutover

> Единый трек: всё это **измерения на реальных данных перед переключением прод-поведения**, а не баги.
> Система функционирует и сейчас; эти шаги подтверждают качество/стоимость до автономного режима.
> Общий метод — shadow-режим + сравнение `extraction_log` / порядка фактов до/после. **Не менять
> семантику вслепую** (чартер): сначала лог, потом решение.

### LITELLM — миграция гейтвея на LiteLLM (Phase 5)

> ⚙️ **В РАБОТЕ (uncommitted, 2026-06-22):** env-driven model config — `scripts/render_litellm_config.py`,
> `.env.example` (тиры `MODEL_TIER_FAST/BALANCED/SMART`, `LLM_GATEWAY=litellm`), `make models-apply`,
> правки `infra/litellm/*`, `docker-compose.yml`, `Makefile`, `ai-python/tests/test_model_config.py`.
> Это инфраструктура под cutover. Закоммитить и довести следующие шаги:

- Per-purpose shadow ≥7 дней, затем `CUTOVER=true`, по порядку: `embed` → `extract` → `summarize`
  → `draft`/`process_message` (последним). Следовать `docs/runbooks/cutover-runbook.md`.
- После cutover всех purpose — удалить `OpenRouterLLMGateway` + dual-credential embed-ветку
  (`ai-python/ai_agent/pipelines/embedding.py` прямой OpenRouter-путь — dev-fallback, см. ARCH-14).

### MEASURE — shadow-замеры retrieval / extraction

- **Rerank: cosine vs LLM.** Shadow-сравнение порядка фактов `RankFactsByCosine` (SQL) vs
  `/ai/rerank-facts` (LLM) на ~50 реальных запросах. После замера — **удалить** `/ai/rerank-facts` +
  `FactReranker` (живут в `aiclient/`, `service.py`, `runtime.py`; из горячего пути LLM-rerank уже
  убран). _Verified open 2026-06-22: эндпоинт и клиент ещё существуют (ARCH-5 хвост)._
- **Extraction tuning (порог / батчинг).** По `extraction_log` (до/после pre-filter `shouldExtractFacts`):
  (a) перенастроить порог `confidence ≥ 0.8` и transience-фильтр в `extraction.py`, **если** лог покажет
  over-filtering; (b) решить про дебаунс-батчинг extraction по эпизодам (MEM-13). Pre-filter коротких
  сообщений уже реализован — здесь только калибровка по данным.
- **Retrieval quality.** Прогнать shadow-harness на фиксированном наборе чатов до/после, подтвердить
  улучшение ретривала и рост выхода фактов от multi-turn окна перед прод-cutover'ом
  (Memory Retrieval Roadmap — код готов, замер открыт).
- **HNSW EXPLAIN.** MEM-7: вручную проверить на реальной БД (pgvector ≥ 0.7), что cosine-запросы идут
  Index Scan по halfvec-HNSW, а не seq scan (в тестовом окружении Postgres с pgvector нет).

---

## 🟡 Non-Critical — тех-долг, качество, тулинг

### DEBT — осознанные отложенные решения

- **Voice role drafting: base64 JSON upload.** Mini App пока буферизует запись `MediaRecorder`
  целиком в памяти и отправляет base64 в JSON (лимит записи 60 секунд, серверный cap 14 MiB).
  При росте лимитов заменить на streaming/multipart upload и добавить codec fallback для WebView
  без `audio/webm`; текущий вариант достаточен для короткого голосового описания роли.
- **Long polling → webhook per bot.** `getUpdates` — решение для MVP/отладки. Транспорт
  инкапсулирован в `internal/botservice/poller.go` (manager поверх `UpdateSource`), точка замены одна,
  core не трогается. Для прода заменить на webhook.
- **ai-python HTTP-сервер на stdlib (`http_server.py`).** Миграция на FastAPI/uvicorn отложена
  осознанно: контракт и context-propagation покрыты тестами. Переходить вместе с переводом
  `schemas.py` (frozen dataclasses) → pydantic BaseModel — оба шага меняют сериализацию, делать под
  защитой `test_contracts.py`.
- **MEM-11 GRANT-защита.** Прямой INSERT в `semantic_facts` из DB-пути убран (только процедура +
  in-memory fallback). Полный вариант — выделенная DB-роль `app_role` + `REVOKE INSERT / GRANT EXECUTE`
  — отложен до управления пользователями БД в `infra/`.
- **MEM-2 `export`.** JSONL-дамп tenant'а (`/forget`, `/forget_all` уже реализованы) — отдельная
  невыполненная подзадача deletion-контракта.
- **Critic-агент получает untrusted-текст без `PromptBuilder`-обёртки.** `AgentRuntime._review_draft`
  ([runtime.py](../ai-python/ai_agent/agents/runtime.py)) собирает `review_payload` прямой
  f-строкой из `message_text` (сырое сообщение собеседника) и `drafted.reply`, минуя
  `PromptBuilder.build` — единственное место в hot path, где untrusted-вход не попадает в
  `<untrusted_user_message>`. На практике риск ниже, чем у draft/orchestrator (критик не имеет
  тулов и не может действовать на инъекцию), но потенциально проинструктированный текст в
  `message_text` может склонить вердикт к `approve`. Обёрнуть `message_text`/`drafted.reply` через
  `prompt_builder.build(...)` так же, как уже делает draft-путь.
  _Найдено 2026-06-29 при разработке Prompt Contract (фаза 1); сознательно не исправлено — вне
  объёма задачи._
- **`model_used`-лейбл для two-stage writer показывает голый `model_tier`, а не конкретный slug.**
  В `AgentRuntime._draft_async` ([runtime.py](../ai-python/ai_agent/agents/runtime.py)) `tier_model_label`
  заполняется только внутри ветки `if role_model_name:` (явный `ai_role.model_name`); когда роль
  задаёт только `model_tier` (например `"smart"`) без конкретной модели, two-stage лейбл деградирует
  до `f"orchestrate({router})+draft(smart)"` вместо реального первого slug из `MODEL_TIER_SMART`. Не
  влияет на то, какая модель реально вызывается (resolution корректен), только на читаемость
  Langfuse/логов. Чинить через `model_name_for_tier(tier, model_name)` и для случая без
  `model_name`.
  _Найдено 2026-06-29 при разработке Prompt Contract (фаза 1); сознательно не исправлено — вне
  объёма задачи._

### TOOLS — integration-инструмент не реализован

- **Commit 5 спины Tools-платформы не сделан.** План
  ([`feature_ideas_implementation_prompts_2026-06-22.md`](plans/feature_ideas_implementation_prompts_2026-06-22.md),
  §4) описывает `integration`-категорию инструментов — исходящие вебхуки (`http.request` / MCP),
  allow-list + защита от SSRF — помеченную в плане как «второстепенно». 5 из 6 коммитов спины уже
  реализованы (см. [Закрыто](#tools-платформа--двумодальное-исполнение-план-feature_ideas_implementation_prompts_2026-06-22md));
  в [`TOOL_CATALOG`](../ai-python/ai_agent/agents/tool_catalog.py) нет записей с
  `category=ToolCategory.INTEGRATION`, исходящих HTTP-вызовов как инструмента в кодовой базе не найдено.
  При реализации не забыть allow-list + SSRF-защиту — это явное требование самого плана, а не
  отдельное допущение.
  _Verified absent 2026-06-26._

### QA — eval / тестовый тулинг

- **Promptfoo CI eval harness** (offline-only, `MOCK_LLM=true`, не трогает рантайм). Проектное описание
  ниже — **не реализовано** (нет `promptfooconfig.yaml`, `eval/`, `.github/workflows/prompt-eval.yml`).
  _Verified absent 2026-06-26._
  - `promptfooconfig.yaml` (root): провайдеры per purpose → `eval/providers/<purpose>.py`; тесты на jsonl.
  - `eval/datasets/{draft,extract,summarize,injection_probes}_golden.jsonl` — 30–50 примеров/purpose;
    20 injection-проб с ожидаемым `refused`/canned-reply.
  - `eval/baseline/main.json` — baseline с последнего `main` (CI-артефакт).
  - workflow на `pull_request` (paths: `ai-python/.../pipelines/**`, `eval/**`, `security.py`); fail при
    pass-rate delta < −2 pp ИЛИ любой проваленной injection-пробе. Diff через `scripts/promptfoo-diff.py`.
  - Future: live-provider eval за лейблом `eval:live` (нужен budget cap + secret).

### DOCS — недостающие runbook'и / конфиги

- `docs/runbooks/litellm-runbook.md` — добавление model-алиасов, ротация ключей, инспекция spend. _(нет)_
- `docs/runbooks/dev-observability.md` — Phoenix RAG debug workflow. _(нет)_
- `infra/langfuse/alerts.json` — alert webhook (block_rate / cost / latency / error_rate). _(нет)_

### HYGIENE — репо

- _(пусто — `prototypeui/` уже удалён, проверено 2026-06-22.)_

---

## 🧊 Iteration 2 / Future — заморожено (не начинать без явной команды)

### Продукт

- **AI Roles Marketplace.** Частично: 2 пресета ролей (`ROLE_TEMPLATES`, `miniapp/screens/roles.js`)
  в проде; полноценный маркетплейс с шарингом — позже.
- **Telegram Client API / MTProto** — доступ к историческим сообщениям для tenant'ов с полным доступом.
- **Разблокировка «Личных» (business) чатов в Mini App.** Сейчас намеренно read-only
  (`chat-item-locked`, ingestion mode `bot`) — фокус на ботах. Разблокировать, когда LS-флоу готов.
- **Реальная загрузка бинарных файлов в role context** (`ai_roles.context_notes`, migration 101):
  сейчас браузер читает текст client-side (`file.text()`); улучшение — серверное извлечение текста из
  PDF/docx.

### Безопасность / QA (отдельный трек)

- Adversarial red-teaming harness.
- Fine-tuning кастомного moderation-классификатора на owner-labelled черновиках.
- DLP сверх LLM Guard `Anonymize`/`Sensitive` (domain-tuned ML DLP).
- Per-tenant LiteLLM virtual keys (жёсткая изоляция). Текущий план — один master key + headers для
  tenant-атрибуции; переход добавляет в Phase 2 хук провижининга tenant'а в Go.

### Из исходного чартера (deferred)

- Mini App UI — ✅ реализовано (см. closed-секцию); остальное по чартеру:
- AI Roles Marketplace (см. выше), MTProto (см. выше).

---

## ✅ Закрыто с прошлого аудита (сверено с кодом, 2026-06-22)

> Удалено из активного беклога. Каждый пункт задокументирован в коде или плановом документе.

### DATA — устранена коллизия миграций 111

- **Коллизия номеров миграций 111 устранена.** Два конфликтующих файла
  (`111_ai_role_speak_as_owner.sql`, `111_role_model_tier.sql`) слиты в один
  [`111_ai_role_columns.sql`](../infra/migrations/111_ai_role_columns.sql) (+`.down.sql`) — оба
  `ADD COLUMN IF NOT EXISTS` (`speak_as_owner`, `model_tier`) теперь в одной идемпотентной миграции,
  версия снова уникальный ключ. Последовательность продолжена линейно: 112 (quarantine) → 113
  (`ai_role_tool_settings`) → 114 (`scheduled_jobs`) → 115 (`ai_role_model_name`).
  _Verified 2026-06-26: в рабочей копии на диске, не закоммичено — закоммитить вместе с остальными
  изменениями ветки `feat/data-1-migration-111-deduplicate`._

### Tools-платформа + двумодальное исполнение (план: [`feature_ideas_implementation_prompts_2026-06-22.md`](plans/feature_ideas_implementation_prompts_2026-06-22.md))

> 6-commit spine: 5 из 6 реализованы, см. таксономию категорий read/schedule/integration/interaction в плане.
> Хвост (Commit 5, integration-инструмент) — открытый пункт, см. [TOOLS](#tools--integration-инструмент-не-реализован) ниже.

- **Commit 1 — Tools-платформа.** Единый каталог `TOOL_CATALOG`
  ([tool_catalog.py](../ai-python/ai_agent/agents/tool_catalog.py)) с категориями
  read/schedule/integration/interaction, per-role `tool_settings` (jsonb, migration 113), Go-зеркало +
  контракт-тест ключей (`tool_catalog_test.go`), отдельный экран `/roles/:id/tools`
  (`miniapp/screens/role-tools.js`). _(commit `929424e`)_
- **Commit 2 — Двумодальное исполнение.** Оркестратор (дешёвая модель-константа `MODEL_ROUTER`,
  выполняет инструменты, не пишет ответ) + писатель (per-role `model_tier`, пишет финал из
  собранного контекста) в [runtime.py](../ai-python/ai_agent/agents/runtime.py); без включённых
  инструментов — старый одновызовный путь без изменений. _(commit `7364b14`)_
- **Commit 3 — interaction-инструмент.** `present_buttons`/`request_confirmation`
  ([interaction_tools.py](../ai-python/ai_agent/agents/interaction_tools.py)) — кнопки/вопрос
  собеседнику, только Transport==bot, идёт обычным путём decision→approval→send. _(commit `cfc1c7d`)_
- **Commit 4 — scheduler-инструмент.** `create_reminder`
  ([schedule_tools.py](../ai-python/ai_agent/agents/schedule_tools.py)) + job worker, таблица
  `scheduled_jobs` (migration 114); напоминания видимы и отменяемы владельцем. _(commit `7ec43b1`)_
- **Commit 6 — голосовая роль.** `POST /ai/draft-role` — голос/текст → черновик роли (name,
  description, speaking_style, goals, constraints, custom_fields), независимо от остальной спины.
  _(commit `902c287`)_

### Rich formatting (план: `docs/plans/rich_models_streaming_plan_2026-06-20.md`)

- **Phase A — деградация markdown, а не сырой дамп.** `sendRecipient`/`sendRichRecipient`
  ([service.go:2905](../backend-go/internal/core/service.go), [:3128](../backend-go/internal/core/service.go))
  деградируют markdown → Telegram HTML (`telegram.MarkdownToHTML`, `internal/telegram/mdrender.go`),
  при отказе HTML — `StripMarkdown` в plain (`ParseMode=""`). Сырой markdown (`###`, `**`) не уходит
  никогда. _(было: Bug «rich formatting fallback, ПЕРЕОТКРЫТО 2026-06-20»; commit `6ecb39d`)_
- **Phase B — нормализация заголовков.** `normalize_rich_markdown`
  ([draft.py:305](../ai-python/ai_agent/pipelines/draft.py)) чинит `### `-в-одну-строку; промпт
  форматирования ужесточён. _(было: Bug «rich formatting `#`/`###`»; commit `4b51370`)_
- **Phase C/D** — per-role `model_tier` (migration `111_role_model_tier`) + live typing-indicator и
  paced rich-frames (commits `3137439`, `79c1299`).
- **DB:** `drafts.draft_type` CHECK расширен значением `warning` (migration 109); seed переписан под
  consolidated schema (`ai_roles`/`semantic_facts`/`episode_summaries`, commit `620fa31`);
  `fact_embeddings` upsert-conflict исправлен (migration 110).

### Hot-path эффективность

- **UserSettings lookup fold.** `deliverySettingsFromUser` принимает предзагруженный `UserSettings`,
  горячий путь не делает второй repository-read
  ([service.go:2937](../backend-go/internal/core/service.go)). _(было: Inefficiency «richFormattingEnabled
  separate lookup»; commit `98332d0`)_
- `ContextScore` TTL-кэш (ARCH-8), `RecentMessages` per-chat индекс (ARCH-13),
  `UpsertSemanticFact` без unbounded in-memory роста (ARCH-13).

### Spam protection & persona-split (план: `docs/plans/spam_protection_plan.md`)

- **② Episode-write flood gate** (`minEpisodeSpan`, commit `c6b4877`), **① message quarantine +
  exclusion** (`messages.quarantined`, migration 112, commit `ec91816`), **③ per chat+sender rate-limit**
  (`chatLimiters`, commit `22ac0f5`), **persona-split** — bot-mode запросы без owner identity/feedback
  (commit `22f3813`).

### AI / Memory архитектура

- **ARCH-1…15** — реализованы (горячий путь = `embed + 1 структурированный LLM-вызов`; critical-warning
  отделён от черновика; webhook = шардированная очередь + быстрый ACK; контекст параллельно, бюджет 4000
  токенов). Хвосты → [Pre-Production Gates](#-pre-production-gates--замеры-перед-боевым-cutover).
- **MEM-1…13** — реализованы (миграции 013–016; семантический дедуп по embedding кандидата + атомарный
  supersede; `/forget`/`/forget_all`; suggestions accept/reject; token budget; halfvec-HNSW). Подробности —
  `docs/architecture/memory_architecture_backlog.md`, `docs/architecture/MEMORY_ARCHITECTURE.md` v6.
- **search_facts embedder** — `_build_query_embedder` ([runtime.py:101](../ai-python/ai_agent/agents/runtime.py))
  настроен; `deps.embedder` используется в `memory_tools.py`, ILIKE остаётся как fallback. _(было: Tech Debt
  «runtime embedder не настраивает»; ARCH phase 0+1)_
- **extraction_log.facts_created** — Go пишет `FactsCreated: outcome.CreatedIDs`
  ([service.go:1434](../backend-go/internal/core/service.go), [:2078](../backend-go/internal/core/service.go)).
  _(было: Tech Debt «Go пишет пустые facts_created»)_
- **`/ai/generate-draft`** помечен deprecated в openapi (regenerate идёт через process-message, ARCH-14).
- **upsert_semantic_fact** dead `RAISE EXCEPTION` ветка убрана (migration 013, MEM-1).
- **`current_topic`** приходит в структурированном ответе process-message (ARCH-4), отдельного
  LLM-вызова в горячем пути нет.
- **Memory/RAG gap §5.6 / health-confirmation** — закрыты v5 (global scope удалён, migration 011).

### Mini App (Iteration 2, реализовано 2026-06-12/13)

- Экраны чатов (Боты/Личные), ролей, настроек, «Я», подключения бота; REST CRUD `/api/v1/roles`
  (`miniapp/screens/role-edit.js`); гайдед-форма роли вместо `|`-one-liner. SSE realtime (не WebSocket).

### Media (план: `docs/plans/media_files_plan.md`, commits `82da5ce`/`29414f5`/`0c7aebe`)

- Агент понимает voice/image/doc-вход через media→text: `/ai/extract-media` (Python), Telegram
  file-download + media-дескрипторы и Go-wiring с лимитами и toggle (`media_enrich_test.go`).

---

## Под-беклоги (Reference)

- [`ai_backend_architecture_backlog.md`](architecture/ai_backend_architecture_backlog.md) — ARCH-1…15 (детальные
  промпты-задачи, все реализованы; хвосты сведены сюда в Pre-Production Gates).
- [`memory_architecture_backlog.md`](architecture/memory_architecture_backlog.md) — MEM-1…13 (детальные
  промпты-задачи, все реализованы; отложенное: export, GRANT-роль, HNSW EXPLAIN).
- [`MEMORY_ARCHITECTURE.md`](architecture/MEMORY_ARCHITECTURE.md) — архитектура памяти v6 (контракт).
- [`cutover-runbook.md`](runbooks/cutover-runbook.md) — процедура LiteLLM per-purpose cutover.
- [`feature_ideas_implementation_prompts_2026-06-22.md`](plans/feature_ideas_implementation_prompts_2026-06-22.md) —
  Tools-платформа + двумодальное исполнение, 6-commit spine (детальные промпты; 5/6 реализованы, см. Закрыто и TOOLS).
