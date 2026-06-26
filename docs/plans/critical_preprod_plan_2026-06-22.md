# Critical + Pre-Production — Implementation Plan

**Audience:** implementer agent (Sonnet 4.6). **Status:** ready to build. **Created:** 2026-06-22.
**Scope:** backend-go, ai-python, infra/migrations, infra/litellm. Each task is its own PR, independently shippable.

> **Read first (binding):** `CLAUDE.md` (repo root) — frozen `/contracts/`, privacy flags, slog-only,
> errors handled-or-`_`-with-comment, functional tests against real in-memory store (`r.db == nil`).
> This plan was written **after** a code audit (2026-06-22); the anchors below are verified against the
> current tree, **not** the older `backlog.md`/`cutover-runbook.md` prose (both were partly stale —
> see the per-task "Reality check" notes).

## Order (independent PRs; recommended sequence)

```
DATA-1  (migration 111 collision — cheapest, unblocks clean bootstrap)  — ✅ DONE 2026-06-26 (uncommitted)
SEC-1   (bot token encryption at rest — security)
SEC-2   (LLM Guard real scanners — security; heavier: deps + Docker prewarm)
OPS-1   (LiteLLM cutover — commit in-flight config, guarded per-purpose flip)
MEASURE-1 (shadow measurements — gates legacy deletion; needs prod data)
```

Finish every Go task with `cd backend-go && go build ./... && go test ./...` green; every Python task
with `cd ai-python && python3 -m pytest tests/ -q` green. Migration tasks also run `make migrate-up`
then `make migrate-down` on a real DB.

---

## 🔴 DATA-1 — Свести коллизию номеров миграций 111 — ✅ DONE (uncommitted, 2026-06-26)

> Реализовано в рабочей копии на ветке `feat/data-1-migration-111-deduplicate`: `111_ai_role_columns.sql`
> (+`.down.sql`) объединяет обе колонки, четыре старых файла удалены, последовательность 112→115 линейна,
> `TestMigration111MergedSequenceIsLinear` (`migrations_test.go`) покрывает регрессию. Осталось только
> закоммитить и добавить reconcile-SQL в описание PR (см. ниже) — код можно не трогать.

**Problem.** Два файла делят версию `111` и оба меняют `ai_roles`:
`infra/migrations/111_ai_role_speak_as_owner.sql` (`ADD COLUMN speak_as_owner boolean`) и
`111_role_model_tier.sql` (`ADD COLUMN model_tier text`). Раннер
([`backend-go/internal/store/migrations.go`](../../backend-go/internal/store/migrations.go)) берёт версию
из числового префикса (`migrationVersionRe`, :45), трекинг — по **имени файла**
(`schema_migrations.filename PRIMARY KEY`, :106), сортировка по версии нестабильна (`sort.Slice`, :97),
а `MigrateDown(targetVersion)` (:288) откатывает по версии. Итог: точечный откат одной из двух 111
невозможен (откат к 110 сносит обе), порядок между ними недетерминирован, последовательность нелинейна.

**Reality check.** Up-путь **не ломается** сегодня: имена разные → дедуп (:91) обе пропускает, обе
применяются; `ADD COLUMN IF NOT EXISTS` на разных колонках идемпотентен. Это латентный долг, фиксим
**до следующего bootstrap чистой БД**.

**Change.** Слить обе в один файл, сохранив номер 111 (он уже «занят» в проде по обоим именам):
- Новый `infra/migrations/111_ai_role_columns.sql` — оба `ALTER TABLE public.ai_roles ADD COLUMN IF NOT
  EXISTS …` (speak_as_owner + model_tier с CHECK), плюс `COMMENT ON COLUMN`.
- Новый `infra/migrations/111_ai_role_columns.down.sql` — `DROP COLUMN IF EXISTS` обе.
- Удалить четыре старых файла: `111_ai_role_speak_as_owner.sql(.down)`, `111_role_model_tier.sql(.down)`.

**Prod reconciliation (обязательно задокументировать в PR).** Трекинг по имени файла → на уже-применённой
проде новый `111_ai_role_columns.sql` раннер сочтёт неприменённым и прогонит повторно (идемпотентно,
безвредно), оставив в `schema_migrations` две устаревшие строки (`111_ai_role_speak_as_owner.sql`,
`111_role_model_tier.sql`). В PR-описании дать ручной reconcile-SQL для прод-оператора:
`DELETE FROM schema_migrations WHERE filename IN ('111_ai_role_speak_as_owner.sql','111_role_model_tier.sql');`
(выполнять только после успешного применения нового файла). На свежей БД — чисто, reconcile не нужен.

