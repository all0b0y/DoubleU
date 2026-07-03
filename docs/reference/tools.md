# Native AI Tools — справочник

Полное описание встроенных инструментов AI-агента DoubleU: назначение, параметры,
поток данных Go↔Python, privacy-гейтинг и идемпотентность. Источник правды для
каталога — Python `TOOL_CATALOG`; Go-сторона зеркалит его (см. синк-тест ниже).

## Как устроены инструменты

Инструменты регистрируются как `FunctionToolset` в `ai-python/ai_agent/agents/` и
никогда сами не выполняют видимых эффектов (не шлют сообщения, не пишут в БД).
Они лишь **записывают намерение** в `AgentDeps` (`ai-python/ai_agent/agents/deps.py`),
которое возвращается в ответе `/ai/process-message`; всё видимое и персистентное
исполняет Go-бэкенд. Это держит Python stateless (`AI_MEMORY_SOURCE=go`) и
сохраняет владение политиками/идемпотентностью за бэкендом.

Поток на каждый инструмент:

```
LLM → tool (пишет в AgentDeps) → DraftOutcome → AIProcessMessageResponse
   → Go core/service.go (валидация, политика, идемпотентность)
   → store (БД/in-memory) или Telegram
```

### Каталог и включение

- **Каталог:** `ai-python/ai_agent/agents/tool_catalog.py` (`TOOL_CATALOG`,
  `TOOL_NAME_TO_KEY`). Go-зеркало — `backend-go/internal/contracts/tool_catalog.go`.
  Синхронность проверяет `TestToolCatalogMatchesCanonicalPythonCatalog`.
- **Включение по роли:** `AIRole.tool_settings[key]`; отсутствующий ключ наследует
  `default_enabled`. Фильтрация тулсета — `resolve_builtin_toolset`
  (`ai-python/ai_agent/agents/runtime.py`).
- **Privacy-гейтинг:** инструменты категории `schedule` отключаются при
  `no_memory`/`private` ещё на этапе фильтрации тулсета; memory-инструменты
  дополнительно проверяют флаги внутри себя (вторая линия защиты).

| Ключ | Категория | default | Python | Go-потребитель |
|---|---|---|---|---|
| `memory.search_facts` | read | ✅ | `memory_tools.py` | прямой ответ |
| `memory.get_episode_summaries` | read | ✅ | `memory_tools.py` | прямой ответ |
| `memory.get_recent_messages` | read | ✅ | `memory_tools.py` | прямой ответ |
| `memory.get_contact_profile` | read | ✅ | `memory_tools.py` | прямой ответ |
| `memory.save_fact` | read | ✅ | `memory_tools.py` | `semantic_facts` |
| `memory.suggest_profile_update` | read | ✅ | `memory_tools.py` | `contact_profile_suggestions` |
| `interaction.ask_questions` | interaction | ❌ | `interaction_tools.py` | `interaction_forms` + Telegram |
| `scheduler.create_reminder` | schedule | ❌ | `schedule_tools.py` | `scheduled_jobs` |
| `scheduler.list_reminders` | schedule | ❌ | `schedule_tools.py` | request `existing_scheduled_jobs` |
| `scheduler.cancel_reminder` | schedule | ❌ | `schedule_tools.py` | `scheduled_jobs` (CancelJob) |
| `skills.load_skill` | skills | ✅ | `skill_tools.py` | `skills` (читает `body` включённого навыка роли) |

### Применимость (prompt policy)

Источник правды — `ai-python/ai_agent/agents/tool_policy.py` (`ToolPromptPolicy`,
`TOOL_PROMPT_POLICIES`). Это отдельный от каталога слой: каталог короткий и
UI/Go-видимый, policy — расширенная применимость для оркестратора/писателя,
покрытая тестом coverage (`test_tool_policy.py`). `orchestrator_spec`/`draft_spec`
(`ai-python/ai_agent/prompts/library.py`) рендерят её в `tool_policy` только для
инструментов, реально включённых в этот ход (`enabled_tool_names(...)`), плюс
сами докстринги тулов (ниже) несут то же самое в JSON-схему вызова инструмента —
это видит каждая модель независимо от prompt-shape семейства.

