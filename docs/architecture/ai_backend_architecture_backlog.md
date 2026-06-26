# AI / Backend Architecture Backlog

> **СТАТУС (2026-06-11): все задачи ARCH-1…ARCH-15 реализованы**, оба набора
> тестов зелёные (`pytest` 134, `go test` 80). ARCH-4 закрыл ARCH-2/3/6.
> Остаточные хвосты, сознательно отложенные:
> - ARCH-5: `/ai/rerank-facts` и `FactReranker` живут до shadow-замера на
>   ~50 реальных запросах; из горячего пути LLM-rerank убран (косинус в SQL,
>   `RankFactsByCosine` + `finalizeFactOrder`).
> - ARCH-14(2): прямой OpenRouter-путь в `embedding.py` остаётся dev-fallback
>   до Phase-5 cutover (см. backlog.md).
> - Связка с MEM-13: дебаунс-батчинг extraction по эпизодам гейтится замером
>   extraction_log; реализован pre-filter коротких сообщений
>   (`shouldExtractFacts`).
> Текст задач ниже сохранён как контекст принятых решений.

Аудит блока `ai-python` + `backend-go` (2026-06-11): эффективность использования данных,
модель LLM-вызовов, алгоритмы. Каждая задача написана как **самодостаточный промпт для
AI-агента-разработчика (Claude Code / Sonnet)**: контекст → что сделать → файлы → критерии приёмки.

Связанные беклоги: [memory_architecture_backlog.md](memory_architecture_backlog.md) (MEM-1…MEM-13),
[backlog.md](../backlog.md) (LLM Safety & Observability — отдельный трек, сюда не переносится).

---

## Сводка аудита

**Текущая модель вызовов на одно входящее сообщение** (горячий путь → ответ собеседнику):

```
webhook → debounce sleep (replyDelay)
  → embed(message)            ~2-5s   API-вызов
  → vector + BM25 + RRF       ~10 DB roundtrips на сборку контекста
  → rerank-facts              ~2-5s   LLM-вызов (listwise)
  → process-message (draft)   ~5-30s  LLM-вызов
async после ответа:
  → tag-topic                 LLM-вызов (КАЖДОЕ сообщение)
  → extract-facts-typed       LLM-вызов
```

Итого: **до 4 LLM-вызовов + 1 embed на каждое сообщение**, из них 2 LLM-вызова сидят в горячем
пути латентности. Вердикт аудита: **полноценный агентный harness (tool-loop) здесь НЕ нужен** —
у сервиса нет инструментов, которые модель должна вызывать итеративно: весь контекст (память,
профиль, фидбек) Go собирает детерминированно ДО вызова. Правильная цель — **структурированный
пайплайн с одним LLM-вызовом на стадию и валидируемым JSON-выходом** (см. ARCH-4): горячий путь
сжимается до `embed + 1 LLM`, асинхронный — до `1 LLM`, а intent/confidence/risk перестают быть
regex-театром. Это даёт ~50-70 % экономии вызовов и убирает 5-10 s латентности без потери качества.

**Уже исправлено в ходе аудита** (см. git-диф от 2026-06-11, тесты зелёные):
- `classifiers.py`: substring-матчинг → word-boundary regex («страх»≠«трах», «корабля»≠«бля»,
  «анализ»≠«анал» больше не дают CRITICAL/HIGH); добавлены русские ключи риска
  (пароль/оплата/договор/срочно) и русские интенты (без них всё падало в `general_message`=0.72
  и авторежим был недостижим для русского языка).
- `classifiers.py`: из `_COMMITMENT_PATTERNS` удалены голые `к`/`by` — они матчили почти каждое
  сообщение и заваливали `semantic_facts` мусорными commitment-фактами.