**Tests** (`backend-go/internal/store/migrations_test.go`).
- Свежий `applyMigrations` со всеми путями → `ai_roles` имеет обе колонки `speak_as_owner` + `model_tier`.
- `MigrationStatuses` не содержит двух строк версии 111 (последовательность линейна).
- Down-файл удаляет обе колонки.

**Acceptance.** Линейная последовательность версий восстановлена; обе колонки создаются одним файлом;
build/tests зелёные; `make migrate-up && make migrate-down` чисто на реальной БД; reconcile-SQL для прода
в описании PR.

**Do not.** Не трогать миграцию 112 (quarantine) и выше. Не менять семантику/дефолты колонок
(`speak_as_owner DEFAULT true`, `model_tier DEFAULT 'balanced' CHECK (fast/balanced/smart)`).

---

## 🔴 SEC-1 — Шифрование `user_bots.token` в покое (at rest)

**Problem.** Bot-токен (credential) хранится в БД открытым текстом. В лог не пишется и наружу не
отдаётся (инвариант в [`user_bots.go:3,22`](../../backend-go/internal/store/user_bots.go)), но любой доступ
к БД/бэкапу = утечка токенов всех ботов всех tenant'ов.

**Files / anchors.**
- Struct: `UserBotRecord.Token` ([`user_bots.go:22`](../../backend-go/internal/store/user_bots.go)).
- Write (DB): INSERT в `SaveUserBot`, `record.Token` = `$4` ([`user_bots.go:71-86`](../../backend-go/internal/store/user_bots.go)).
- Read (DB): `scanUserBot` `&rec.Token` ([`user_bots.go:140-141`](../../backend-go/internal/store/user_bots.go)),
  колонки `userBotColumns` (:152), SELECT в `UserBotsByStatus`/`UserBotByID` (:203, :154+).
- Consumer: поллер берёт `p.bot.Token` напрямую ([`botservice/poller.go:50`](../../backend-go/internal/botservice/poller.go)),
  sender — `telegram.NewBotSender(bot.Token, …)` ([`botservice/manager.go:218`](../../backend-go/internal/botservice/manager.go)).
  ⇒ дешифровка должна быть **прозрачной в store-слое**: всё выше store видит plaintext.
- Config: добавить ключ в [`config.go:55-67`](../../backend-go/internal/config/config.go).

**Change.**
1. Новый `backend-go/internal/store/secrets.go`: `tokenCipher` на AES-256-GCM. Ключ — 32 байта из env
   `BOT_TOKEN_ENCRYPTION_KEY` (base64/hex; провалидировать длину при старте). `Encrypt(plain) -> string`
   (nonce‖ciphertext, base64, с 1-байтовым version-префиксом `v1`), `Decrypt(stored) -> (string, error)`.
2. `Repository` получает `*tokenCipher` через конструктор (как прочие зависимости; не глобал). Когда ключ
   не задан — **fail-closed на DB-пути**: `SaveUserBot`/чтение логируют и отказывают (или, если нужен
   dev-режим без ключа, явный `BOT_TOKEN_ENCRYPTION_OPTIONAL=true` с громким `slog.Warn`). Реши и
   задокументируй один вариант.
3. Запись: шифровать `record.Token` перед INSERT-параметром (in-memory путь `r.userBots[key]` хранит
   plaintext — он не персистентен, оставить как есть).
4. Чтение: дешифровать в `scanUserBot` сразу после `Scan`. **Backward-compat:** старые plaintext-строки
   не имеют version-префикса → `Decrypt` детектит отсутствие `v1`-маркера и возвращает строку как есть
   (lazy-upgrade: следующий `SaveUserBot` перезапишет зашифрованной). Задокументировать как осознанный
   transitional path; добавить TODO на удаление ветки после backfill.
5. Никогда не логировать ни plaintext, ни ciphertext (инвариант :22 сохранить; `redactToken` уже есть).

**Tests** (`backend-go/internal/store/*_test.go`, real-DB integration где есть).
- Round-trip: `SaveUserBot(token)` → строка в `user_bots.token` ≠ plaintext (начинается с `v1`-маркера);
  `UserBotByID`/`UserBotsByStatus` возвращают исходный plaintext.
