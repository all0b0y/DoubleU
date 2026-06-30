# План разделения крупных Go-файлов `service.go` и `store.go`

Дата: 2026-06-29

## Цель

Разнести крупные Go-файлы по ответственностям без смены пакетов:

- `backend-go/internal/core/service.go` остается в `package core`;
- `backend-go/internal/store/store.go` остается в `package store`;
- публичные методы `Service` и `Repository` сохраняют текущие сигнатуры;
- изменения должны быть механическими и проверяемыми после каждого среза.

Текущее состояние:

- `backend-go/internal/core/service.go` - около 5077 строк;
- `backend-go/internal/store/store.go` - около 3540 строк;
- в `store` уже есть вынесенные файлы: `chats.go`, `scheduled_jobs.go`, `user_bots.go`, `migrations.go`;
- в рабочей копии также есть новый `interaction_forms.go`, поэтому будущие срезы должны учитывать текущие локальные изменения.

## Принципы разделения

1. Делить по доменным зонам ответственности, а не по размеру файла.
2. Оставаться в текущих пакетах, чтобы не создавать циклические зависимости и не менять импортную модель.
3. Не вводить новые интерфейсы ради самого разделения. Методы могут оставаться методами `*Service` и `*Repository`.
4. В каждом срезе переносить связанный блок целиком: типы, SQL/scan helpers, clone/encode helpers, тесты и локальные private-функции.
5. После каждого среза выполнять `gofmt` и целевые тесты. Финальный `go test ./...` делать только после серии механических переносов.
6. Не смешивать перенос файлов с изменением поведения, схем, контрактов или бизнес-логики.

## Желаемая структура `backend-go/internal/store`

`store.go` должен стать точкой сборки репозитория:

- `Repository` struct;
- `NewRepository`;
- `NewPostgresRepository`;
- `Close`;
- общие низкоуровневые helpers, если они реально используются всеми зонами.

Предлагаемые файлы:

### `records.go`

Общие record-типы, которые сейчас живут в верхней части `store.go`:

- `MessageRecord`;
- `DraftRecord`;
- `ScheduledJobRecord`;
- `InteractionFormRecord`;
- `SemanticFactRecord`;
- `ActiveEpisodeRecord`;
- `EpisodeSummaryRecord`;
- `ExtractionLogRecord`;
- `SendRecord`;
- `SemanticFactUpsertResult`;
- `FactRelationRecord`;
- `AIRoleRecord`.

Причина: эти типы являются API пакета `store`, но не должны раздувать файл с конструктором репозитория.

### `messages.go`

Сообщения, состояния и аудит:

- `SaveMessage`;
- `Message`;
- выборки сообщений по датам/диапазонам;
- `AppendState`;
- `AppendAudit`;
- `Audits`;
- `insertState`;
- связанные helpers для `MessageRecord`.

Причина: это операционный лог диалога, отдельный от настроек чата, ролей и памяти.

### `idempotency.go`

Идемпотентность receive/send/callback операций:

- `MarkCallback`;
- `MarkSend`;
- `markIdempotency`;
- `SendRecord`;
- helpers ключей, если они локальны для этого поведения.

Причина: эти методы используются как cross-cutting guard, но доменно не относятся ни к draft, ни к message storage.

### `drafts.go`

Draft lifecycle:

- создание, чтение и обновление draft-записей;
- `UpdateDraft`;
- `DraftByMessage`;
- `DraftBySentMessageID`;
- `dbDraft`;
- discard/cleanup pending drafts, если они сейчас находятся в `store.go`.

Причина: approval flow в `core` будет проще анализировать, когда все persistence-операции draft лежат рядом.

### `draft_feedback.go`

Feedback по draft-ответам:

- `SaveDraftFeedback`;
- `RecentFeedback`;
- record/helpers для feedback, если они появятся или уже локально связаны.

Причина: feedback логически связан с draft, но используется AI-контекстом отдельно от самого draft lifecycle.

### `chat_settings.go`

Настройки чата:

- `ChatSettings`;
- `SaveChatSettings`;
- privacy flags encode/decode helpers, если они сейчас рядом с settings.

При желании этот срез можно объединить с существующим `chats.go`, но лучше не раздувать `chats.go` сверх роли chat registry.

### `ai_roles.go`

AI-роли и назначения ролей:

- `AIRoleRecord`;
- `AIRoles`;
- `AIRoleByID`;
- `AIRoleByName`;
- `AIRoleForChat`;
- create/update/delete role methods;
- `AssignAIRoleToChat`;
- `UpdateAIRoleToolSettings`;
- `encodeRoleToolSettings`;
- `decodeRoleToolSettings`;
- `cloneRoleToolSettings`;
- custom fields helpers.

Причина: роль - самостоятельный aggregate: настройки генерации, persona, delivery, model и tool settings.

### `user_settings.go`

Owner/user settings:

- `UserSettings`;
- `SaveUserSettings`;
- helpers вторичных языков, timezone/default mode, rich/streaming defaults.

Причина: user settings имеют другой lifecycle, чем chat settings и role settings.

### `profiles.go`

Контактный профиль:

- `ContactProfile`;
- сохранение/обновление профиля;
- profile-field helpers;
- часть `forgetChatMemoryLocked`, если она чистит профильные in-memory зеркала.

Причина: профиль контакта используется и Mini App, и AI context, но это не semantic memory.

### `profile_suggestions.go`

Предложения обновления профиля:

- `ProfileSuggestionRecord`;
- `SaveProfileSuggestion`;
- `ResolveSuggestion`;
- list/read helpers для pending suggestions.

Причина: suggestion lifecycle имеет отдельную state machine и callback/API обработку.

### `semantic_facts.go`

Долгосрочные факты:

- `SemanticFactRecord`;
- `SaveSemanticFact`;
- `UpsertSemanticFact`;
- `SemanticFacts`;
- `SemanticFactsByVector`;
- `SemanticFactsByBM25`;
- `SemanticFactsByIDs`;
- `SemanticFactTextsByIDs`;
- `SimilarFactPairs`;
- `SaveFactRelation`;
- `FactRelationsByFactID`;
- `UnlearnFact`;
- `ConsolidateFacts`;
- `hardIncludeChatFacts`;
- `normalizeFactKey`;
- `invalidateContextScore`;
- `vectorOrderExpr`.

Причина: это отдельный memory subsystem, и он не должен быть перемешан с drafts/settings.

### `episode_summaries.go`

Episode runtime и summaries:

- `ActiveEpisodeRecord`;
- `EpisodeSummaryRecord`;
- `UpsertActiveEpisode`;
- `UpdateEpisodeRuntime`;
- `ClearEpisodeRuntimeState`;
- `ActiveEpisodeByChat`;
- `CloseActiveEpisode`;
- `SaveEpisodeSummary`;
- `EpisodeSummaries`;
- `EpisodeSummariesByVector`;
- `EpisodeSummariesWithoutEmbeddings`;
- `EpisodeSummaryOverlaps`;
- `LatestCoveredEnd`;
- summary scan helpers.

Причина: episode lifecycle связан с summary/embedding workers, но хранится отдельно от фактов.

### `embeddings.go`

DB-операции для embedding backlog:

- `SaveFactEmbedding`;
- `SaveEpisodeSummaryEmbedding`;
- выборки без embeddings, если после переноса видно, что они лучше живут рядом с batch writers.

Решение по этому файлу можно принять после `semantic_facts.go` и `episode_summaries.go`: если файл получается слишком маленьким, оставить embedding methods рядом с соответствующим доменом.

## Желаемая структура `backend-go/internal/core`

`service.go` должен стать точкой сборки `Service`:

- `Service` struct;
- `Option` и `With...` опции;
- `NewService`;
- маленькие общие interfaces/config types;
- `Repository()` accessor.

Предлагаемые файлы:

### `decision.go`

Decision policy:

- `DecisionEngine`;
- `NewDecisionEngine`;
- `Decide`;
- небольшие helpers, которые обслуживают только принятие решения.

Причина: decision policy не зависит от Telegram transport и должна читаться отдельно.

### `webhook.go`

Telegram webhook ingress:

- `EnqueueTelegramWebhook`;
- webhook worker structs/helpers;
- `HandleTelegramWebhook`;
- `telegramUpdateSummary`;
- top-level routing по типу Telegram update.

Причина: входной HTTP/Telegram слой должен быть отделен от AI turn orchestration.

### `bot_ingress.go`

Bot-owned inbound events:

- `HandleBotMessage`;
- `HandleBotCallbackQuery`;
- bot command routing;
- bot-scoped chat/role resolution helpers.

Причина: bot-chat сценарии имеют отдельную модель tenant/bot scope.

### `owner_ingress.go`

Owner-authored messages:

- `handleOwnerMessage`;
- owner command handling;
- `ManualSend`;
- owner-side message detection helpers.