| Инструмент | Применять когда | НЕ применять когда | Side effect | Пустой/error результат |
|---|---|---|---|---|
| `search_facts` | нужны детали о собеседнике/договорённостях не из видимого контекста | вопрос про общие знания, не про этот чат; факт уже в контексте | `none` | `[]` — фактов нет/память недоступна, не повод выдумывать |
| `get_episode_summaries` | нужен контекст более ранних эпизодов | видимого контекста уже достаточно | `none` | `[]` — эпизодов нет/память недоступна |
| `get_recent_messages` | видимого контекста не хватает для хода разговора | первое сообщение при достаточном контексте; история уже видна | `none` | `[]` — сообщений нет/память недоступна |
| `get_contact_profile` | в инструкции нет данных о собеседнике, важных для тона | профиль уже явно есть в trusted-контексте | `none` | `{}` — профиль не заполнен/`no_memory`/нет пула |
| `save_fact` | прозвучал устойчивый факт, которого нет в памяти | факт сиюминутный; уже есть в контексте | `staged_memory_write` | текстовый статус (`saved (...)` / ошибка), не structured bool |
| `suggest_profile_update` | сообщение явно меняет атрибут идентичности собеседника | атрибут не изменился; информация неявная | `staged_memory_write` | `staged for owner review` / `already pending` — профиль не меняется этим вызовом |
| `ask_questions` | нужен вопрос с выбором перед ответом (уточнение/опрос/квиз) | вопрос без выбора; форма этого хода уже задана | `staged_interaction` | подтверждение постановки — форма уходит ПОСЛЕ хода |
| `request_confirmation` | хватает одного да/нет вопроса | нужно больше вопросов/вариантов — используй `ask_questions` | `staged_interaction` | как у `ask_questions` |
| `create_reminder` | явно просят поставить напоминание на конкретное будущее время | время уже прошло и перенос неоднозначен — сначала уточни | `staged_schedule` | `scheduled reminder for <iso>` / `error: ...` |
| `list_reminders` | нужно сослаться на напоминание или узнать id перед отменой | роль не включает list_reminders — придёт пусто независимо от данных | `none` | `Активных напоминаний нет.` — действительно нет pending |
| `cancel_reminder` | id известен (из `list_reminders`) и явно просят отменить | id не известен — не угадывай номер | `staged_schedule` | `error: no pending reminder with id N` — неверный/чужой id |
| `send_message` | в одном ходе нужно разделить РАЗНЫЕ по характеру части на отдельные сообщения | ответ — одна цельная реплика; вопрос с кнопками (это `ask_questions`) | `staged_delivery` | `ModelRetry` при пустом тексте/лимите — не тихий провал |
| `load_skill` | в промпте перечислен навык, релевантный ходу, и нужна его процедура/детали | ни один навык из списка не относится к вопросу — не выдумывай навык | `none` | `<skill>…</skill>` с телом; `навык не найден` — навыка нет в списке роли |

---

## Memory tools (`memory.*`)

Файл: `ai-python/ai_agent/agents/memory_tools.py`. Все включены по умолчанию,
категория `read`. Читают/пишут память чата напрямую в Postgres, когда есть пул;
при stateless-режиме деградируют в пустой результат / no-op. Privacy `no_memory`
блокирует чтение и запись внутри каждого инструмента.

- `search_facts(query)` — семантический/лексический поиск сохранённых фактов.
- `get_episode_summaries()` — сводки прошлых эпизодов.
- `get_recent_messages()` — последние сообщения чата.
- `get_contact_profile()` — сохранённые имя/тон/обращение.
- `save_fact(...)` — устойчивый факт в `semantic_facts`.
- `suggest_profile_update(...)` — предложение владельцу обновить профиль.

---

## Interaction tool — анкета (`interaction.ask_questions`)

Файлы: `ai-python/ai_agent/agents/interaction_tools.py`,
`backend-go/internal/core/service.go` (форма + callback),
`backend-go/internal/store/interaction_forms.go` (состояние).

**Назначение.** Собрать у собеседника несколько уточнений ПЕРЕД ответом. Агент
готовит пул из 1–6 вопросов; бот задаёт их **по одному**. На каждый вопрос
собеседник отвечает либо нажатием кнопки-подсказки, либо своими словами (есть
кнопка «✍️ Ответить своими словами»). Ответы копятся; только после ответа на ВСЕ
вопросы Q&A уходит в ИИ как одно сообщение, и формируется финальный ответ.