- Legacy-строка без префикса читается как есть (back-compat), после `SaveUserBot` становится зашифрованной.
- Поллер-тест ([`poller_test.go:119`](../../backend-go/internal/botservice/poller.go)) остаётся зелёным —
  токен доходит до `getUpdates` расшифрованным.
- Отсутствие ключа на DB-пути → fail-closed (по выбранному в п.2 контракту).

**Acceptance.** В БД токен зашифрован; весь код выше store видит plaintext без изменений; ключ из env;
legacy-строки не ломаются; токен не утекает в логи; build/tests зелёные.

**Do not.** Не добавлять токен/ciphertext в `/contracts/` типы. Не хранить ключ в репозитории/Compose
по умолчанию (только env). Не менять in-memory fallback на шифрование (он эфемерный).

---

## 🔴 SEC-2 — Включить реальные LLM Guard сканеры (input + output)

**Reality check (важно — backlog устарел).** Это **не** «раскомментировать llm-guard». Полноценный
pluggable-конвейер сканеров уже есть и подключён в рантайм:
- `safety/input.py` `InputSafetyScanner.scan()` — гейтится `flags.safety_input_enabled`, набор сканеров
  per purpose из `policies.py`, сейчас вызывает `regex_fallback as rx` (`rx.scan_all`) напрямую
  ([`safety/input.py:95`](../../ai-python/ai_agent/safety/input.py)).
- `safety/output.py` `OutputSafetyScanner` + `safety/deanonymize.py` — вызывается в рантайме
  (`_scan_output`, [`agents/runtime.py:554`](../../ai-python/ai_agent/agents/runtime.py)).
- Наборы: `INPUT_SCANNERS_BY_PURPOSE` / `OUTPUT_SCANNERS_BY_PURPOSE`
  ([`safety/policies.py:15,25`](../../ai-python/ai_agent/safety/policies.py)).
- `safety/results.py` `SafetyBlocked`/`ScanResult`; `safety/regex_fallback.py` — детерминированный fallback.
- Флаги: `SAFETY_INPUT_ENABLED`/`SAFETY_OUTPUT_ENABLED` (default off,
  [`config/flags.py:29-30`](../../ai-python/ai_agent/config/flags.py)).

То есть **сейчас даже при включённых флагах работает только regex**, а реальные ML-сканеры LLM Guard не
подключены. Задача — подключить их за тем же seam'ом, оставив regex страховочной сеткой.

**Change.**
1. `ai-python/requirements.txt`: раскомментировать `llm-guard==0.3.*` (:11). (`langfuse==2.*` (:12) —
   отдельный observability-трек, **не** трогать здесь.)
2. Ввести реальные сканеры за seam'ом. В `InputSafetyScanner.__init__` (и `OutputSafetyScanner`)
   опционально строить LLM Guard сканеры (`llm_guard.input_scanners` / `output_scanners`), когда пакет
   импортируется **и** не MOCK/CI. `scan()` рефакторить так: прогон реальных сканеров → при их отсутствии
   или превышении `latency_budget_ms` (50 ms, уже есть) → `rx.scan_all` как сеть. Маппинг имён из
   `policies.py` (`Anonymize/Secrets/PromptInjection/Toxicity/BanTopics` ↔ output `Toxicity/Sensitive/
   Deanonymize/JSON`) на конкретные классы LLM Guard. Каждый проход пишет `ScanResult` (как сейчас), чтобы
   observability видел, реальный сканер или fallback сработал (`details={"engine": "llm_guard"|"regex"}`).
3. `Dockerfile.ai-python`: добавить prewarm-шаг — при сборке скачать трансформеры LLM Guard для input
   **и** output сканеров (`python -c "import llm_guard …"` warm-up), чтобы рантайм не тянул модели на
   холодную. Сейчас в Dockerfile только stdlib fallback-сервер ([`Dockerfile.ai-python:6-49`](../../Dockerfile.ai-python)).
4. Гарантии: MOCK (`MOCK_LLM=true`) и CI всегда используют regex (детерминизм, без скачивания моделей) —
   это уже задокументировано в `safety/input.py:17-20`, не сломать. Канонический `CANNED_INJECTION_REPLY`
   и `SafetyBlocked`-поведение сохранить.