Причина: owner replies/manual sends имеют отдельные правила сохранения и discard draft behavior.

### `ai_turn.go`

Основной AI turn orchestration:

- `processContactMessage`;
- подготовка запроса в AI;
- применение `contracts.AIProcessMessageResponse`;
- сохранение входящих/исходящих событий;
- вызов decision engine;
- маршрутизация в draft/auto-send/question/scheduler branches.

Важно: сначала перенести функцию как есть. Разбивать `processContactMessage` на внутренние шаги стоит отдельным последующим срезом, когда файл уже отделен.

### `ai_context.go`

Сбор контекста для AI:

- role/profile/settings resolution;
- memory context helpers;
- feedback context helpers;
- functions вроде `factIDs`, `finalizeFactOrder`, `feedbackContext`.

Причина: контекстная подготовка меняется часто и не должна теряться внутри transport-кода.

### `media.go`

Media enrichment:

- `MediaConfig`;
- `EnrichBotMedia`;
- media reject/extract helpers;
- in-memory-only media handling.

Причина: media privacy и обработка файлов требуют отдельного места для аудита.

### `delivery.go`

Доставка сообщений:

- `deliverySettings`;
- `deliverySettingsFor`;
- `deliverySettingsFromUser`;
- `botForDeliverySettings`;
- `sendRecipient`;
- `sendRecipientWithMarkup`;
- `sendRichRecipient`;
- `sendStreamingRichRecipient`;
- `sendRichDraftPreview`;
- `sendMessage`;
- rich/plain fallback helpers.

Причина: delivery - отдельная инфраструктурная зона, пересекающая auto-send, scheduler, drafts и manual sends.

### `owner_cards.go`

Карточки и уведомления владельцу:

- `sendOwnerCard`;
- `sendDraftToOwner`;
- `sendWarningToOwner`;
- `sendRecommendationsToOwner`;
- `sendQuestionToOwner`;
- card text/keyboard glue на уровне core.

Причина: owner-facing UX в Telegram должен быть рядом, но отдельно от persistence и AI turn.

### `approvals.go`

Approval lifecycle:

- `HandleApprovalCallback`;
- `removeDraftCard`;
- `stripRatingButtons`;
- `saveOutgoingMessageFromDraft`;
- owner feedback score отправка.

Причина: approval flow крупный и рискованный, поэтому его лучше выделить после `delivery.go` и `owner_cards.go`.

### `interaction_forms.go`

Core-логика анкет/уточняющих вопросов:

- `startInteractionForm`;
- `sendInteractionQuestion`;
- `handleInteractionFormTextAnswer`;
- `handleQFormCallback`;
- `handleChoiceOption`;
- `qformQuestionKeyboard`.

Причина: storage для forms уже отделяется в `store/interaction_forms.go`; core-обвязка должна быть симметричной.

### `scheduler.go`

Scheduled jobs runtime:

- `StartScheduler`;
- `runSchedulerOnce`;
- `dispatchScheduledJob`;
- `scheduledJobEvent`;
- `sendScheduledOwnerNotification`;
- scheduler routing helpers.

Причина: jobs уже имеют отдельный store-файл, core worker тоже должен быть отдельным.

### `summaries.go`

Daily/episode summary workflow:

- `StartDailySummaryScheduler`;
- `StartEpisodeCloser`;
- `runDailySummariesForDate`;
- `summarizeChatDay`;
- `summarizeEpisode`;
- `closeStaleEpisodes`;
- summary marker/cache helpers.

Причина: summary lifecycle связан с memory, но является отдельным background workflow.

### `embeddings.go`

Embedding worker:

- `StartEmbeddingWorker`;
- `nudgeEmbeddingWorker`;
- `runEmbeddingBatch`;
- `runFactEmbeddingBatch`;
- `runEpisodeSummaryEmbeddingBatch`.

Причина: embedding backlog имеет отдельный worker-loop и хорошо отделяется механически.

### `business_connections.go`

Business connection and deleted business messages:

- `handleBusinessConnection`;
- deleted business message handling;
- business connection specific audit/settings updates.

Причина: Telegram Business lifecycle отличается от обычного inbound message flow.

## Порядок выполнения

### Срез 0 - подготовка

1. Зафиксировать текущий `git status --short`.
2. Убедиться, что перенос не смешивается с активной фичей.
3. Снять baseline:
   - `wc -l backend-go/internal/core/service.go backend-go/internal/store/store.go`;
   - `cd backend-go && go test ./internal/store ./internal/core`.

### Срез 1 - безопасные store declarations

