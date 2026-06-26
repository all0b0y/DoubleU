# Memory Architecture Backlog

> **СТАТУС (2026-06-11): задачи MEM-1…MEM-13 реализованы** (миграции 013–016,
> оба набора тестов зелёные). Сознательно отложенное:
> - MEM-2: `export` (JSONL-дамп tenant'а) — отдельная задача, не реализован.
> - MEM-11: выбран вариант 1 — DB-путь записи фактов только через процедуру,
>   прямой INSERT из `SaveSemanticFact` удалён; GRANT/REVOKE — после появления
>   выделенной DB-роли (вариант 2).
> - MEM-13: реализован pre-filter (`shouldExtractFacts`); батчинг по эпизодам
>   гейтится замером extraction_log до/после (чартер: не менять семантику
>   экстракции вслепую).
> - MEM-10: counters в observability оформлены TODO-комментарием; все
>   memory-write пути логируют ошибки.
> - MEM-7: EXPLAIN-проверка использования HNSW-индекса — выполнить вручную на
>   реальной БД (в тестовом окружении нет Postgres с pgvector ≥ 0.7).
> Документ синхронизирован: MEMORY_ARCHITECTURE.md v6 (MEM-12).

**Источник:** аудит `docs/architecture/MEMORY_ARCHITECTURE.md` (Draft v5) против кода, 2026-06-11.
**Формат задач:** каждая задача самодостаточна и написана для AI-агента (Claude Code, модель Sonnet). Перед началом любой задачи прочитай `CLAUDE.md` (корень репо) и раздел документа, указанный в задаче. Не начинай задачу, не прочитав все файлы из блока «Файлы».

**Конвенции для агента-исполнителя:**
- Миграции: следующий свободный номер в `infra/migrations/` (на момент аудита — `013`), всегда парой `NNN_name.sql` + `NNN_name.down.sql`.
- Go: `go build ./... && go test ./...` из `backend-go/` должны быть зелёными до коммита.
- Python: `pytest` из `ai-python/` должен быть зелёным до коммита.
- Тесты пишутся против контракта (поведения), не против текущей реализации.
- Память строго per-chat (v5): никакая задача не должна вводить кросс-чатное чтение/запись фактов.

**Статус-легенда:** `[ ]` не начато · `[~]` частично · `[x]` сделано в рамках аудита.

---

## Исправлено в ходе аудита (для контекста)

- [x] **BUG: сводки эпизодов молча не сохранялись.** `summarizeChatDay` передавал `closure_reason='message_limit'` и `'daily_summary'`, а CHECK в `001_clean_schema.sql` допускает только `('silence_gap','message_count','topic_shift','manual')`; ошибка INSERT глотается в `SaveEpisodeSummary`. Исправлено: `service.go` теперь пишет `message_count`, миграция `012` расширила CHECK значением `daily_summary`.
- [x] **BUG: эмбеддинги сводок никогда не сохранялись.** `SaveEpisodeSummaryEmbedding` обновлял колонку `episode_summaries.embedding_model`, которой не существовало ни в одной миграции → UPDATE молча падал, embedding-worker бесконечно пере-эмбеддил одни и те же строки (расход LLM-кредитов). Исправлено миграцией `012` (добавлена колонка).

---

## P0 — критично: память пишется/дедупится неправильно

### MEM-1. Переписать `upsert_semantic_fact`: семантический дедуп сейчас сломан by design

**Проблема.** Документ (§5.8) требует: «сгенерировать embedding кандидата → top-1 cosine в пределах (chat, fact_type, subject) → 0.97 identical / 0.85 supersede». Реальная процедура (последняя версия — `infra/migrations/003_embeddings_qwen_4096.sql`) **не принимает embedding кандидата вообще**. Вместо этого tier 2/3 сравнивает существующие факты с эмбеддингом *последнего созданного факта чата* (вложенный SELECT `ORDER BY created_at DESC LIMIT 1`) — это сравнение «всех со всеми последним», не имеющее отношения к новому кандидату. Дополнительные дефекты той же процедуры:
1. Кандидат в момент INSERT ещё не имеет эмбеддинга (его пишет асинхронный worker позже) — значит, дедуп по cosine на write невозможен в текущей архитектуре в принципе; семантический tier — мёртвый код, дающий случайные supersede.
2. `UPDATE semantic_facts SET superseded_by_id = NULL WHERE id = v_existing` — no-op (бессмысленный остаток).
3. При supersede старый факт **не** получает `deleted_at = now()` (контракт §7.3); это маскируется ночным `consolidate_facts` (миграция 010), т.е. до ночи retrieval может выдать оба факта.
4. Поиск похожих не фильтрует по `fact_type`/`subject` (контракт §5.6: конфликт = тот же type + subject).
5. Мёртвая ветка `RAISE EXCEPTION` для `(owner_global, health)` — global scope упразднён в v5.
6. Возвращает `BIGINT` вместо `TABLE(action, fact_id)`; Go-код компенсирует костылём `FindSupersededFactID` (лишний запрос после каждого insert).

**Файлы.**
- `infra/migrations/003_embeddings_qwen_4096.sql` (текущая версия процедуры — только читать),
- новая миграция `infra/migrations/013_upsert_semantic_fact_v3.sql` (+ `.down.sql`),
- `backend-go/internal/store/store.go` — `UpsertSemanticFact`, `SaveFactEmbedding`, `FindSupersededFactID`,
- `backend-go/internal/core/service.go` — `saveTypedFacts`, `saveExtractedFacts`,
- `backend-go/internal/core/service_test.go`, `backend-go/internal/store/` тесты.

**Что сделать.**
1. Новая миграция `013`: `CREATE OR REPLACE FUNCTION upsert_semantic_fact(...)` с дополнительным параметром `p_embedding vector(4096) DEFAULT NULL` и возвратом `TABLE(action TEXT, fact_id BIGINT, superseded_fact_id BIGINT)`. Логика:
   - advisory lock как сейчас (tenant:scope:chat);
   - tier 1 — exact `normalized_fact_hash` → `('occurrence_added', existing_id, NULL)`;
   - tier 2/3 — **только если** `p_embedding IS NOT NULL`: top-1 cosine среди фактов того же `tenant, chat, fact_type, subject` (активных), сравнение с `p_embedding`; ≥0.97 → occurrence; 0.85–0.97 → INSERT нового + `UPDATE old SET superseded_by_id = new_id, deleted_at = now()` + вернуть `('superseded', new_id, old_id)`;
   - иначе INSERT → `('inserted', new_id, NULL)`; обработка `unique_violation`/`ON CONFLICT DO NOTHING` как сейчас → `('occurrence_added_race', ...)`;
   - если `p_embedding IS NOT NULL` — в той же транзакции INSERT в `fact_embeddings` (модель — параметр `p_embedding_model TEXT DEFAULT 'qwen/qwen3-embedding-8b'`), чтобы worker не пере-эмбеддил;
   - удалить ветку owner_global/health и все global-ветки поиска.
2. Go `store.UpsertSemanticFact`: принимать опциональный `embedding []float64`, сканировать `(action, fact_id, superseded_fact_id)`; вернуть всё это в `SemanticFactUpsertResult`. Удалить `FindSupersededFactID` и его вызов в `saveTypedFacts` — связь `fact_relations(supersedes)` создавать из `superseded_fact_id` результата.
3. Go `saveTypedFacts`/`saveExtractedFacts`: перед upsert синхронно получать embedding кандидата через `aiclient.TextEmbedder` (timeout 5s; код уже делает так для retrieval в `conversationContext` — переиспользуй паттерн). При ошибке/таймауте передавать `nil` — дедуп деградирует до hash-only (допустимо, embedding-worker доэмбеддит позже).
4. Тесты: (a) два последовательных upsert с одинаковым текстом → одна строка + occurrence; (b) upsert с embedding, близким к существующему той же пары (type, subject) → supersede, у старого `deleted_at IS NOT NULL`, создана запись `fact_relations`; (c) embedding=nil → факт вставлен, дедуп по хешу работает; (d) разные `fact_type` с похожими векторами НЕ supersede'ятся.

**Критерии приёмки.** Семантический дедуп использует embedding кандидата; supersede атомарно ставит `deleted_at`; `FindSupersededFactID` удалён; build/tests зелёные; в `docs/backlog.md` Tech Debt пункт про «dead RAISE EXCEPTION branch» закрыт.

**Не делать.** Не менять пороги 0.85/0.97; не возвращать scope `owner_global`; не делать очередь — advisory lock остаётся (§16.2).

---

### MEM-2. Deletion contract (§10.2): forget_chat / unlearn_fact / forget_all / export

**Проблема.** Раздел §10.2 не реализован вообще: в Go нет ни одного метода удаления памяти (grep `forget|unlearn` по `backend-go/internal` — пусто). GDPR-операции и право владельца стереть чат — обязательный контракт, на нём держится доверие к privacy-модели.

**Файлы.** `backend-go/internal/store/store.go`, `backend-go/internal/core/service.go`, `backend-go/internal/bot/layer.go` (+ тесты), `contracts/openapi.yaml` (если решишь дать HTTP-ручку — необязательно, бот-команд достаточно для MVP).

**Что сделать.**
1. `store.ForgetChat(tenantID, chatID)` — одна транзакция, hard delete в порядке: сначала health-факты (§10.3 — отдельным DELETE первым), затем `fact_occurrences`, `fact_relations`, `fact_embeddings`, `semantic_facts`, `episode_summaries`, `active_episodes`, `extraction_log`, `contact_profile_suggestions`, `contact_profiles`, `message_states`, `messages`, `drafts`, `draft_feedback`. Учитывай существующие FK ON DELETE CASCADE — не дублируй каскады вручную, но проверь каждый FK прежде чем полагаться на него.
2. `store.UnlearnFact(tenantID, factID)` — hard delete факта + embeddings + occurrences + relations.
3. `store.ForgetAll(tenantID)` — удаление всех данных tenant'а (chats, connections, settings и всё из п.1).
4. Каждая операция пишет `audit_events` (тип `memory_forget_chat` / `memory_unlearn_fact` / `memory_forget_all`) ДО удаления, в той же транзакции.
5. Бот-команды владельца в `bot/layer.go`: `/forget <номер чата>` с обязательным подтверждением через inline-кнопку (двухшаговый flow — используй существующий механизм callback-кнопок, как в draft approve), `/forget_all` с подтверждением фразой. `export` — отдельная подзадача: JSONL-дамп таблиц tenant'а в файл, отправляемый владельцу документом; если объём работ велик, оформи export отдельным PR.
6. Функциональные тесты: после `ForgetChat` все перечисленные таблицы пусты для этого чата, факты другого чата не тронуты; `audit_events` содержит запись.

**Критерии приёмки.** Команды работают end-to-end в тестах с реальной тестовой БД; никакая операция не выполняется без подтверждения владельца; audit-записи создаются.

**Не делать.** Никакого soft-delete здесь — §10.2 требует hard delete; не трогать данные других tenant'ов (обязательный `tenant_id` в каждом WHERE).

---

### MEM-3. `messages.risk_level` → NOT NULL (план §12 Phase 2)

**Проблема.** Колонка `messages.risk_level` (enum `risk_level`) до сих пор nullable; backfill-фаза из документа не выполнялась. Инвариант «policy_check заполняет risk_level до записи» не закреплён схемой.

**Файлы.** `infra/migrations/014_risk_level_not_null.sql` (+down), `backend-go/internal/store/store.go` (`SaveMessage`), `backend-go/internal/core/service.go` (место вызова policy-check).

**Что сделать.**
1. Проверь write-path: убедись, что `SaveMessage` всегда получает risk_level от policy-классификатора; если есть пути записи без него (owner-authored, passive) — проставь там `'low'` явно в Go-коде с комментарием-обоснованием.
2. Миграция: `UPDATE messages SET risk_level='low' WHERE risk_level IS NULL;` затем `ALTER TABLE messages ALTER COLUMN risk_level SET NOT NULL;` (документ §12 Phase 2 сознательно допускает дефолт 'safe' на историческом датасете; в нашей enum это `'low'`).
3. Тест: вставка сообщения без risk_level через репозиторий невозможна / получает 'low'.

**Критерии приёмки.** `SELECT count(*) FROM messages WHERE risk_level IS NULL` = 0; колонка NOT NULL; build/tests зелёные.

---

## P1 — контрактные дыры: фича описана, кода нет

### MEM-4. Contact profile suggestions flow (§4.5): таблица есть, кода — ноль

**Проблема.** Таблица `contact_profile_suggestions` создана в `001`, но ни одна строка кода её не использует (grep по `backend-go` и `ai-python` — пусто). Сейчас AI вообще не предлагает обновления профиля — слой «AI предлагает, владелец подтверждает» отсутствует, профиль заполняется только руками через бота. Это блокирует confidence-gates авто-режима (§11.6: «не повышать до accepted_facts ≥ N AND filled_profile_fields ≥ K»).

**Файлы.** `ai-python/ai_agent/pipelines/extraction.py`, `ai-python/ai_agent/schemas.py`, `contracts/openapi.yaml`, `backend-go/internal/aiclient/`, `backend-go/internal/store/store.go`, `backend-go/internal/core/service.go`, `backend-go/internal/bot/layer.go`.

**Что сделать.**
1. Python: расширь typed-извлечение — если LLM видит identity-свойство контакта (`relationship_type`, `preferred_address`, `language`, `tone`, `gender`), возвращай его отдельным массивом `profile_suggestions: [{field_name, suggested_value, rationale}]` в `TypedExtractionResponse` (не как факт!). Валидируй `field_name` по whitelist полей `contact_profiles`, значения — по тем же enum'ам, что CHECK'и в схеме (нормализация как в `_normalize_typed_fact`). Обнови контракт `contracts/openapi.yaml` синхронно (frozen contract — менять обе стороны одним PR).
2. Go: `store.SaveProfileSuggestion`, `store.PendingSuggestions(tenantID, chatID)`, `store.ResolveSuggestion(id, status)` (accepted/rejected; accepted → применить значение к `contact_profiles` в той же транзакции). Не позволяй прямой UPDATE профиля из extraction-пути — только через suggestions (анти-паттерн §15).
3. `saveTypedFacts`-аналог для suggestions в `service.go`: дедуп — не плодить pending-дубликаты на то же поле с тем же значением (UNIQUE partial index `(tenant_id, chat_id, field_name) WHERE status='pending'` новой миграцией — обнови и `ix_cps_pending`).
4. Бот: уведомление владельцу «ИИ считает, что Х — коллега. Принять?» с кнопками accept/reject (паттерн callback-кнопок из draft flow); команда `/suggestions` — список pending.
5. Фоновая экспирация: pending старше 14 дней → status='expired' (добавь в существующий nightly consolidation worker).
6. Тесты: suggestion создаётся, accept применяет поле к профилю, reject не применяет, дубликат pending не создаётся.

**Критерии приёмки.** AI-предложение никогда не пишет в `contact_profiles` напрямую; полный цикл suggest→accept→profile обновлён покрыт тестом; openapi и обе стороны контракта синхронны.

---

### MEM-5. Runtime state не заполняется: `open_question_message_id`, `pending_owner_action` всегда NULL

**Проблема.** §6.2 определяет три поля runtime-состояния. `current_topic` уже заполняется topic-tagger'ом, но `open_question_message_id` и `pending_owner_action` никто никогда не пишет (grep: единственное упоминание — чтение в `service.go` при сборке `AIProcessMessageRequest`). Промпт-блок `[RUNTIME: ожидается действие владельца]` существует, но мёртв.

**Файлы.** `ai-python/ai_agent/pipelines/topic.py` (паттерн дешёвого классификатора), новая логика в `ai-python/ai_agent/pipelines/`, `contracts/openapi.yaml`, `backend-go/internal/aiclient/`, `backend-go/internal/core/service.go` (`UpsertActiveEpisode` вызовы), тесты обеих сторон.

**Что сделать.**
1. Расширь существующий topic-tagging вызов (не добавляй отдельный LLM-вызов — это уже fire-and-forget запрос на каждое сообщение): пусть `/ai/tag-topic` возвращает дополнительно `{has_open_question: bool, pending_owner_action: string|null}` для текущего сообщения. Обнови openapi + Go-клиент.
2. Go: если `has_open_question` и сообщение от контакта → `open_question_message_id = event.SourceMessageID` в `UpsertActiveEpisode`. Если владелец ответил (owner-authored сообщение в чате) → сбрось `open_question_message_id` и `pending_owner_action` в NULL (отдельный UPDATE-метод store; текущий `UpsertActiveEpisode` использует COALESCE и не умеет занулять — учти это).
3. Прокинь `open_question` в контекст промпта (`conversationContext`): строка `[RUNTIME: незакрытый вопрос] <текст сообщения по open_question_message_id>`.
4. Тесты: вопрос от контакта ставит поле; ответ владельца сбрасывает; закрытие эпизода удаляет строку целиком (уже так — не сломай).

**Критерии приёмки.** Поля заполняются и сбрасываются по жизненному циклу §6.2; ни одного нового блокирующего LLM-вызова в webhook-пути.

---

### MEM-6. Эпизодическая идемпотентность: «одна сводка в день» теряет эпизоды

**Проблема.** Документ моделирует эпизоды (закрытие по тишине 6ч / 40 сообщений), но идемпотентность реализована как `EpisodeSummaryExistsForDate(date)` — одна сводка на чат **в день**. Если эпизод закрылся по message_count в полдень, вечерний эпизод того же дня молча не суммаризуется. Плюс параллельно живёт третий механизм — `StartDailySummaryScheduler` («сводка вчерашнего дня»), который не описан в документе вовсе: фактически два пересекающихся слоя суммаризации с потерями на стыке. `summarizeChatDay` к тому же суммаризует «весь день», а не сообщения эпизода (`period_start/end` = границы суток, `source_message_ids` не заполняется — провенанс §6 потерян).

**Файлы.** `backend-go/internal/core/service.go` (`summarizeChatDay`, `closeStaleEpisodes`, `runDailySummariesForDate`, message-limit блок в `HandleTelegramWebhook`), `backend-go/internal/store/store.go` (`EpisodeSummaryExistsForDate`, `SaveEpisodeSummary`), тесты.

**Что сделать.**
1. Введи `summarizeEpisode(tenantID, chatID, startedAt, closedAt, reason)`: берёт сообщения `MessagesByDateRange(startedAt, closedAt)` (границы эпизода, не суток), пишет `period_start=startedAt, period_end=closedAt`, заполняет `source_message_ids` реальными ID, `message_count` — числом сообщений эпизода.
2. Идемпотентность — по пересечению периодов: «существует сводка этого чата с `period_end > startedAt AND period_start < closedAt`» вместо проверки по дате.
3. Закрытие по message_count и по silence_gap используют `summarizeEpisode` с реальными границами (у `ActiveEpisodeRecord` есть `StartedAt`/`LastMessageAt`).
4. `StartDailySummaryScheduler` оставь как fallback-сетку, но он должен суммаризовать только сообщения, **не покрытые** ни одной существующей сводкой за тот день (иначе пропусти) — это закрывает дыру «чат активен, но эпизод не закрылся до полуночи».
5. Проверь CHECK `content BETWEEN 50 AND 1500`: если summarizer вернул < 50 символов, логируй и пропускай вместо молчаливого падения INSERT (связано с MEM-10).
6. Тесты: два эпизода в один день → две сводки; повторное закрытие того же эпизода → одна; daily-fallback не дублирует существующую.

**Критерии приёмки.** Ни один закрытый эпизод не теряется; `source_message_ids` заполнен; дубликатов сводок нет.

---

### MEM-7. Векторный поиск без индекса: вернуть ANN (halfvec HNSW)

**Проблема.** Миграция 003 перевела эмбеддинги на `vector(4096)` (qwen3-embedding-8b) и **сбросила HNSW-индексы**, т.к. pgvector HNSW поддерживает ≤2000 измерений для типа `vector`. Все cosine-запросы (`SemanticFactsByVector`, `EpisodeSummariesByVector`, tier 2/3 дедупа) — sequential scan. На текущем объёме терпимо, но это тикающая деградация, и §14 документа («переезд на Qdrant, если нужен HNSW») уже фактически наступил по своему триггеру.

**Файлы.** `infra/migrations/015_halfvec_hnsw.sql` (+down), `backend-go/internal/store/store.go` (cast'ы `::vector` в запросах), проверка версии pgvector в `infra/` (docker-образ).

**Что сделать.**
1. Проверь версию pgvector в используемом образе (нужна ≥ 0.7 для `halfvec`). Если ниже — сначала подними образ.
2. Миграция: expression-индексы без смены типа колонки (наименее инвазивно):
   `CREATE INDEX ... ON fact_embeddings USING hnsw ((embedding::halfvec(4096)) halfvec_cosine_ops);` и аналогично для `episode_summaries` (partial `WHERE deleted_at IS NULL AND embedding IS NOT NULL`).
3. Перепиши ORDER BY в `SemanticFactsByVector` / `EpisodeSummariesByVector` / процедуре дедупа на `(embedding::halfvec(4096) <=> $N::halfvec(4096))`, чтобы планировщик использовал expression-индекс. Внимание: в `SemanticFactsByVector` ORDER BY умножается на CASE-боусты типов — такой ORDER BY не использует индекс; раздели на «ANN top-(K×3) подзапрос → пере-сортировка с боустами снаружи».
4. `EXPLAIN`-проверка в тесте или вручную: запрос использует Index Scan по hnsw.

**Критерии приёмки.** ANN-индексы существуют и используются; результаты retrieval эквивалентны (top-K может незначительно отличаться — это свойство ANN, допустимо); обнови §14 документа в рамках MEM-12.

**Не делать.** Не менять embedding-модель и не вводить Qdrant — это отдельное решение с триггерами из §14.

---

### MEM-8. Health-факты: утечка в `extraction_log.raw_output` (§10.3)

**Проблема.** §10.3: health-факты «не попадают в `extraction_log.raw_output` в открытом виде (только fact_id)». Сейчас `LogExtraction` пишет сырой LLM-вывод целиком (обрезанный до 2000 символов в Python), включая текст health-фактов.

**Файлы.** `backend-go/internal/core/service.go` (вызовы `LogExtraction` после `saveTypedFacts`), `ai-python/ai_agent/pipelines/extraction.py`, тесты.

**Что сделать.**
1. В Go перед `LogExtraction`: если среди сохранённых фактов есть `fact_type='health'` (или `taboo` — реши консистентно и задокументируй), замени в `RawOutput` текст каждого такого факта на `[health fact #<fact_id>]`. Замена по подстроке `fact_text` достаточна (raw_output — JSON-массив с теми же текстами).
2. Тест: extraction с health-фактом → строка `extraction_log.raw_output` не содержит текста факта, но содержит fact_id-маркер; обычные факты остаются читаемыми.

**Критерии приёмки.** Ни один health-текст не хранится в extraction_log в открытом виде; диагностическая ценность лога сохранена.

---

### MEM-9. Token budget и порядок урезания контекста (§8.1) + paused_hours

**Проблема.** §8.1 задаёт бюджет и приоритет урезания (working → episodic → chat-facts; identity/role/runtime/hard-include не режутся). В `conversationContext` бюджета нет вообще: 20 сообщений + все pending-черновики + все commitments конкатенируются без ограничения — длинный чат с десятком черновиков раздувает промпт непредсказуемо. Также `view_chat_paused_hours` (создана в 001) нигде не вызывается — Python вынужден парсить таймстемпы строк, чтобы понять паузу (см. `_greeting_rule` в `draft.py`).

**Файлы.** `backend-go/internal/core/service.go` (`conversationContext`), `backend-go/internal/store/store.go` (добавить `PausedHours` по образцу `GreetedToday`), `backend-go/internal/core/service_test.go`.

**Что сделать.**
1. Добавь `store.PausedHours(tenantID, chatID)` → вызов `view_chat_paused_hours`; инжекть `[RUNTIME: paused_hours=N.N]` рядом с greeting-флагом. В `draft.py` `_greeting_rule` может использовать его как авторитетный сигнал паузы (флаг-провайдед паттерн там уже есть для greeting).
2. Бюджет: оценка токенов как `len(text)/3` (для ru/en смеси достаточно; не тащи tokenizer-зависимость). Константа `contextTokenBudget = 4000` (working+episodic+facts+drafts; identity/role собираются на Python-стороне и сюда не входят). Урезание строго в порядке §8.1: сначала messages 20→10, затем episodic 3→1, затем chat-facts 8→5, затем pending-drafts (оставить максимум 2 последних). Hard-include (taboo/health), runtime-строки и commitments не режутся никогда.
3. Лог при срабатывании урезания (`slog.Info`, поля: chat_id, что урезано, итоговая оценка).
4. Тесты: контекст с искусственно длинными сообщениями укладывается в бюджет; порядок урезания соблюдён; hard-include выживает при любом бюджете.

**Критерии приёмки.** Размер контекста ограничен и предсказуем; урезание детерминированное; paused_hours доступен промпту.

---

## P2 — гигиена, наблюдаемость, документ

### MEM-10. Write-path наблюдаемость: store глотает ошибки БД молча

**Проблема.** Оба бага, исправленные в ходе аудита, прожили в проде незамеченными именно потому, что `store.go` системно игнорирует ошибки: `_ = r.db.QueryRow(...).Scan(...)` / `_, _ = r.db.Exec(...)` в `SaveEpisodeSummary`, `SaveEpisodeSummaryEmbedding`, `SaveFactEmbedding`, `LogExtraction`, `SaveFactRelation` и др. Это нарушает чартер («Errors must be handled or explicitly ignored with a comment why») и делает memory-слой ненаблюдаемым.

**Файлы.** `backend-go/internal/store/store.go` (все write-методы memory-слоя).

**Что сделать.**
1. Добавь `logger *slog.Logger` в `Repository` (через конструктор; есть ли он уже — проверь), и в каждом write-методе memory-слоя логируй ошибку `Warn` с операцией, tenant_id, chat_id. Не меняй сигнатуры на возврат error там, где вызывающие пути fire-and-forget; меняй на возврат error там, где вызывающий уже обрабатывает (например `SaveEpisodeSummary` → id=0 уже трактуется как провал — добавь лог внутри).
2. Счётчики (§12 Phase 5.4): через существующий `observability`-пакет добавь counters `memory_write_errors_total{op}`, `fact_upsert_actions_total{action}` (inserted/occurrence_added/superseded/rejected — данные уже возвращаются из MEM-1). Если в `observability` нет counters-механики — лог-поля достаточно, метрики оформи TODO-комментарием.
3. Тест: при закрытой/сломанной БД write-метод не паникует и логирует.

**Критерии приёмки.** Ни одного `_ = r.db...` в memory-write-путях без лога или комментария-обоснования.

---

### MEM-11. Запретить прямые INSERT в `semantic_facts` (анти-паттерн §15) — или честно убрать из документа

**Проблема.** Документ обещает «Прямые INSERT в semantic_facts запрещены через GRANT» и `REVOKE ... FROM app_role`, но ни одного GRANT/REVOKE в миграциях нет, роль `app_role` не существует, а `store.SaveSemanticFact` содержит прямой INSERT (используется как in-memory fallback, но DB-ветка прямого INSERT доступна).

**Файлы.** `backend-go/internal/store/store.go` (`SaveSemanticFact`, `UpsertSemanticFact`), `infra/migrations/016_app_role_grants.sql` (+down), деплой-конфиг подключения к БД (`backend-go/internal/config/`, `infra/`).

**Что сделать.**
1. Сначала реши масштаб честно: если бэкенд подключается суперюзером и менять это сейчас не планируется — задача сводится к удалению DB-ветки прямого INSERT из `SaveSemanticFact` (оставить только in-memory fallback при `r.db == nil`, DB-путь — всегда через процедуру) и фиксации в документе, что GRANT-защита отложена до появления выделенной роли. Это минимум, делается без миграции.
2. Полный вариант (если в `infra/` есть управление пользователями БД): миграция создаёт `app_role`, `REVOKE INSERT, UPDATE ON semantic_facts, fact_embeddings FROM app_role; GRANT EXECUTE ON FUNCTION upsert_semantic_fact TO app_role;` и деплой переключает подключение Go на эту роль. Делай только если можешь проверить на реальной БД.
3. Тест: запись фактов в DB-режиме проходит исключительно через `upsert_semantic_fact`.

**Критерии приёмки.** В кодовой базе не остаётся используемого DB-пути прямого INSERT в `semantic_facts`; выбранный вариант отражён в MEMORY_ARCHITECTURE.md (см. MEM-12).

---

### MEM-12. MEMORY_ARCHITECTURE.md v6: привести документ к реальности

**Проблема.** Документ отстал от кода и содержит внутренние противоречия; список ниже — конкретные правки. Документ — контракт, расхождение с кодом обесценивает его.

**Файлы.** `docs/architecture/MEMORY_ARCHITECTURE.md`, сверка с `infra/migrations/003,008,009,010,011,012` и кодом.

**Что исправить (каждый пункт — проверяемое расхождение):**
1. **Эмбеддинги:** всюду заменить `text-embedding-3-small` / `vector(1536)` / ivfflat на реальность: `qwen/qwen3-embedding-8b`, `vector(4096)`, асинхронный embedding-worker в Go, ANN-статус (после MEM-7 — halfvec HNSW). Описать write-path честно: embedding кандидата приходит параметром процедуры (после MEM-1), а не «генерируется в stored proc» (PG не умеет).
2. **§9 DDL:** убрать «исторические» DDL `pending_global_health_facts` и процедуру `accept_pending_health_fact`, а также GRANT на несуществующие `change_fact_scope`/`accept_pending_health_fact` — это шум, противоречащий v5. Исправить невалидный индекс `ix_ae_stale ... WHERE last_message_at < now() - interval '6 hours'` — `now()` не immutable, такой partial index не создастся (001 правильно сделал обычный индекс).
3. **§10.1:** удалить флаг `no_global_facts` и фразу «Global-факты владельца... остаются видны» — наследие v3, противоречит v5. Сверить имена флагов с кодом: в коде `no_summary` (ед. число, `contract_enums`), в документе `no_summaries`.
4. **Новые подсистемы, реализованные в коде, но отсутствующие в архитектуре:** temporal knowledge graph (`fact_relations`, view `current_facts`, миграция 009), sleep-time consolidation (`consolidate_facts`, nightly worker, миграция 010), embedding worker, topic tagger, reranker `/ai/rerank-facts`, daily-summary-fallback (после MEM-6 — описать единый механизм). Добавить разделы или changelog-пункт v6.
5. **§12 Migration plan:** пометить как выполненный/устаревший — `memory_facts` не существует (001 — clean schema), фазы 1–3 неприменимы; оставить только невыполненные пункты (risk_level → MEM-3, suggestions UI → MEM-4, GRANT'ы → MEM-11) со ссылками на этот беклог.
6. **§14 Qdrant:** обновить триггеры — «нужен HNSW» уже наступил и решён иначе (halfvec); сформулировать новые триггеры (например, p95 retrieval latency, объём фактов).
7. **§0:** пометить вводные метрики (1400 строк memory_facts, 62% дубликатов) как исторический контекст v1 — таблиц этих больше нет.
8. **§5.8/§9.1:** синхронизировать псевдокод процедуры с фактической сигнатурой после MEM-1 (возврат `(action, fact_id, superseded_fact_id)`; advisory lock без fact_type/subject — задокументировать сознательно выбранную более грубую гранулярность или поменять в MEM-1, одно из двух).
9. **§7.1:** отразить реальный pipeline: extraction per-message с окном последних 5 сообщений (не «batched 5-10»), либо оставить батчинг как целевое состояние со ссылкой на MEM-13. Отразить лимит Python-валидатора `fact_text ≤ 120` (или ослабить его до контрактных 200 — реши и сделай консистентно).

**Критерии приёмки.** Каждый DDL/инвариант в документе либо существует в миграциях/коде, либо помечен как план со ссылкой на задачу беклога; changelog v6 добавлен.

---

### MEM-13. Стоимость экстракции: per-message LLM-вызовы без батчинга

**Проблема.** На каждое входящее сообщение уходит до трёх LLM-вызовов (typed extraction + topic tagging + embedding запроса) — для small-talk-чатов это сжигание бюджета ради `[]` (это уже видно в `extraction_log`: почти все вызовы возвращают пусто). Документ §7.1 предполагает батчинг 5–10 сообщений.

**Файлы.** `backend-go/internal/core/service.go` (extraction-блок в `HandleTelegramWebhook`, `processPassive`), `extraction_log` данные прода.

**Что сделать.**
1. Сначала измерь (чартер: не менять семантику экстракции вслепую): по `extraction_log` посчитай долю вызовов с пустым результатом и среднюю длину входа. Если пустых > 80% — батчинг оправдан.
2. Дебаунс-батчинг: вместо немедленного вызова — накапливай message_ids эпизода и запускай extraction при (a) закрытии эпизода, (b) каждые N=5 сообщений, (c) до генерации драфта (чтобы факт из текущего сообщения успел в контекст следующего ответа — проверь, нужен ли тут сценарий вообще, ведь текущее сообщение и так в промпте). `extraction_log.input_message_ids` уже массив — заполняй все ID батча.
3. Дешёвый pre-filter перед LLM: пропускай extraction для сообщений короче ~15 символов без цифр/имён (метрика из п.1 покажет порог).
4. Сравни `extraction_log` до/после: фактов на 1000 сообщений не меньше, вызовов — кратно меньше.

**Критерии приёмки.** Число extraction-вызовов на сообщение < 0.5 при сохранении выхода фактов; решение подтверждено данными extraction_log до/после.

---

## Сводная таблица соответствия документа и кода (результат аудита)

| Компонент (раздел дока) | Статус в коде |
|---|---|
| Слои: user_settings / contact_profiles / chat_settings (§3–4) | ✅ есть; cold-start defaults по relationship_type реализованы в `contracts/types.go` |
| active_episodes lifecycle (§6.2) | 🟡 upsert/закрытие/topic есть; open_question и pending_owner_action не заполняются (MEM-5); идемпотентность по дате теряет эпизоды (MEM-6) |
| Derived views (§6.2) | 🟡 greeted_today и open_commitments используются; paused_hours — нет (MEM-9) |
| episode_summaries (§7.5) | 🟡 работает после фикса 012; границы = сутки, а не эпизод; source_message_ids пуст (MEM-6) |
| semantic_facts + dedup-on-write (§5.8) | 🔴 hash-tier работает; semantic-tier сломан by design (MEM-1) |
| Hard-include taboo/health (§8.2) | ✅ `hardIncludeChatFacts` в retrieval |
| Retrieval-веса (§8.2) | ✅ commitment/preference боусты в `SemanticFactsByVector` |
| Hybrid search BM25+RRF+reranker (§8) | ✅ миграция 008, `rrfFuse`, `/ai/rerank-facts` |
| Token budget (§8.1) | 🔴 отсутствует (MEM-9) |
| Suggestions flow (§4.5) | 🔴 таблица без кода (MEM-4) |
| Deletion contract (§10.2) | 🔴 отсутствует (MEM-2) |
| Health в extraction_log (§10.3) | 🔴 утечка raw_output (MEM-8) |
| Privacy no_memory / no_summary / private (§10.1) | ✅ проверяются в extraction/summary/decision путях |
| AI-роли (§13) | ✅ миграция 011, бот-команды, role-first промпт в `draft.py` |
| Строго per-chat scope (§5.3–5.4) | ✅ Go хардкодит `scope_kind='chat'`, хинты Python игнорируются |
| GRANT-защита от прямых INSERT (§9) | 🔴 не существует (MEM-11) |
| risk_level NOT NULL (§6.1, §12) | 🔴 не выполнено (MEM-3) |
| Temporal graph, consolidation (вне дока) | ✅ в коде (009/010), в документе отсутствуют (MEM-12) |