5. Включение в проде — только env (`SAFETY_INPUT_ENABLED=true`, `SAFETY_OUTPUT_ENABLED=true`); код не
   меняет дефолты (safe-off).

**Tests** (`ai-python/tests/test_safety_input.py`, `test_safety_output.py`).
- С установленным llm-guard: реальный сканер ловит кейс, который regex пропускает (например, перефраз
  инъекции) → `SafetyBlocked`; `ScanResult.details["engine"]=="llm_guard"`.
- Без llm-guard (или MOCK/CI): regex-сеть по-прежнему блокирует известные пробы; `engine=="regex"`.
- `latency_budget` превышен → fallback на regex, `ScanResult(reason="latency_budget")`.
- Embed-путь (`scan_embed_batch`) не запускает PromptInjection (инвариант policies сохранён).

**Acceptance.** При включённых флагах в проде реальные LLM Guard сканеры работают на input и output;
regex остаётся сетью; MOCK/CI детерминированы; Dockerfile прогревает модели; build/tests зелёные.

**Do not.** Не менять `/contracts/`. Не делать сканеры блокирующими для `embed` сверх текущего набора
(коррупция RAG). Не включать флаги в коде/Compose по умолчанию.

---

## 🟠 OPS-1 — LiteLLM cutover (закоммитить конфиг + управляемый per-purpose flip)

**Reality check (важно).** Маршрутизация через LiteLLM **уже реализована**: `agents/models.py`
`build_model`/`build_tier_model` при `LLM_GATEWAY=litellm` строят OpenAI-совместимую модель на
`LITELLM_BASE_URL`+`LITELLM_MASTER_KEY` ([`agents/models.py:90-160`](../../ai-python/ai_agent/agents/models.py)).
**Синхронного dual-run shadow из `cutover-runbook.md` в коде НЕТ:** `build_model` выбирает один гейтвей;
Go-флаги `ShadowModeOnly`/`Cutover` только **логируются** ([`main.go:46-49`](../../backend-go/cmd/backend/main.go),
не потребляются для поведения); в `config/flags.py` полей shadow/cutover нет вовсе. Раннбук в этой части
аспирационный.

**Decision (зафиксировано этим планом).** **Не строить** синхронный dual-run (удваивает стоимость и
латентность горячего пути). Делаем **управляемый прямой cutover** с offline-сравнением и killswitch'ем:

**Change.**
1. **Закоммитить in-flight конфиг** (сейчас uncommitted): `scripts/render_litellm_config.py`,
   `.env.example`, `infra/litellm/*`, `docker-compose.yml`, `Makefile` (`models-apply`),
   `ai-python/tests/test_model_config.py`. Убедиться, что `make models-apply` рендерит валидный
   `model_list`, содержащий каждый env-slug (`MODEL_TIER_*`, `MODEL_DRAFT/CRITIC/CHEAP`, `OPENROUTER_MODEL`).
2. **Привести `cutover-runbook.md` к реальности:** заменить таблицу dual-run flag-семантики на
   фактический путь — `LLM_GATEWAY=litellm` включает LiteLLM целиком; `LLM_GATEWAY=none` (killswitch)
   мгновенно возвращает прямой OpenRouter. Per-purpose «shadow» выполняется **offline** (replay фикс-набора
   через обе ветки + сравнение по порогам divergence из раннбука), а не в проде синхронно.
3. **Offline-сравнение:** небольшой harness `scripts/cutover_compare.py` (или расширить
   `ai-python/evals/`): прогон фикс-набора входов через `LLM_GATEWAY=litellm` и `=none`, метрики divergence
   per purpose (draft: edit-similarity; extract: Jaccard по `fact_text`; summarize: ROUGE-L; embed:
   identity) против порогов раннбука. Запускается вручную/в CI, не в горячем пути.
4. **Промоут по порядку** `embed → extract → summarize → draft` (раннбук): для каждого — прогнать harness,
   уложиться в порог, переключить staging на LiteLLM, проверить `/health/liveliness` 200 и реальный
   `/ai/process-message` ответ schema-valid.
5. **После всех purpose:** удалить мёртвый dual-credential embed-путь прямого OpenRouter в
   [`pipelines/embedding.py`](../../ai-python/ai_agent/pipelines/embedding.py) (ARCH-14 хвост) и
   неиспользуемые Go-флаги `ShadowModeOnly`/`Cutover`, если они остаются только логами (или
   задокументировать их как наблюдаемость).