1. Создать `backend-go/internal/store/records.go`.
2. Перенести record/result types без изменения полей.
3. Запустить:
   - `gofmt -w backend-go/internal/store/records.go backend-go/internal/store/store.go`;
   - `cd backend-go && go test ./internal/store`.

Почему первым: перенос типов почти не меняет поведение и быстро выявляет import/ordering проблемы.

### Срез 2 - store drafts/messages/idempotency

1. Создать `messages.go`, `idempotency.go`, `drafts.go`, `draft_feedback.go`.
2. Переносить не по алфавиту, а по call cluster:
   - сначала idempotency;
   - затем message/audit state;
   - затем draft lifecycle;
   - затем draft feedback.
3. Проверка:
   - `gofmt -w backend-go/internal/store/*.go`;
   - `cd backend-go && go test ./internal/store`;
   - при затронутых approvals дополнительно `cd backend-go && go test ./internal/core`.

### Срез 3 - store settings and roles

1. Создать `chat_settings.go`, `ai_roles.go`, `user_settings.go`.
2. Перенести role CRUD вместе с role encoding/clone helpers.
3. Не трогать contracts/OpenAPI в рамках этого среза.
4. Проверка:
   - `cd backend-go && go test ./internal/store ./internal/httpapi`.

### Срез 4 - store memory/profile/episodes

1. Создать `profiles.go`, `profile_suggestions.go`, `semantic_facts.go`, `episode_summaries.go`.
2. Внутри memory сначала переносить read-only/query helpers, потом write/upsert/delete helpers.
3. Проверка:
   - `cd backend-go && go test ./internal/store`;
   - `cd backend-go && go test ./internal/core` для summary/memory сценариев.

### Срез 5 - core low-risk workers

1. Создать `scheduler.go`, `summaries.go`, `embeddings.go`.
2. Перенести worker loops и их локальные helpers.
3. Проверка:
   - `cd backend-go && go test ./internal/core -run 'Scheduler|Summary|Episode|Embedding'`;
   - затем `cd backend-go && go test ./internal/core`.

Почему здесь: worker-блоки имеют четкие границы и уже видны по именам методов.

### Срез 6 - core transport and delivery

1. Создать `webhook.go`, `bot_ingress.go`, `owner_ingress.go`, `delivery.go`, `media.go`.
2. Перенести entrypoints и helpers без изменения порядка ветвления.
3. Проверка:
   - `cd backend-go && go test ./internal/core`;
   - `cd backend-go && go test ./internal/telegram ./internal/botservice`.

### Срез 7 - core owner UX and approvals

1. Создать `owner_cards.go`, `approvals.go`, `interaction_forms.go`.
2. Сначала перенести owner card sending, потом approval callback, потом interaction form callbacks.
3. Проверка:
   - `cd backend-go && go test ./internal/core -run 'Approval|Draft|Interaction|Choice|Question'`;
   - `cd backend-go && go test ./internal/core`.

### Срез 8 - core AI turn extraction

1. Создать `ai_context.go` и `ai_turn.go`.
2. Перенести `processContactMessage` как цельный метод в `ai_turn.go`.
3. В `ai_context.go` вынести только уже существующие helpers, без нового дизайна.
4. После стабилизации отдельным будущим срезом можно дробить `processContactMessage` на шаги:
   - load settings/context;
   - prepare AI request;
   - persist AI side effects;
   - decide;
   - deliver or draft.
5. Проверка:
   - `cd backend-go && go test ./internal/core`;
   - `cd backend-go && go test ./...`.

## Что не делать в этом плане

- Не создавать подпакеты `core/delivery`, `store/memory` и т.п. Сейчас ограничение - остаться в тех же пакетах.
- Не менять SQL, migrations, OpenAPI или contracts ради переноса.
- Не переименовывать публичные методы `Service` и `Repository`.
- Не делать массовый refactor `processContactMessage` одновременно с переносом файла.
- Не объединять перенос с feature work, особенно если рабочая копия уже содержит незавершенные изменения.

## Критерии готовности

После всех срезов:

- `backend-go/internal/core/service.go` содержит только сборку `Service`, options, constructor и общие типы;
- `backend-go/internal/store/store.go` содержит только сборку `Repository`, constructors, close и реально общие helpers;
- каждый новый файл описывает одну доменную область;
- `gofmt` выполнен;
- `cd backend-go && go test ./...` проходит;
- итоговый diff не меняет поведение, кроме возможного порядка declarations внутри пакета.