**Инструменты.**
- `ask_questions(questions: list[{prompt, options[0..8]}])` — записывает
  `ResponsePlan(shape="questions", questions=...)`. `options` опциональны:
  пустой список → чисто текстовый вопрос. Лимиты: ≤6 вопросов, ≤8 уникальных
  опций на вопрос.
- `request_confirmation(prompt)` — частный случай: анкета из одного да/нет-вопроса.

**Контракт.** `ResponsePlan{shape, questions[{prompt, options}]}` в
`AIProcessMessageResponse.response_plan` (Go: `contracts.ResponsePlan`/`Question`,
Python wire: `ResponsePlanData`/`QuestionData`).

**Поток в Go (`service.go`).**
1. `contactQuestionnaire` валидирует форму и проверяет, что инструмент включён и
   транспорт — бот (`contactInteractionEnabled`).
2. `startInteractionForm` создаёт запись `interaction_forms` (один активный form
   на чат) и шлёт ПЕРВЫЙ вопрос с клавиатурой `questionnaireKeyboard`
   (кнопки-опции + кнопка «своими словами»). Обычный ответ при этом НЕ шлётся.
3. Ответ кнопкой → callback `qf:{formID}:{qIdx}:{optIdx|t}`
   (`CallbackDataEncodeQForm`/`DecodeQForm`) → `handleQFormCallback`. Ответ
   текстом → перехват в начале `processContactMessage` → `advanceForm`.
4. `advanceForm` записывает ответ (`RecordFormAnswer`), задаёт следующий вопрос
   или, когда пул исчерпан, `CompleteForm` и запускает обычный ход
   `processContactMessage` с собранным Q&A в качестве сообщения — тот персистит
   сообщение в память и формирует ответ ИИ.

**Состояние (`interaction_forms.go`, миграция `116_interaction_forms.sql`).**
`CreateInteractionForm` (вытесняет прежний активный form), `ActiveInteractionForm`,
`RecordFormAnswer`, `CompleteForm`, `CancelForm`. TTL анкеты — 24 часа.

**Идемпотентность.** Кнопка: `MarkCallback(qform:{form}:{q}:{slot})` — повтор
нажатия не двигает форму. Текст-ответ: `MarkReceive` по update_id. Завершающий
ход получает синтетический отрицательный `telegram_update_id`, чтобы его
receive-ключ не сталкивался с ответом-триггером.

**Privacy.** Инструмент доступен только для бот-транспорта и явно включённой роли.

---

## Scheduler tools — напоминания (`scheduler.*`)

Файлы: `ai-python/ai_agent/agents/schedule_tools.py`,
`backend-go/internal/core/service.go`,
`backend-go/internal/store/scheduled_jobs.go` (миграция `114_scheduled_jobs.sql`).
Все выключены по умолчанию, категория `schedule`, гейтятся `no_memory`/`private`.

**Инструменты.**
- `create_reminder(fire_at_iso, text, target)` — будущее напоминание (≤1 года).
  Записывает `ScheduledJob` в `AgentDeps.scheduled_jobs`. Go (`saveAIScheduledJobs`)
  персистит в `scheduled_jobs`. Идемпотентность — SHA-256 ключ от
  (tenant, chat, source_message, fire_at, target, text).
- `list_reminders()` — показывает активные напоминания чата. Читает
  `AgentDeps.existing_jobs` (БД не трогает).
- `cancel_reminder(reminder_id)` — записывает id в `AgentDeps.cancelled_job_ids`;
  валидирует, что id есть среди pending. Go применяет `CancelJob` в scope чата.

**Контракт.**
- Запрос: `AIProcessMessageRequest.existing_scheduled_jobs`
  (`ExistingScheduledJob{id, kind, target, fire_at, text}`) — Go заполняет из
  `repository.ListJobs`, отфильтровав `pending`, и **только когда** роль включает
  `list_reminders`/`cancel_reminder` (create-only кейс не читает БД).
- Ответ: `AIProcessMessageResponse.scheduled_jobs` (создание) и
  `cancelled_job_ids` (отмена).