**Tests.**
- `test_model_config.py` (есть): env→`model_list` рендер корректен; добавить кейс «каждый slug из
  `.env.example` присутствует в рендере».
- `build_model`/`build_tier_model` под `LLM_GATEWAY=litellm` дают модель с `openrouter/`-префиксом для
  bare-slug и без префикса для именованных алиасов ([`models.py:158`](../../ai-python/ai_agent/agents/models.py)) —
  покрыть юнит-тестом (без сети).
- Killswitch: `LLM_GATEWAY=none` строит OpenRouter-модель (или MOCK в CI).

**Acceptance.** Конфиг закоммичен и `make models-apply` валиден; раннбук соответствует коду; offline-harness
считает divergence per purpose; staging обслуживается через LiteLLM с рабочим killswitch'ем; легаси
embed-путь удалён после промоута.

**Do not.** Не строить синхронный dual-run в горячем пути. Не менять `/contracts/`. Не коммитить реальные
ключи (`OPENROUTER_API_KEY`, `LITELLM_MASTER_KEY`) — только в `.env` (gitignored), в `.env.example` плейсхолдеры.

---

## 🟠 MEASURE-1 — Shadow-замеры перед боевым cutover (гейт удаления легаси)

**Nature.** Это не фичи, а **измерения на реальных данных**; часть требует прод-`extraction_log` и реальной
БД с pgvector. Агент строит harness'ы и фиксирует решения; финальные «удаления» — отдельными мелкими PR
после данных. Чартер: **не менять семантику экстракции/ретривала вслепую** — сначала лог, потом решение.

**Sub-tasks.**
1. **Rerank: cosine vs LLM.** Уже: `finalizeFactOrder` ([`service.go:1787`](../../backend-go/internal/core/service.go))
   зовёт `RankFactsByCosine` (SQL-косинус, [`store.go:3177`](../../backend-go/internal/store/store.go)); легаси
   `/ai/rerank-facts` ([`aiclient/http.go:151`](../../backend-go/internal/aiclient/http.go)) ещё вызываем.
   Harness: shadow-сравнение порядка top-8 фактов cosine vs LLM на ~50 реальных запросах. Если cosine не
   хуже → удалить `/ai/rerank-facts`, `RerankFacts` (client) и `FactReranker` (Python) отдельным PR.
2. **Extraction tuning.** Запрос по `extraction_log` (`input_message_ids text[]`, `facts_created bigint[]`,
   миграция 100): доля вызовов с пустым `facts_created` и средняя длина входа. Если пустых >80% → включить
   debounce-batching по эпизодам (MEM-13); порог `confidence ≥ 0.8`/transience-фильтр в `extraction.py`
   менять **только если** лог показал over-filtering.
3. **Retrieval quality.** Replay фикс-набора чатов до/после multi-turn окна; подтвердить рост выхода фактов
   и качества ретривала.
4. **HNSW EXPLAIN** (MEM-7). На реальной БД (pgvector ≥ 0.7) `EXPLAIN` для `SemanticFactsByVector` /
   `EpisodeSummariesByVector`: подтвердить Index Scan по halfvec-HNSW, а не seq scan.

**Deliverable.** `scripts/` harness(ы) + короткий findings-документ `docs/measure_findings_2026-06-xx.md`
с числами и решениями (что удаляем/тюним). Удаления легаси — отдельные PR, гейтятся этими числами.

**Acceptance.** Каждое решение (удалить rerank / включить батчинг / оставить как есть) подтверждено числами
из лога/replay, не «на глаз»; harness воспроизводим; findings-документ закоммичен.

**Do not.** Не удалять `/ai/rerank-facts` и не менять пороги extraction до получения чисел. Не менять
embedding-модель/вводить Qdrant (отдельное решение).

---

## Verify (общее)

```
make test-go          # backend-go: go build ./... && go test ./...
make test             # go + python
make migrate-up       # DATA-1: 111 применяется линейно; make migrate-down откатывает
make models-apply     # OPS-1: LiteLLM рендер + перезапуск; /health/liveliness 200
```

Это бэкенд/инфра-изменения — браузерного preview нет. Верификация = функциональные тесты выше + (для
миграций) реальная БД, (для LiteLLM) живой `/health` и schema-valid `/ai/process-message`.