- `security.py`: русские паттерны prompt-injection («забудь все инструкции», «системный промпт»…).
- `pipelines/jsonio.py` (новый): срезание ```json-фенсов перед `json.loads` в extraction и
  reranking — раньше обёрнутый в фенсы ответ модели молча давал `[]`.
- `service.go`: гонка в topic-tagging — горутина писала назад устаревший снапшот эпизода и
  откатывала `MessageCount`/`LastMessageAt`; теперь перечитывает эпизод перед записью.
- `service.go`: `rrfFuse` — вставочная сортировка O(n²) → `sort.Slice` с tie-break по id
  (детерминизм при равных score).

---

## P0 — корректность и безопасность

### ARCH-1. Canned-предупреждение для critical-risk можно отправить собеседнику

**Контекст.** При critical-risk (профанация/секс-контент) `maybe_generate_response`
([draft.py](../../ai-python/ai_agent/pipelines/draft.py), `_MANUAL_REASONS`) возвращает текст
«⚠️ Сообщение интимного характера. ИИ не формирует ответ — ответьте вручную» как `response_text`.
Go `DecisionEngine.Decide` видит непустой `ResponseText` при `RiskCritical` → `AskOwner` →
`createDraft(event, eventID, proposal.ResponseText)` ([service.go:54-59, 566-612](../../backend-go/internal/core/service.go)).
Кнопка «Approve» на такой карточке **отправит служебное предупреждение собеседнику**.

**Задача.** Раздели owner-facing предупреждение и contact-facing черновик:
1. В Python верни предупреждение в отдельном поле (например `owner_note`) или оставь
   `response_text=""` и добавь reason-код; обнови контракт `/contracts/openapi.yaml` и обе стороны.
2. В Go при critical+manual-reason создавай draft c пустым `ResponseText` и `DraftType="warning"`,
   текст предупреждения — в карточку (`bot.DraftCardText`), кнопки Approve/Edit скрой, оставь
   только Reject/Reply-manually.
3. Функциональный тест: critical-сообщение → карточка владельцу есть, отправка собеседнику
   невозможна ни одним callback'ом.

**Файлы:** `ai-python/ai_agent/pipelines/draft.py`, `ai-python/ai_agent/schemas.py`,
`backend-go/internal/core/service.go`, `backend-go/internal/bot/layer.go`, `contracts/openapi.yaml`.

### ARCH-2. `choice_question` — мёртвая ветка: интент никто не производит

**Контекст.** Go ждёт `proposal.Intent == "choice_question"` и строит question-card с кнопками
([service.go:569](../../backend-go/internal/core/service.go), `parseOptions`, `sendQuestionToOwner`,
`handleChoiceOption` — всё работает), но `IntentClassifier`
([classifiers.py](../../ai-python/ai_agent/classifiers.py)) возвращает только
question/scheduling/commercial/acknowledgement/request/general_message. Ветка недостижима.

**Задача.** Реализуй производителя интента в рамках ARCH-4 (структурированный выход draft-вызова:
LLM сам помечает `intent="choice_question"` и кладёт варианты в массив `options`). Если ARCH-4
откладывается — добавь детерминированный детектор («или» между альтернативами + «?») с тестами.
Не удаляй Go-ветку — она рабочая.

**Файлы:** `ai-python/ai_agent/pipelines/draft.py`, `ai-python/ai_agent/classifiers.py`,
`backend-go/internal/core/service.go` (только чтение), тесты обеих сторон.

### ARCH-3. Confidence — синтетический: авторежим гейтится regex-ключами, а не уверенностью модели

**Контекст.** `confidence_for` ([decisions.py](../../ai-python/ai_agent/pipelines/decisions.py)) =
`min(0.96, intent_confidence + 0.1)`, где `intent_confidence` — захардкоженные числа
regex-классификатора (0.72–0.88). Пороги Go (`SemiAutoMinConfidence=0.85`, `AutoMinConfidence=0.92`)
сравниваются с этим числом ⇒ авто-ответ де-факто разрешает класс ключевых слов, а не уверенность.
Вопрос («?») всегда даёт 0.96 ≥ 0.92 → в auto-режиме любой low-risk вопрос уходит автоматически.

**Задача.** В рамках ARCH-4 получай self-assessed confidence из структурированного ответа LLM
(`confidence: float`), и комбинируй: `final = min(llm_confidence, risk_cap)` — риск-кэпы из
`confidence_for` остаются как потолок (LOW→0.96, MEDIUM→0.84, HIGH→0.55, CRITICAL→0.35).
Regex-интент остаётся fallback'ом при невалидном JSON. Пороговые значения не менять
(контракт `contract_enums.gen.go` генерируется — менять только через contract source).

**Файлы:** `ai-python/ai_agent/pipelines/decisions.py`, `ai-python/ai_agent/service.py`,
`ai-python/ai_agent/pipelines/draft.py`.

---

## P1 — модель вызовов («harness»)

### ARCH-4. Консолидация: один структурированный LLM-вызов вместо зоопарка

**Контекст.** Сейчас draft-вызов возвращает голый текст; intent/risk/confidence считаются regex'ами;
topic — отдельный LLM-вызов на каждое сообщение; choice-options не существуют (ARCH-2).
Все данные для этих сигналов уже находятся в одном промпте draft-вызова.

**Задача.** Переведи `/ai/process-message` на структурированный JSON-выход одной генерацией:
```json
{
  "reply": "текст ответа",
  "intent": "question|scheduling|commercial|acknowledgement|request|choice_question|general_message",
  "options": ["вариант 1", "вариант 2"],   // только для choice_question
  "confidence": 0.0-1.0,                    // самооценка уместности авто-отправки
  "topic": "3-7 слов — тема разговора"      // заменяет /ai/tag-topic в горячем пути
}
```
Требования:
1. Жёсткая валидация выхода (по образцу `_normalize_typed_fact` в
   [extraction.py](../../ai-python/ai_agent/pipelines/extraction.py)): невалидный JSON → fallback на
   текущее поведение (текст как reply, regex-интент, синтетический confidence). Используй
   `jsonio.strip_code_fences`.
2. Детерминированный `RiskClassifier` ОСТАЁТСЯ источником risk_level (политика — на бэкенде,
   per charter); LLM риск не решает.
3. `topic` из ответа Go пишет в `active_episodes.current_topic` (вместо отдельного `TagTopic`
   fire-and-forget вызова — его убрать из `HandleTelegramWebhook`; эндпоинт `/ai/tag-topic`
   оставить для эпизодных воркеров).
4. Контракт: новые поля в `AIProcessMessageResponse` (`contracts/openapi.yaml`, Go
   `contracts/types.go`, Python `schemas.py`) — обновить обе стороны атомарно.
5. Тесты против контракта: валидный JSON → поля проброшены; мусорный выход → fallback;
   mock-gateway обновить, чтобы он отдавал валидный JSON для known keywords.

**Эффект:** −2 LLM-вызова на сообщение (topic + будущий rerank), живой confidence (ARCH-3),
рабочий choice_question (ARCH-2).

**Файлы:** `ai-python/ai_agent/pipelines/draft.py`, `ai-python/ai_agent/service.py`,
`ai-python/ai_agent/schemas.py`, `ai-python/ai_agent/llm.py` (mock), `contracts/openapi.yaml`,
`backend-go/internal/contracts/types.go`, `backend-go/internal/core/service.go`,
`backend-go/internal/aiclient/*`.

### ARCH-5. Заменить listwise-LLM-reranking на детерминированный/локальный

**Контекст.** `rerankFacts` ([service.go](../../backend-go/internal/core/service.go)) шлёт top-20
кандидатов в `/ai/rerank-facts`, где **LLM** просят отсортировать ID
([reranking.py](../../ai-python/ai_agent/pipelines/reranking.py)). Это +1 LLM-вызов и до 5 s в горячем
пути, при том что: (а) кандидаты уже отранжированы RRF (vector+BM25), (б) у фактов уже есть
эмбеддинги в `fact_embeddings` — косинус к query-вектору считается в SQL бесплатно.

**Задача.**
1. Быстрая победа: в Go пропускай rerank, когда `len(chatFacts) <= topChat` (8) — сейчас LLM зовут
   даже для 3 кандидатов.
2. Замена: вместо LLM-вызова — финальное упорядочивание по косинусу в SQL
   (`ORDER BY fe.embedding <=> $query LIMIT 8` по fused-ID) либо локальный cross-encoder
   (`BAAI/bge-reranker-v2-m3`) внутри ai-python без внешнего API. Решение зафиксируй замером:
   shadow-сравнение порядка фактов на 50 реальных запросах (см. `extraction_log`-подход).
3. `/ai/rerank-facts` и `FactReranker` удалить только после cutover-замера.

**Файлы:** `backend-go/internal/core/service.go` (`conversationContext`, `rerankFacts`),
`backend-go/internal/store/store.go` (новый запрос), `ai-python/ai_agent/pipelines/reranking.py`.

### ARCH-6. Topic-tagging: троттлинг и контекст

**Контекст.** LLM-вызов `TagTopic` стреляет на **каждое** входящее сообщение
([service.go:372-390](../../backend-go/internal/core/service.go)), при этом Go передаёт только
`MessageText`, хотя контракт `TopicTaggingRequest.recent_messages` поддерживает историю
([topic.py](../../ai-python/ai_agent/pipelines/topic.py)) — тема по одному сообщению почти всегда шум.

**Задача.** (Снимается автоматически, если ARCH-4 переносит topic в draft-ответ.)
Иначе: тегируй раз в N сообщений (`ep.MessageCount % 5 == 1`) и передавай последние 5 сообщений
(`RecentMessages` уже фетчится рядом — переиспользуй, см. ARCH-8). Не вызывай LLM для сообщений
короче ~10 символов («ок», «да»).

**Файлы:** `backend-go/internal/core/service.go`.

### ARCH-7. Быстрый ACK вебхука + ограниченный пул асинхронных задач

**Контекст.** `HandleTelegramWebhook` держит HTTP-запрос Telegram открытым через
`time.After(replyDelay)` + embed + rerank + LLM-draft — десятки секунд. Telegram ретраит медленные
вебхуки → дубли (идемпотентность их ловит, но соединения и порядок страдают). Параллельно каждое
сообщение порождает несвязанные горутины (topic, extraction, summarize) без пула и backpressure —
при всплеске это шторм из 30-секундных LLM-вызовов.

**Задача.**
1. Раздели приём и обработку: вебхук-хендлер нормализует, персистит, отвечает 200 сразу;
   debounce+AI-вызов выполняет внутренняя очередь (канал + воркеры, по ключу чата для порядка).
   Идемпотентность (`MarkReceive`) уже есть — переиспользуй.
2. Асинхронные LLM-задачи (extraction, topic, episode summarize) — через общий ограниченный пул
   (семафор как в `runDailySummariesForDate`, `maxSummaryConcurrency`), не голые `go func()`.
3. Тест: вебхук отвечает < 1 s при медленном AI-клиенте (fake с задержкой); burst-debounce
   поведение сохраняется (существующие тесты остаются зелёными).

**Файлы:** `backend-go/internal/core/service.go`, `backend-go/internal/httpapi/server.go`,
`backend-go/cmd/backend/main.go`.

### ARCH-8. Сборка контекста: ~10 последовательных DB-roundtrip'ов на сообщение

**Контекст.** На каждый inbound: `ChatSettings`, `SaveChat`, `ActiveEpisodeByChat` (×2),
`GreetedToday`, `OpenCommitments`, `SemanticFactsByVector`, `SemanticFactsByBM25`,
`SemanticFactsByIDs`, `EpisodeSummariesByVector`, `RecentMessages` (×2: limit 20 для контекста и
limit 5 для extraction-окна), `DraftsForChat`, `RecentFeedback`, `ContactProfile`, `UserSettings`,
`AIRoleForChat` — все последовательно ([service.go `conversationContext` + `HandleTelegramWebhook`](../../backend-go/internal/core/service.go)).

**Задача.**
1. Переиспользуй один фетч `RecentMessages(20)` и для extraction-окна (последние 5 из него).
2. Сгруппируй независимые чтения в `errgroup`/горутины ИЛИ одну SQL-функцию
   `get_chat_context(tenant_id, chat_id, query_vec)` возвращающую JSON (по образцу
   `view_chat_greeted_today`). Цель: ≤ 3 roundtrip'а до LLM-вызова.
3. Перенесено из [backlog.md](../backlog.md): `ContextScore` пересчитывается на каждый чек выбора
   режима — кэшируй per chat с коротким TTL.

**Файлы:** `backend-go/internal/core/service.go`, `backend-go/internal/store/store.go`,
`infra/migrations/` (новая функция, если выбран SQL-путь).

---

## P2 — эффективность данных и качество

### ARCH-9. Температура per task (сейчас 0.2 на всё)

`OpenRouterLLMGateway._complete_with_model` хардкодит `temperature: 0.2`
([llm.py:104](../../ai-python/ai_agent/llm.py)) для всех задач. Черновики получаются однообразными —
промпт даже содержит костыль про «не цитируй факт в каждом ответе», а extraction/rerank наоборот
хотят 0. Добавь `temperature` в `GatewayRequest`, роутер назначает per `ModelTask`
(DRAFT_REPLY≈0.6, EXTRACT_FACTS/rerank=0.0, SUMMARIZE_CHAT≈0.3); прокинь в OpenRouter и LiteLLM
гейтвеи; mock игнорирует.
**Файлы:** `ai-python/ai_agent/llm.py`, `ai-python/ai_agent/llm_litellm.py`, `ai-python/ai_agent/schemas.py`.

### ARCH-10. Суммаризация: structured output вместо англоязычных эвристик и мёртвого поля

**Контекст.** `summarize_chat` ([summarization.py](../../ai-python/ai_agent/pipelines/summarization.py)):
action items — substring-эвристика по англоязычным токенам (`please/send/need…`) → для русского
продукта **никогда не срабатывает**, и возвращает сырые сообщения целиком. Поле
`Recommendations` есть в Go-контракте ([types.go:576](../../backend-go/internal/contracts/types.go))
и используется в `sendRecommendationsToOwner`, но **Python его никогда не заполняет** — мёртвый код.

**Задача.** Один структурированный вызов: `{summary, action_items[], recommendations[]}` JSON
(валидация + `strip_code_fences`, fallback: только summary). Удали `_ACTION_TOKENS`-эвристику.
Обнови `contracts/openapi.yaml` + Python `schemas.py` (поля `recommendations`) — сейчас контракт
рассинхронизирован с Go. Тест: русский диалог с задачами → action_items непустой.
**Файлы:** `ai-python/ai_agent/pipelines/summarization.py`, `ai-python/ai_agent/schemas.py`,
`contracts/openapi.yaml`, `backend-go/internal/core/service.go` (только проверить).

### ARCH-11. Subject-метки детерминированного экстрактора неверны

`TypedMemoryExtractor` ([classifiers.py](../../ai-python/ai_agent/classifiers.py)) помечает
commitment/health как `subject="owner"`, но extraction вызывается **только для сообщений
собеседника** (owner-сообщения выходят раньше — `owner_message_observed`). «Я пришлю договор» от
контакта записывается как обязательство владельца → `OpenCommitments` подмешивает в промпт чужие
обязательства как свои. Реши семантику (по умолчанию `subject="contact"` для обоих) и поправь
метки + докстринги; проверь `view_open_commitments` (фильтрует ли по subject) перед изменением.
**Файлы:** `ai-python/ai_agent/classifiers.py`, `infra/migrations/` (просмотр view).

### ARCH-12. Embedding-воркер: 5-минутная дыра в ретривале

Новые факты получают эмбеддинги только тиком воркера раз в 5 минут
([service.go `StartEmbeddingWorker`](../../backend-go/internal/core/service.go)) — до этого они
невидимы для vector recall (BM25 частично спасает). Добавь nudge-канал: `saveTypedFacts` после
вставки шлёт неблокирующий сигнал воркеру (`select { case ch <- struct{}{}: default: }`), воркер
слушает тикер ИЛИ канал. Батч-логика не меняется.
**Файлы:** `backend-go/internal/core/service.go`.

### ARCH-13. In-memory режим Repository (перенесено из backlog.md)

1. `Repository.UpsertSemanticFact` аппендит в in-memory `semanticFacts` даже при настроенной DB —
   неограниченный рост за жизнь процесса ([store.go:1974-1976](../../backend-go/internal/store/store.go));
   слайс нужен только как no-DB fallback. Убери аппенд на DB-пути (и в `SaveSemanticFact`).
2. `RecentMessages` в in-memory режиме итерирует все сообщения O(n) на каждый вызов — для больших
   in-memory датасетов перейди на отсортированную структуру per chat.
**Файлы:** `backend-go/internal/store/store.go`.

### ARCH-14. Перенесённые контрактные хвосты (из backlog.md, Tech Debt v5)

1. `/ai/generate-draft` (`DraftRequest`) не несёт `ai_role` — только `/ai/process-message`.
   Если standalone-эндпоинт остаётся (его использует `regenerateDraft`? — проверь: regenerate идёт
   через process-message; standalone используют тесты/внешние клиенты) — расширь контракт ролью,
   иначе пометь deprecated в openapi.
2. `ai-python/ai_agent/pipelines/embedding.py` — embed-путь при выключенном LiteLLM ходит в
   OpenRouter напрямую; при cutover-чистке переведи окончательно через LiteLLM
   (связано с Phase 5 в [backlog.md](../backlog.md)).
**Файлы:** `contracts/openapi.yaml`, `ai-python/ai_agent/schemas.py`,
`ai-python/ai_agent/pipelines/embedding.py`.

### ARCH-15. Токен-бюджет промпта (связка с MEM-9)

`conversationContext` кладёт 20 сообщений + 8 фактов + 3 сводки + ВСЕ pending-черновики +
обязательства без учёта токенов — длинные чаты раздувают вход draft-вызова (расход + разбавление
внимания). Реализуй бюджетирование из design §8.1 (см. MEM-9 в
[memory_architecture_backlog.md](memory_architecture_backlog.md)): приоритет
[факты hard-include → обязательства → черновики → сводки → сообщения с конца], грубая оценка
4 chars/token, потолок ~2000 токенов контекста.
**Файлы:** `backend-go/internal/core/service.go`.

---

## Порядок выполнения

```
ARCH-1 (safety)  →  ARCH-4 (structured output; закрывает ARCH-2, ARCH-3, ARCH-6)
                 →  ARCH-5 (rerank)  →  ARCH-7 (async ACK + pool)  →  ARCH-8 (DB batching)
P2 — независимы, можно параллельно; ARCH-15 после ARCH-8.
```

Каждую задачу завершать: `cd ai-python && python3 -m pytest tests/ -q` и
`cd backend-go && go test ./...` — оба набора обязаны быть зелёными.