**Go-потребление.** `saveAIScheduledJobs` (создание) и `cancelAIScheduledJobs`
(отмена) в `service.go`; диспетчер due-задач — `scheduler` worker. Хранилище:
`scheduled_jobs.go` (`InsertScheduledJob`, `ListJobs`, `CancelJob`, `DueJobs`,
`ClaimScheduledJob`).

---

## Agent-only — разбиение хода (`send_message`)

`send_message(text)` (`ai-python/ai_agent/agents/delivery_tools.py`) — внутренний
инструмент **только для агента-писателя**, всегда доступен (НЕ в `TOOL_CATALOG`, не
в `tool_settings`, не зеркалится в Go, не виден в UI). Позволяет писателю разбить
один ход на несколько сообщений: каждый вызов кладёт упорядоченное сообщение в
`AgentDeps.outbound_messages`; в ответе они едут как `messages[]`. Go доставляет их
**по очереди ПЕРЕД** финальным `response_text`/`response_plan`
(`service.go::sendLeadingMessages`, своя idempotency-метка на сообщение). Сценарий:
лекция через `send_message`, затем вопрос через `ask_questions` (кнопки) — «текст,
потом вопрос» физически приходит двумя сообщениями. Лимит — `_MAX_MESSAGES`, кривой
вызов → `ModelRetry` (не рушит ход).

## MCP-тулсеты (внешние инструменты роли)

Роль может объявить MCP-серверы (`AIRole.mcp_servers`). `runtime._resolve_mcp_toolsets`
оборачивает каждый в `FilteredToolset` с allow-list, и полностью отключает их при
`no_memory`/`private` (внешний инструмент мог бы утащить содержимое чата). Это не
часть `TOOL_CATALOG` — это per-role расширение.

**Control plane (Г1):** owner CRUD через `/api/v1/mcp-servers` (`miniapp_mcp_handlers.go`);
`auth_header` шифруется at rest (AES-256-GCM, ключ `MCP_SECRETS_KEY`, `store.go`);
URL проходит SSRF-гард (`mcp_url_guard.go`: только https, запрет private/loopback/link-local
и docker-хостов); результаты каждого MCP-инструмента оборачиваются в
`<untrusted_tool_result>` (`sanitize_toolset.py`). Проверка сервера и листинг инструментов —
`POST /ai/mcp-check` (Python) через прокси `apiCheckMCPServer` (Go). Имена подключённых
серверов пишутся в run-trace (`_record_run`, только name).

## Навыки роли (`skills.load_skill`, Г4)

Skill = owner-authored запись в `skills` (`tenant_id NULL` = глобальный first-party,
виден всем; привязка к роли — `role_skills`). Progressive disclosure: **уровень 1**
(name+description) идёт в `tool_policy` промпта writer'а из `ai_role.skills`; **уровень 2**
(тело) писатель тянет `load_skill` — но только для навыка из `deps.enabled_skill_names`
(включённого у роли), иначе «навык не найден». Writer-only (оркестратор навыки не видит,
как `delivery_toolset`), обёрнут `DedupToolset`, **не** гейтится `no_memory`/`private`
(это контент владельца, как `role.context`, а не память чата и не внешний вызов).
Исполняемых скриптов нет (осознанное ограничение фазы). Owner CRUD — `/api/v1/skills`
(`miniapp_skills_handlers.go`); импорт из URL — вторая итерация (см. `docs/backlog.md`).

---

## Где что менять

- Новый встроенный инструмент: добавить тул в соответствующий `*_tools.py`
  (докстринг сразу пишется с «Используй когда / НЕ используй когда» — это
  единственное, что видит модель в JSON-схеме вызова), запись в `TOOL_CATALOG` +
  `TOOL_NAME_TO_KEY` (`tool_catalog.py`), запись в `TOOL_PROMPT_POLICIES`
  (`tool_policy.py`, тест `test_tool_policy.py` проверяет coverage), зеркало в Go
  `tool_catalog.go`, при необходимости — поля контракта в `schemas.py` +
  `contracts/types.go` + `openapi.yaml` и бамп `contracts/contracts.json`
  (затем `python3 scripts/generate_contracts.py`). `ToolPromptPolicy` сам в
  Go/контракт не идёт — это чисто prompt-facing слой.
- Поток исполнения и персистентность инструмента — всегда на Go-стороне
  (`service.go` + `store/`), Python остаётся stateless.
