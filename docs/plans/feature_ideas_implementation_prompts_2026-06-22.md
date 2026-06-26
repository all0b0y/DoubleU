# Tools-платформа + двумодальное исполнение — планы и промты для ChatGPT 5.5 (v2)

> Дата: 2026-06-22 (ревизия 2 — переосмысление вокруг Tools как платформы).
> Исполнитель: ChatGPT 5.5. **Каждый раздел = ОТДЕЛЬНЫЙ коммит.**

## 0. Переосмысление (главное)

**TOOLS — это не 6 отдельных фич, а ПЛАТФОРМА.** Всё остальное строится на ней:
- Единый реестр нативных инструментов (память, «спросить/подтвердить кнопками», «cron-напоминание»,
  «вызвать вебхук») с per-role вкл/выкл и UI. Это **не только MCP** — это первоклассные внутренние tools.
- ИИ **сам** строит действия через tools: сам ставит cron, сам шлёт кнопки и получает ответ, сам просит
  подтверждение, сам задаёт вопрос и принимает ответ.

**«Двумодальность» — следствие tools, а не отдельная фича.** Когда у роли включён хотя бы один инструмент,
ход раздваивается:
- **Оркестратор** — *дешёвая модель-константа* (`MODEL_ROUTER`, по умолчанию `MODEL_CHEAP`). Понимает, какие
  инструменты нужны, **выполняет их**, собирает контекст/результаты и передаёт дальше.
- **Писатель** — *модель, выбранная владельцем роли* (`model_tier`). Инструментов не имеет, только пишет
  финальный ответ из переданного контекста.
- Нет включённых тулзов → один вызов (текущий путь без изменений).

### Зафиксированные решения (из обсуждения)
1. **Права инструментов:** оркестратор выполняет ВСЕ инструменты (чтение + действия). Дешёвая модель —
   константа; дорогая — выбор владельца.
2. **Аппрув не меняется:** владелец оценивает только финальный ответ; в manual/semi_auto получает черновик с
   готовым ответом. Использование инструментов он не гейтит (внутренняя механика хода).
3. **Модели:** глобальный `MODEL_ROUTER` (оркестратор) + `model_tier` роли (писатель).
4. **Удвоение вызова — только при включённых тулзах** у роли.

---

## 1. Таксономия инструментов (ключевой принцип примирения с аппрувом)

Чтобы «ИИ действует сам» не сломало модель аппрува, инструменты делятся на 4 категории по **видимому
наружу** эффекту. Поле `category` в каталоге определяет, как именно эффект применяется:

| Категория | Примеры | Когда исполняется | Отношение к аппруву |
|---|---|---|---|
| **read** | `memory.*` | Немедленно в стадии оркестратора | Не виден владельцу, всегда (privacy-gated) |
| **schedule** | `scheduler.create_reminder` | Немедленно (оркестратор), эффект отложен | Создаётся автономно; **видим и отменяем** владельцем; контакт-фейсинг срабатывание уважает режим чата в момент `fire_at` |
| **integration** | `http.request` (вебхук), MCP | Немедленно (оркестратор) | Только allow-list + SSRF; для контакт-видимых эффектов — gate (см. ниже) |
| **interaction** | `interaction.ask_contact` / `present_buttons` / `request_confirmation` | **Отложенно**: формирует ФОРМУ ответа (не шлёт сразу) | Становится черновиком → проходит обычный путь decision→approval→send. В manual/semi владелец одобряет, в auto — летит |

**Главный инвариант:** всё, что **видит собеседник**, — это «ответ», и он всегда проходит существующий
путь decision→approval→send (UX аппрува не меняется). Внутренняя механика (чтение памяти, постановка
приватного cron, fetch по вебхуку) — автономна и владельцем не гейтится, но `schedule`-эффекты видимы/отменяемы.

---

## 2. Что уже есть (grounding)

| Подсистема | Факт |
|---|---|
| **Tools** | Есть `memory_toolset` (6 инструментов, `ai-python/ai_agent/agents/memory_tools.py`) и MCP per-role (`mcp_servers`+`role_mcp_access`, миграция 105, `runtime.py::_resolve_mcp_toolsets` через `FilteredToolset`). Нет: реестра/каталога, per-role вкл/выкл встроенных, UI. |
| **Модели** | `agents/models.py`: `AgentTask`, `ModelTier` (fast/balanced/smart), `build_model`/`build_tier_model`, env `MODEL_DRAFT`/`MODEL_CHEAP`/`MODEL_TIER_*`. Роль несёт `model_tier`. Есть `GET /ai/model-config`. |
| **Двухстадийность** | Нет. Один draft-агент гоняет ReAct с инструментами, затем критик (`_review_draft`). |
| **Голос→текст** | Есть `POST /ai/extract-media` (Whisper/OCR), `aiclient.ExtractMedia`. |
| **Inline-кнопки** | Транспорт **готов** (`telegram.go`: `InlineKeyboardMarkup`, `AnswerCallbackQuery`, `EditMessageText`, `ParseCallbackQueryUpdate`; `callback_query` в `allowed_updates`). Роутинг — `core/service.go::handleCallbackQuery`. Есть `choice_question`+`options` (карточка владельцу). |
| **Cron** | Только `core/service.go::StartDailySummaryScheduler`. Общего планировщика нет. |
| **Контракт** | Заморожен. Go `contracts/types.go` ↔ Python `schemas.py`; enum-ы — `contracts/contracts.json`+`make contracts`. |
| **Тесты** | `make test`. Старшая миграция — `112_quarantine` (следующая свободная — 113). |

---

## 3. Обязательные правила проекта (вшиты в каждый промт)

1. Контракт меняем **синхронно** Go+Python (+контракт-тест); enum-ы только через `make contracts`.
2. Privacy-флаги (`no_memory`/`no_training`/`no_summary`/`private`) уважать на каждом слое.
3. Идемпотентность отправок/колбэков/вебхуков/отложенных заданий.
4. Недоверенный ввод — через `PromptBuilder.build(...)` перед LLM.
5. Go: `slog`, без гонок, ошибки обрабатывать/`_` с комментом. Python: type-аннотации, `@dataclass(frozen=True)`, без голого `except`.
6. Миграции: следующий свободный номер (сейчас 112 — старший), идемпотентные, с `.down.sql`.
7. Тесты функциональные, реальная БД в интеграционных, детерминизм через `MOCK_LLM=true`; `make test` зелёный.
8. Техдолг/неэффективности — в `docs/backlog.md`.
9. **Один коммит на фичу**, Conventional Commits.

---

## 4. Порядок коммитов (спина = tools → двумодальность → инструменты-действия)

```
Commit 1: Tools-платформа        (реестр+каталог+per-role toggle+UI; память — первые записи каталога)
Commit 2: Двумодальное исполнение (оркестратор MODEL_ROUTER + писатель model_tier; gate: тулзы включены)
Commit 3: interaction-инструмент  (спросить/подтвердить кнопками собеседника; + choice_question как tool)
Commit 4: scheduler-инструмент    (cron/напоминания; ИИ строит сам; worker)
Commit 5: integration-инструмент  (исходящие вебхуки; allow-list+SSRF) — второстепенно
Commit 6: Голосовая роль          (НЕЗАВИСИМО, в любой момент)
```
1→2 — фундамент. 3/4/5 просто регистрируют свой `category`-инструмент в каталоге из commit 1 и используются
оркестратором из commit 2. Commit 6 ни от чего не зависит.

---

## 4.1 MASTER-ПРОМТ — единый контекст-бриф для ChatGPT 5.5 (дать ПЕРВЫМ)

Включает все доработки из обсуждения. Дать ChatGPT 5.5 один раз как контекст, затем подавать промты по коммитам.

```
Ты — senior fullstack-инженер на проекте DoubleU (Go backend + Python AI-сервис на pydantic-ai + vanilla-JS
Telegram Mini App). Это КОНТЕКСТ-БРИФ. Прочитай целиком, затем выполняй задачи ПО ОДНОЙ, каждую — ОТДЕЛЬНЫМ
коммитом (промт на каждый коммит дам отдельно). Ничего вне границ текущего коммита не трогай.

ВИДЕНИЕ (главное):
- TOOLS — это ПЛАТФОРМА, а не набор фич. Единый реестр нативных инструментов (НЕ только MCP) с категориями и
  включением/выключением на уровне роли. На этой платформе строится всё: память, кнопки-вопросы собеседнику,
  cron-напоминания, вебхуки.
- ДВУМОДАЛЬНОСТЬ — следствие tools. Когда у роли включён ≥1 инструмент, ход раздваивается:
  • Оркестратор — дешёвая модель-КОНСТАНТА (env MODEL_ROUTER, default MODEL_CHEAP): понимает какие инструменты
    нужны, ВЫПОЛНЯЕТ их, собирает контекст; финальный ответ НЕ пишет.
  • Писатель — модель, ВЫБРАННАЯ владельцем роли (model_tier): пишет финал из контекста, БЕЗ инструментов.
  Нет включённых инструментов → один вызов (текущий путь без изменений). Удвоение вызова — ТОЛЬКО при включённых тулзах.

ЗАФИКСИРОВАННЫЕ РЕШЕНИЯ:
1) Оркестратор выполняет ВСЕ инструменты (чтение + действия). Дешёвая модель — константа; дорогая — выбор владельца.
2) Аппрув НЕ меняется: владелец оценивает только финальный ответ; в manual/semi_auto получает черновик с готовым
   ответом; использование инструментов он не гейтит.
3) Модели: глобальный MODEL_ROUTER (оркестратор) + model_tier роли (писатель).

ТАКСОНОМИЯ ИНСТРУМЕНТОВ (поле category в каталоге; примиряет «ИИ действует сам» с аппрувом):
- read (memory.*): сразу, невидимо, privacy-gated.
- schedule (scheduler.create_reminder): сразу, эффект отложен; видим и отменяем владельцем; контакт-срабатывание
  уважает режим чата в момент fire_at.
- integration (http.request, MCP): сразу; allow-list + SSRF.
- interaction (кнопки/вопрос/подтверждение собеседнику): ОТЛОЖЕННО — формирует ФОРМУ ответа, идёт обычным путём
  decision→approval→send; кнопки собеседнику только Transport==bot.
ИНВАРИАНТ: всё, что ВИДИТ собеседник, = «ответ» и всегда идёт через approval→send (UX аппрува не меняется).
Внутренняя механика (чтение памяти, приватный cron, fetch по вебхуку) — автономна.

ЧТО УЖЕ ЕСТЬ (НЕ дублируй, переиспользуй):
- memory_toolset — ai-python/ai_agent/agents/memory_tools.py (FunctionToolset).
- MCP per-role через FilteredToolset — ai-python/ai_agent/agents/runtime.py::_resolve_mcp_toolsets.
- per-task/per-tier модели — ai-python/ai_agent/agents/models.py (AgentTask/ModelTier/build_model/build_tier_model).
- Транспорт inline-кнопок и роутинг колбэков — backend-go/internal/telegram/telegram.go и
  backend-go/internal/core/service.go::handleCallbackQuery; есть choice_question+options (карточка ВЛАДЕЛЬЦУ).
- Транскрипция — POST /ai/extract-media (+ aiclient.ExtractMedia). Контракт ЗАМОРОЖЕН: Go contracts/types.go ↔
  Python schemas.py; enum — только через `make contracts`. Планировщик — только ручной StartDailySummaryScheduler.

ОБЯЗАТЕЛЬНЫЕ ПРАВИЛА (каждый коммит):
- Контракт синхронно Go+Python (+контракт-тест); enum через `make contracts`. Privacy-флаги на каждом слое.
  Идемпотентность отправок/колбэков/заданий/вебхуков. Недоверенный ввод через PromptBuilder.
- Go: slog, без гонок. Python: type-аннотации, frozen dataclass, без голого except.
- Миграции: следующий свободный номер (старший — 112), идемпотентные, с .down. Тесты функциональные,
  MOCK_LLM=true; `make test` зелёный. Техдолг — docs/backlog.md. РОВНО ОДИН коммит на фичу, conventional-сообщение.

ПОРЯДОК (промт на каждый дам отдельно):
1) tools-платформа: реестр+каталог (category) + per-role tool_settings (jsonb); UI инструментов — ОТДЕЛЬНЫЙ экран
   /roles/:id/tools по кнопке «Инструменты» внизу формы роли (не внутри формы).
2) двумодальность: оркестратор+писатель; ДВА системных промпта — новый _ORCHESTRATOR_INSTRUCTIONS и обновлённый
   build_draft_prompt (инжект бандла + «инструментов нет»); gate: тулзы включены; флаг TWO_STAGE_DRAFT.
3) interaction-инструмент: кнопки/вопрос собеседнику; aibtn-колбэки; только bot-mode.
4) scheduler-инструмент: cron; ИИ строит сам; worker + таблица scheduled_jobs.
5) integration-инструмент: исходящие вебхуки; allow-list+SSRF — второстепенно.
6) голосовая роль: POST /ai/draft-role; независимо.

Жди промт конкретного коммита и выполняй только его.
```

---

## 5. COMMIT 1 — Tools-платформа (реестр + per-role вкл/выкл + UI)

### План
1. **Каталог инструментов** — единый источник правды. Запись: `key, label, description, category
   (read|schedule|integration|interaction), default_enabled`. Встроенные `memory.*` → `category=read`,
   `default_enabled=true`. Мапа `tool_name→key`. Новый `ai-python/ai_agent/agents/tool_catalog.py` + зеркало
   на Go (константа) + контракт-тест на совпадение ключей.
2. **Хранилище:** миграция `ALTER TABLE ai_roles ADD COLUMN IF NOT EXISTS tool_settings jsonb NOT NULL
   DEFAULT '{}'::jsonb;` (+`.down`). Семантика: отсутствующий ключ = `default_enabled`.
3. **Контракт:** `AIRole.ToolSettings map[string]bool` (Go) + `AIRoleData.tool_settings` (Python frozen);
   `store.AIRoleRecord.ToolSettings` (encode/decode jsonb по образцу `encodeRoleCustomFields`).
4. **API:** `roleJSON`/`roleRequest` (`miniapp_roles_handlers.go`) + валидация ключей по каталогу.
5. **ИИ:** хелпер `resolve_builtin_toolset(ai_role)` → `FilteredToolset` над `memory_toolset` по
   `tool_settings` (как уже сделано для MCP). Privacy-гейты внутри инструментов не трогать.
6. **Каталог-эндпоинт:** `GET /api/v1/tool-catalog` (Go отдаёт каталог) для UI.
7. **UI — ОТДЕЛЬНЫЙ экран инструментов (не внутри формы роли):**
   - В самом низу `role-edit.js` — кнопка «Инструменты» (рядом/под «Сохранить»).
   - Новый экран `miniapp/screens/role-tools.js` + маршрут `/roles/:id/tools` в `app.js`. Экран грузит
     `GET /api/v1/tool-catalog` + текущее `role.tool_settings`, рендерит список доступных инструментов,
     **сгруппированных по `category`**, с тогглами (паттерн `speakAsSection`); кнопка «назад» → в форму роли.
   - Сохранение — отдельный `PUT /api/v1/roles/{role_id}/tools {tool_settings}` (не пересылаем всю роль).
   - **Новая (несохранённая) роль:** кнопка сперва сохраняет роль (если форма валидна — имя+описание), затем
     открывает экран инструментов; иначе подсказка «сначала заполните имя и описание и сохраните роль».
   - Методы `toolCatalog()` и `saveRoleTools(roleId, toolSettings)` в `miniapp/api/client.js`.

### Критерии приёмки
Роль без `tool_settings` = прежнее поведение (все memory доступны). Отключённый инструмент отсутствует в
`FilteredToolset` (тест). Каталоги Go/Python совпадают (контракт-тест). Кнопка «Инструменты» внизу формы открывает
ОТДЕЛЬНЫЙ экран-список; тогглы сохраняются через `PUT .../tools` и читаются обратно. `make test` зелёный.

### ПРОМТ
```
Ты — fullstack-инженер в проекте DoubleU (Go backend + Python AI на pydantic-ai + vanilla-JS Mini App).
ЗАДАЧА (ОДИН коммит): построить TOOLS-ПЛАТФОРМУ — реестр инструментов с категориями, вкл/выкл на уровне роли,
бэкенд и UI. Это фундамент, на котором дальше строятся inline-кнопки, cron и вебхуки как инструменты.

Что уже есть:
- Встроенные инструменты памяти: ai-python/ai_agent/agents/memory_tools.py (FunctionToolset, 6 инструментов).
- MCP уже фильтруется per-role через FilteredToolset в ai-python/ai_agent/agents/runtime.py
  (_resolve_mcp_toolsets, allowed_tools) — СКОПИРУЙ этот паттерн для встроенных инструментов.
- Роль: Go backend-go/internal/contracts/types.go (AIRole), Python ai-python/ai_agent/schemas.py (AIRoleData),
  store backend-go/internal/store/store.go (AIRoleRecord, encode/decodeRoleCustomFields),
  API backend-go/internal/httpapi/miniapp_roles_handlers.go (roleJSON/roleRequest),
  UI miniapp/screens/role-edit.js (паттерны speakAsSection() и deliverySettingsHTML()).

Сделай:
1) Каталог инструментов — единый источник правды. Запись: key (напр. "memory.search_facts"), label, description,
   category ∈ {read, schedule, integration, interaction}, default_enabled. Встроенные memory.* → category=read,
   default_enabled=true. Заведи мапу tool_name→key. Положи каталог в новый ai-python/ai_agent/agents/tool_catalog.py
   и его зеркало на Go (константа), + контракт-тест на совпадение списков ключей и категорий.
2) Миграция (следующий свободный номер; сейчас старший 112; +.down): ALTER TABLE ai_roles ADD COLUMN IF NOT EXISTS
   tool_settings jsonb NOT NULL DEFAULT '{}'::jsonb. Семантика: отсутствующий ключ = default_enabled инструмента.
3) Контракт синхронно: AIRole.ToolSettings map[string]bool (Go) и AIRoleData.tool_settings (Python frozen dataclass).
   store.AIRoleRecord.ToolSettings + encode/decode jsonb по образцу custom_fields.
4) API: roleJSON/roleRequest получают tool_settings; валидируй ключи по каталогу (неизвестные → 400).
5) ИИ: хелпер resolve_builtin_toolset(ai_role) → FilteredToolset над memory_toolset по tool_settings (отсутствующий
   ключ → default_enabled). Используй его в runtime.py там же, где сейчас memory_toolset. Privacy-гейты не трогай.
6) Mini App — управление инструментами на ОТДЕЛЬНОМ экране (НЕ внутри формы роли):
   - Go: GET /api/v1/tool-catalog (отдаёт каталог) и PUT /api/v1/roles/{role_id}/tools {tool_settings} (сохраняет
     ТОЛЬКО tool_settings роли) — маршруты в backend-go/internal/httpapi/server.go, хендлеры в miniapp_roles_handlers.go.
   - В самом низу miniapp/screens/role-edit.js добавь кнопку «Инструменты» (рядом с «Сохранить»). Новый экран
     miniapp/screens/role-tools.js + маршрут '/roles/:id/tools' в miniapp/app.js: грузит каталог + role.tool_settings,
     рендерит список инструментов, СГРУППИРОВАННЫХ ПО category, с тогглами (паттерн speakAsSection); кнопка «назад» → в форму роли.
   - Новая (несохранённая) роль: кнопка «Инструменты» сперва сохраняет роль (если форма валидна), затем открывает экран
     инструментов; если форма невалидна — подсказка «сначала заполните имя и описание и сохраните роль».
   - Методы toolCatalog() и saveRoleTools(roleId, toolSettings) в miniapp/api/client.js.

Правила: контракт синхронно обе стороны (+контракт-тест); enum-ы через `make contracts`; privacy уважать; Go — slog;
Python — type-аннотации, frozen dataclass, без голого except; миграция идемпотентна и с .down; техдолг — docs/backlog.md.

Тесты: роль без tool_settings = прежнее поведение; отключённый инструмент отсутствует в FilteredToolset (агент не
вызовет); round-trip store; валидация API (вкл. PUT .../tools); совпадение каталогов Go/Python. `make test` зелёный.

Коммить ОДНИМ коммитом: feat(tools): per-role tool registry + enablement (platform). Только это в коммите.
```

---

## 6. COMMIT 2 — Двумодальное исполнение (оркестратор + писатель)

### План
1. **Модель оркестратора (константа):** `AgentTask.ORCHESTRATE` → env `MODEL_ROUTER` (default `MODEL_CHEAP`).
2. **`OrchestratorResult` (frozen, `results.py`):** `gathered_context: tuple[str,...]`, `suggested_intent: str`,
   `notes: str`. (Поле под форму ответа добавит commit 3 — оставь точку расширения.)
3. **Промпты — ДВА разных инструктажа (это и есть ядро двумодальности):**
   - **Оркестратору — НОВЫЙ системный промпт.** Константа `_ORCHESTRATOR_INSTRUCTIONS` (по образцу
     `_CRITIC_INSTRUCTIONS` в `runtime.py`) + хелпер `build_orchestrator_prompt(request, prompt_builder)` в
     `pipelines/draft.py`: «пойми последнее сообщение и роль; реши, какие из ДОСТУПНЫХ инструментов нужны; вызови
     их и собери факты/контекст (и выполни разрешённые действия); верни `OrchestratorResult`. **НЕ пиши финальный
     ответ.** Экономь вызовы (бюджет).» Даём урезанный персона-контекст (роль/цели/что важно), без имперсонации и
     без правил формата ответа (это забота писателя).
   - **Писателю — ОБНОВИТЬ существующий промпт** (`build_draft_prompt`/`_compose_base_instruction`): в двухстадийном
     режиме (а) инжектить бандл оркестратора (`gathered_context`) в контекстный блок; (б) добавить директиву
     «контекст уже собран — опирайся на него; в этом ходе инструментов НЕТ; не делай вид, что ищешь».
     Одностадийный режим (нет тулзов) — промпт без изменений.
   - **Докстринги инструментов** (`memory_tools.py` «Используй, когда…») — это промпт ВЫБОРА инструментов; теперь
     их читает дешёвый оркестратор. Оставить модель-агностичными.
4. **Оркестратор-агент (`runtime.py`):** модель `ORCHESTRATE`, `instructions=_ORCHESTRATOR_INSTRUCTIONS`, владеет
   **включённым** тулсетом роли (`resolve_builtin_toolset` из commit 1 + MCP),
   `UsageLimits(tool_calls_limit=_DRAFT_TOOL_CALLS_LIMIT)`. Выполняет инструменты, отдаёт `OrchestratorResult`.
5. **Писатель = текущий draft-агент**, но при двумодальном пути запускается с `toolsets=[]` на
   **модели роли** (`build_tier_model(role.model_tier)` либо DRAFT-модель). user-payload = сообщение + бандл
   (через обновлённый `build_draft_prompt`). Критик (`_review_draft`) — стадия 3 без изменений.
6. **Гейт:** двумодальный путь тогда и только тогда, когда `(флаг включён)` И `(у роли ≥1 включённый
   инструмент)`. Иначе — текущий одностадийный `_draft_async` без изменений. Флаг `TWO_STAGE_DRAFT`
   (env, default off для раскатки) — глобальный kill-switch.
7. **Наблюдаемость:** обе стадии через `_record_run`; `model_used = "orchestrate(<router>)+draft(<tier>)"`
   (+`"+critic"`).

### Критерии приёмки
Роль без включённых тулзов ИЛИ флаг off → один вызов, существующие тесты зелёные, путь не изменён. Роль с тулзами
и флагом on → оркестратор на `MODEL_ROUTER` с инструментами, писатель на модели роли с `toolsets=[]`, итог проходит
критика. Есть тест на обе стадии (`TestModel`/`FunctionModel`). Privacy уважается (инструменты уже гейтят). `make test` зелёный.

### ПРОМТ
```
Ты — инженер AI-пайплайна в проекте DoubleU (Python, pydantic-ai).
ЗАДАЧА (ОДИН коммит): двумодальное исполнение. Когда у роли ВКЛЮЧЕНЫ инструменты — дешёвая модель-КОНСТАНТА
(оркестратор) понимает какие инструменты нужны, ВЫПОЛНЯЕТ их и собирает контекст, затем модель, ВЫБРАННАЯ ВЛАДЕЛЬЦЕМ
роли (писатель), пишет финальный ответ из этого контекста БЕЗ инструментов. Нет включённых инструментов → один вызов
(текущий путь без изменений). Удвоение вызова — ТОЛЬКО при включённых тулзах.

Где смотреть:
- ai-python/ai_agent/agents/models.py — AgentTask, build_model, build_tier_model, env MODEL_DRAFT/MODEL_CHEAP/MODEL_TIER_*.
- ai-python/ai_agent/agents/runtime.py — AgentRuntime; _draft_async уже передаёт per-run model/toolsets/usage_limits;
  есть resolve_builtin_toolset(ai_role) (из tools-платформы) и _resolve_mcp_toolsets; критик _review_draft.
- ai-python/ai_agent/agents/results.py — DraftResult, CriticVerdict. config/flags.py — фиче-флаги.
- ai-python/ai_agent/pipelines/draft.py — build_draft_prompt/_compose_base_instruction (системный промпт ПИСАТЕЛЯ);
  инструкции-агентов задаются константами (образец _CRITIC_INSTRUCTIONS в runtime.py). Докстринги — memory_tools.py.

Сделай:
1) AgentTask.ORCHESTRATE → env MODEL_ROUTER (default = MODEL_CHEAP). Это ГЛОБАЛЬНАЯ КОНСТАНТА (не per-role).
2) OrchestratorResult (frozen) в results.py: gathered_context: tuple[str,...], suggested_intent: str, notes: str.
   Оставь место под будущее поле «форма ответа» (его добавит коммит interaction).
3) ПРОМПТЫ — ДВА инструктажа (ядро задачи):
   3a) Оркестратору — НОВЫЙ системный промпт: константа _ORCHESTRATOR_INSTRUCTIONS (по образцу _CRITIC_INSTRUCTIONS в
       runtime.py) + хелпер build_orchestrator_prompt(request, prompt_builder) в pipelines/draft.py. Смысл: «пойми
       последнее сообщение и роль; реши какие из ДОСТУПНЫХ инструментов нужны; вызови их и собери факты/контекст
       (+выполни разрешённые действия); верни OrchestratorResult. НЕ пиши финальный ответ. Экономь вызовы.» Дай
       урезанный персона-контекст (роль/цели), без имперсонации и без правил формата ответа.
   3b) Писателю — ОБНОВИ существующий промпт build_draft_prompt/_compose_base_instruction: в двухстадийном режиме
       (а) инжектируй бандл оркестратора (gathered_context) в контекстный блок; (б) добавь директиву «контекст уже
       собран, опирайся на него; инструментов в этом ходе НЕТ; не делай вид, что ищешь». Одностадийный режим — промпт
       без изменений (обратная совместимость).
   3c) Докстринги инструментов в memory_tools.py — это промпт ВЫБОРА инструментов, теперь его читает оркестратор;
       оставь их модель-агностичными.
4) Оркестратор-агент в runtime.py: модель ORCHESTRATE, instructions=_ORCHESTRATOR_INSTRUCTIONS, владеет ВКЛЮЧЁННЫМ
   тулсетом роли (resolve_builtin_toolset + MCP), с UsageLimits(tool_calls_limit=_DRAFT_TOOL_CALLS_LIMIT). Выполняет
   инструменты, возвращает OrchestratorResult.
5) Писатель = существующий draft-агент: в двумодальном пути запусти с toolsets=[] на МОДЕЛИ РОЛИ
   (build_tier_model(role.model_tier), иначе DRAFT-модель). user-payload = сообщение + бандл оркестратора (через
   обновлённый build_draft_prompt). Критик _review_draft — стадия 3, без изменений.
6) Гейт: двумодальный путь ⇔ (флаг TWO_STAGE_DRAFT включён) И (у роли ≥1 включённый инструмент — проверь по
   tool_settings/каталогу). Иначе — текущий одностадийный _draft_async без изменений. Флаг в config/flags.py, default false.
7) Наблюдаемость: записывай ОБЕ стадии через _record_run; model_used = "orchestrate(<router>)+draft(<tier>)"
   (+"+critic" при ревизии).

Правила: type-аннотации, frozen dataclass, без голого except; недоверенный ввод через PromptBuilder; privacy —
бандл уважает no_memory (инструменты уже гейтят); техдолг — docs/backlog.md; обнови .env.example (MODEL_ROUTER,
TWO_STAGE_DRAFT).

Тесты (ai-python/tests/test_agents.py): флаг off ИЛИ роль без тулзов → один вызов, существующие тесты зелёные, путь
не изменён; флаг on + роль с тулзами → оркестратор на MODEL_ROUTER с инструментами и НЕ пишет финал, писатель на
модели роли с toolsets=[] и получает бандл оркестратора в промпте, результат проходит критика (TestModel/FunctionModel,
MOCK_LLM-стиль). `make test` зелёный.

Коммить ОДНИМ коммитом: feat(ai): bimodal orchestrator+writer execution (gated by enabled tools). Только это в коммите.
```

### Черновики текстов промптов (готовые — чтобы ChatGPT не сочинял с нуля)

`_ORCHESTRATOR_INSTRUCTIONS` (в `runtime.py`, рядом с `_CRITIC_INSTRUCTIONS`):
```python
_ORCHESTRATOR_INSTRUCTIONS = (
    "Ты — стадия подготовки контекста для ответа в Telegram-чате; финальный ответ ты НЕ пишешь."
    " Задача: понять последнее сообщение собеседника и назначенную роль, решить, каких данных не хватает,"
    " и собрать их ТОЛЬКО доступными тебе инструментами."
    " Правила:"
    " 1) вызывай инструмент лишь когда он реально нужен для точного ответа; не дублируй вызовы;"
    " 2) не выдумывай факты — если инструмент ничего не вернул, так и отметь;"
    " 3) действия (например, напоминания) выполняй только при явной необходимости из сообщения;"
    " 4) НЕ пиши ответ собеседнику и не имитируй владельца."
    " Верни OrchestratorResult: gathered_context — собранные факты/контекст (по строке на пункт);"
    " suggested_intent — предполагаемый тип ответа; notes — короткие пометки писателю, что учесть."
)
```

Директива писателю (добавляется в `build_draft_prompt` ТОЛЬКО в двухстадийном режиме; `{bundle}` = строки `gathered_context`):
```python
_WRITER_TWO_STAGE_DIRECTIVE = (
    "\n\nКОНТЕКСТ УЖЕ СОБРАН на предыдущей стадии и приведён ниже — опирайся на него."
    " В этом ходе у тебя НЕТ инструментов: не ищи в памяти и не делай вид, что ищешь."
    " Если данных не хватает — отвечай по тому, что есть, ничего не выдумывая."
    "\n\nСОБРАННЫЙ КОНТЕКСТ:\n<gathered_context>\n{bundle}\n</gathered_context>"
)
```

---

## 7. COMMIT 3 — interaction-инструмент (кнопки/вопрос/подтверждение собеседнику)

### Принцип
Это **interaction**-категория: инструмент НЕ шлёт сразу. Он формирует ФОРМУ ответа (вопрос+варианты),
которая становится черновиком и идёт обычным путём decision→approval→send. Контакт-фейсинг кнопки — только
`Transport == bot`. Нажатие собеседника → новый ход ИИ. `choice_question` (карточка владельцу) остаётся и
параллельно выражается как этот инструмент.

### План
1. **Инструмент** `interaction.present_buttons(prompt, options[])` (и/или `request_confirmation(prompt)`):
   при вызове НЕ отправляет, а записывает «план ответа» в `deps` (мутабельное поле, читается рантаймом после
   прогона) и расширяет `OrchestratorResult` полем `response_plan` (shape="question", options).
2. **Рендер в черновик:** рантайм, увидев `response_plan`, формирует `DraftResult` с `intent="choice_question"` +
   `options` (переиспользуй существующий контракт options).
3. **Отправка контакту (Go, `core/service.go`):** при `Transport == bot` И включённом tool-key
   `interaction.present_buttons` — на этапе send собрать `InlineKeyboardMarkup` из options и отправить контакту на
   `contracts.RawChatID`. `callback_data` = новый префикс `aibtn:<draftId>:<idx>:<idem>` (расширь
   `contracts.CallbackData*` по образцу choice-кодеков). Сохрани соответствие draft_id→options (миграция при необходимости).
4. **Нажатие:** ветка в `handleCallbackQuery` — декод `aibtn` → `AnswerCallbackQuery` → выбор как новый входящий ход
   → обычный AI-пайплайн. Опционально `EditMessageText` (пометить выбор, снять клавиатуру).
5. **Гейты:** строго `Transport == bot` + включённый tool-key. Идемпотентность по `ReceiveIdempotencyKey`
   (`updateID:callback_query:queryID`). Аппрув не меняется: в manual/semi владелец одобряет черновик-вопрос, в auto — летит.

### Критерии приёмки
В режиме бота ИИ присылает сообщение с кнопками (после аппрува, если режим того требует); нажатие порождает
корректный следующий ход. В personal/business кнопки НЕ шлются (тест). Двойное нажатие не даёт двух ходов. `make test` зелёный.

### ПРОМТ
```
Ты — инженер DoubleU (Go core + Python AI на pydantic-ai). ЗАДАЧА (ОДИН коммит): добавить INTERACTION-инструмент —
ИИ может ответить СОБЕСЕДНИКУ вопросом с inline-кнопками / запросить подтверждение и понять нажатие. Только режим
бота (Transport == bot). Это инструмент в существующей tools-платформе (category=interaction).

КЛЮЧЕВОЙ ПРИНЦИП: инструмент НЕ шлёт сразу. Он формирует ФОРМУ ответа, которая становится черновиком и идёт обычным
путём decision→approval→send (аппрув владельца НЕ меняется: manual/semi → черновик-вопрос на одобрение, auto → летит).

Что уже есть (не дублируй): транспорт готов в backend-go/internal/telegram/telegram.go (InlineKeyboardMarkup,
AnswerCallbackQuery, EditMessageText, ParseCallbackQueryUpdate; callback_query в allowed_updates). Роутинг —
backend-go/internal/core/service.go::handleCallbackQuery (там же handleChoiceOption и contracts.CallbackData* кодеки).
Есть intent "choice_question"+options (карточка ВЛАДЕЛЬЦУ) — переиспользуй контракт options, но это ДРУГОЙ,
контакт-фейсинг поток. Двумодальный рантайм (оркестратор+писатель) и OrchestratorResult — в ai-python/ai_agent/agents/
runtime.py и results.py. Tool-каталог — agents/tool_catalog.py. python-telegram-bot НЕ использовать.

Сделай:
1) Python: инструмент interaction.present_buttons(prompt: str, options: list[str]) (+ опц. request_confirmation(prompt))
   в новом гейтируемом тулсете; зарегистрируй в каталоге (category=interaction). При вызове НЕ отправляет, а пишет план
   ответа в AgentDeps (новое мутабельное поле) и расширь OrchestratorResult полем response_plan (shape, options).
   Рантайм по response_plan формирует DraftResult intent="choice_question" + options.
2) Go отправка: в send-пути core/service.go при Transport == bot И включённом tool-key "interaction.present_buttons"
   собери InlineKeyboardMarkup из options и отправь контакту на contracts.RawChatID(chatID). callback_data = новый
   префикс "aibtn:<draftId>:<optionIdx>:<idem>" — расширь contracts.CallbackData* (encode+decode) по образцу
   choice-кодеков. Сохрани draft_id→options (миграция при необходимости; следующий номер, сейчас 112; +.down).
3) Нажатие: ветка в handleCallbackQuery — декод "aibtn" → AnswerCallbackQuery → выбор контакта как новый входящий ход
   (текст = значение опции) → обычный AI-пайплайн. Опционально EditMessageText (пометить выбор, снять клавиатуру).
4) Гейты: строго Transport == bot + включённый tool-key. Идемпотентность по ReceiveIdempotencyKey
   (updateID:callback_query:queryID); повтор игнор (как duplicate_callback_query). Контракт (options/новые поля) —
   синхронно Go+Python + контракт-тест.

Правила: privacy уважать; Go — slog; Python — type-аннотации, frozen dataclass, без голого except; миграция (если есть)
идемпотентна и с .down; техдолг — docs/backlog.md.

Тесты (core/service_test.go, telegram_test.go, test_agents.py): инструмент формирует choice_question-черновик (не шлёт
сам); сборка клавиатуры из options; encode/decode "aibtn"; нажатие→новый ход; ГЕЙТ — personal/business кнопки НЕ шлются;
идемпотентность двойного нажатия. `make test` зелёный.

Коммить ОДНИМ коммитом: feat(tools): interaction tool — contact buttons/confirm (bot mode). Только это в коммите.
```

---

## 8. COMMIT 4 — scheduler-инструмент (cron/напоминания, ИИ строит сам)

### Принцип
**schedule**-категория: создаётся автономно оркестратором, эффект отложен, **видим и отменяем** владельцем.
Контакт-фейсинг срабатывание уважает режим чата в момент `fire_at` (manual → черновик владельцу; auto → отправка).
Owner-фейсинг напоминание — просто уведомление владельцу.

### План
1. **Миграция** `scheduled_jobs` (+`.down`): `id, tenant_id, chat_id, bot_id NULL, kind
   ('reminder'|'message'|'ai_followup'), target ('owner'|'contact'), fire_at timestamptz, payload jsonb, status
   ('pending'|'sent'|'cancelled'|'failed'), created_by ('ai'|'owner'), idempotency_key UNIQUE, created_at, sent_at`.
   Индекс `(status, fire_at)`.
2. **Store:** `InsertScheduledJob`, `DueJobs(now, limit)` (`FOR UPDATE SKIP LOCKED`), `MarkJobSent/Failed`,
   `CancelJob`, `ListJobs(tenant, chat)`.
3. **Воркер (Go)** `core/service.go::StartScheduler(ctx)`: тикер 30–60с, берёт due, диспатчит: `target=contact` —
   уважать режим чата (auto→send, manual/semi→черновик владельцу); `target=owner` — уведомить владельца; `ai_followup` —
   новый AI-ход. Идемпотентно. Подключить в `cmd/backend/main.go` и `httpapi/server.go` рядом со `StartDailySummaryScheduler`.
4. **Инструмент** `scheduler.create_reminder(fire_at_iso, text, target)` (category=schedule), пишет `scheduled_jobs`
   в скоупе `deps.tenant_id/chat_id`. В системный промт подать текущее время + таймзону владельца
   (`UserSettings.Timezone`). Валидация: `fire_at` строго в будущем, ≤ 1 год. Privacy: `no_memory`/`private` → недоступен.
5. **UI:** список `scheduled_jobs` по чату + отмена (GET/DELETE + метод в `client.js`).

### Критерии приёмки
Store CRUD + due покрыты; воркер с `NoopSender` помечает `sent` и идемпотентен; инструмент создаёт задание (реальная БД,
`MOCK_LLM`, `AI_MEMORY_SOURCE=python`); прошедший/слишком далёкий `fire_at` отклоняется; контакт-фейсинг срабатывание
уважает режим чата. `make test` зелёный.

### ПРОМТ
```
Ты — fullstack-инженер DoubleU (Go core + Python AI на pydantic-ai). ЗАДАЧА (ОДИН коммит): SCHEDULER-инструмент —
ИИ сам ставит напоминания/cron (category=schedule в tools-платформе), плюс воркер, исполняющий их в срок.

Принцип: задание создаётся автономно, но ВИДИМО И ОТМЕНЯЕМО владельцем. Контакт-фейсинг срабатывание уважает режим
чата в момент fire_at (auto→отправка, manual/semi→черновик владельцу). Owner-фейсинг — уведомление владельцу.

Что есть: ручной планировщик core/service.go::StartDailySummaryScheduler (подключён в cmd/backend/main.go и
httpapi/server.go) — образец цикла. Инструменты — паттерн ai-python/ai_agent/agents/memory_tools.py (FunctionToolset,
скоуп deps.tenant_id/chat_id, privacy-гейты) и реальная-БД-проверка ai-python/tests/test_memory_tools_db.py.
Tool-каталог — agents/tool_catalog.py. Таймзона — contracts.UserSettings.Timezone (уже шлётся в process-message).

Сделай:
1) Миграция (следующий свободный номер; сейчас старший 112; +.down): scheduled_jobs(id, tenant_id, chat_id, bot_id NULL,
   kind CHECK in ('reminder','message','ai_followup'), target CHECK in ('owner','contact'), fire_at timestamptz,
   payload jsonb, status CHECK in ('pending','sent','cancelled','failed'), created_by CHECK in ('ai','owner'),
   idempotency_key text UNIQUE, created_at timestamptz default now(), sent_at). Индекс (status, fire_at).
2) Store (backend-go/internal/store/store.go): InsertScheduledJob, DueJobs(now, limit) с FOR UPDATE SKIP LOCKED,
   MarkJobSent, MarkJobFailed, CancelJob, ListJobs(tenant, chat).
3) Воркер core/service.go::StartScheduler(ctx): тикер 30–60с, берёт due pending и диспатчит: target=contact — уважать
   режим чата (auto→send через s.sender, manual/semi→черновик владельцу), target=owner — уведомить владельца,
   ai_followup→новый AI-ход. Идемпотентно (MarkJobSent так, чтобы повтор не дублировал). Подключи в cmd/backend/main.go
   и httpapi/server.go рядом со StartDailySummaryScheduler.
4) AI-инструмент scheduler.create_reminder(fire_at_iso: str, text: str, target: 'owner'|'contact') (category=schedule);
   зарегистрируй в каталоге и в резолвинге тулсета роли. Пишет scheduled_jobs в скоупе deps.tenant_id/chat_id. В системный
   промт подай текущее время и таймзону владельца, чтобы модель вычислила fire_at. Валидация: fire_at строго в будущем и
   ≤ 1 год — иначе вернуть ошибку строкой. Privacy: no_memory/private → недоступен.
5) Mini App: список scheduled_jobs по чату + отмена (GET/DELETE эндпоинты + метод в client.js).

Правила: контракт (если меняешь обмен Go↔Python) — синхронно + контракт-тест; privacy/идемпотентность обязательны;
Go — slog, без гонок (тикер + SKIP LOCKED); Python — type-аннотации, frozen dataclass, без голого except; миграция
идемпотентна и с .down; техдолг — docs/backlog.md; обнови .env.example при новых переменных.

Тесты: store CRUD + DueJobs; воркер с NoopSender помечает sent и идемпотентен; инструмент создаёт задание (реальная БД
в стиле test_memory_tools_db.py, MOCK_LLM=true, AI_MEMORY_SOURCE=python); прошедший/слишком далёкий fire_at отклоняется;
контакт-фейсинг срабатывание уважает режим чата. `make test` зелёный.

Коммить ОДНИМ коммитом: feat(tools): scheduler tool + job worker (AI-built reminders). Только это в коммите.
```

---

## 9. COMMIT 5 — integration-инструмент (исходящие вебхуки) — второстепенно

### Принцип
**integration**-категория. Для «инструментообразных» интеграций предпочитать **MCP** (уже есть). Этот инструмент —
для простых fire-and-forget вебхуков. Жёсткий allow-list + SSRF-защита.

### План
1. **Allow-list:** таблица `role_webhooks(tenant_id, name, url, method, auth_header, enabled)` (+`.down`) и методы store.
   (Или переиспользовать `mcp_servers`, если чище.)
2. **Инструмент** `http.request(name, body)` (category=integration); только allow-listed URL; усечённый ответ.
   **SSRF:** только https; хост строго из allow-list; запрет приватных/loopback/link-local IP и cloud-metadata
   (169.254.169.254); таймаут; лимит размера. Privacy: `no_memory`/`private` → недоступен.
3. **(Опц.) Входящий** `POST /hooks/{tenant}/{token}`: per-tenant секрет → постановка события; идемпотентность по ключу.

### Критерии приёмки
Хост вне allow-list отклонён; внутренние IP/SSRF заблокированы; таймаут/лимит соблюдены. (Если входящий) неверный
секрет → отказ; повтор с ключом не дублирует. `make test` зелёный.

### ПРОМТ
```
Ты — backend-инженер DoubleU (Go core + Python AI на pydantic-ai). ЗАДАЧА (ОДИН коммит, ВТОРОСТЕПЕННАЯ): INTEGRATION-
инструмент — ИИ вызывает внешние вебхуки (исходящие) с жёстким allow-list и SSRF-защитой (category=integration в
tools-платформе). Входящий приём — опционально в этом же коммите, если останется время.

Контекст: «инструментообразные» интеграции уже покрыты MCP (mcp_servers/role_mcp_access, runtime.py::_resolve_mcp_toolsets,
FilteredToolset) — этот инструмент для ПРОСТЫХ fire-and-forget вебхуков. Паттерн — ai-python/ai_agent/agents/memory_tools.py.
Tool-каталог — agents/tool_catalog.py.

Сделай:
1) Allow-list: таблица role_webhooks(tenant_id, name, url, method, auth_header, enabled) — миграция (следующий номер,
   сейчас 112; +.down) и методы store. (Или переиспользуй mcp_servers, если чище.)
2) Инструмент http.request(name, body) (category=integration; зарегистрируй в каталоге и резолвинге тулсета роли).
   Запрос ТОЛЬКО на allow-listed URL; усечённый текст ответа. SSRF ОБЯЗАТЕЛЬНО: только https; хост строго из allow-list;
   запрет приватных/loopback/link-local IP и 169.254.169.254; таймаут; лимит размера. Privacy: no_memory/private → недоступен.
3) (Опц.) Входящий POST /hooks/{tenant}/{token} (httpapi/server.go + хендлер): валидация per-tenant секрета → постановка
   события (напр. запланированное сообщение/уведомление). Идемпотентность по переданному ключу.

Правила: privacy/идемпотентность; контракт (если трогаешь) — синхронно + контракт-тест; Go — slog; Python —
type-аннотации, frozen dataclass, без голого except; миграция идемпотентна и с .down; секреты не логировать; техдолг — docs/backlog.md.

Тесты: хост вне allow-list отклонён; внутренние IP/SSRF заблокированы; таймаут/лимит соблюдены; (если входящий) неверный
секрет → отказ, повтор с ключом не дублирует. `make test` зелёный.

Коммить ОДНИМ коммитом: feat(tools): outbound webhook tool (allow-list + SSRF). Только это в коммите.
```

---

## 10. COMMIT 6 — Голосовое создание AI-роли (независимо)

### План
1. **AI** `POST /ai/draft-role`: вход — свободный текст (+язык-хинт), выход — структурный `RoleDraft` (зеркало
   `AIRoleData`) через нового агента со structured output; ввод обернуть `PromptBuilder`.
2. **Go** `POST /api/v1/roles/draft`: принимает `{text}` ИЛИ `{audio_base64, mime_type}`; для audio — сперва
   `ExtractMedia` (транскрипция), затем `DraftRole`; возвращает roleJSON-черновик (НЕ сохраняет). `aiclient.DraftRole`.
3. **Контракт:** `DraftRoleRequest/Response` синхронно.
4. **UI:** в `role-edit.js` (создание) — «🎙 Описать голосом» (MediaRecorder→base64) и «✍️ Из текста»; черновик
   пред-заполняет форму → существующий Save.

### Критерии приёмки
`/ai/draft-role` отдаёт валидный структурный черновик (`MOCK_LLM`); Go-эндпоинт работает на обоих путях; черновик не
персистится до Save. `make test` зелёный.

### ПРОМТ
```
Ты — fullstack-инженер DoubleU (Go backend + Python AI на pydantic-ai + vanilla-JS Mini App). ЗАДАЧА (ОДИН коммит,
НЕЗАВИСИМАЯ фича): голосовое/текстовое создание AI-роли — пользователь описывает роль, мы транскрибируем и генерируем
заполненный черновик, который он правит и сохраняет.

Что есть: транскрипция POST /ai/extract-media (ai-python/ai_agent/http_server.py + service.py; Go —
backend-go/internal/aiclient/{client.go,http.go} ExtractMedia, вход base64+mime/kind). Роли: AIRoleData
(ai-python/ai_agent/schemas.py), AIRole (backend-go/internal/contracts/types.go), CRUD
backend-go/internal/httpapi/miniapp_roles_handlers.go (roleJSON/roleRequest), UI miniapp/screens/role-edit.js
(renderRoleEdit). Агент со structured output — образец extraction_agent в agents/runtime.py; результаты agents/results.py.

Сделай:
1) Python: RoleDraft (frozen) в results.py — поля зеркально AIRoleData (name, description, speaking_style, goals,
   constraints, custom_fields[]). Новый агент в runtime.py на mid/cheap модели со structured output, инструкция: «из
   свободного описания собери цельную готовую роль». Эндпоинт POST /ai/draft-role (http_server.py + service.py + schemas/
   parse). Недоверенный ввод — PromptBuilder.build. Язык черновика — по языку описания.
2) Контракт синхронно (Go+Python): DraftRoleRequest/Response. aiclient: метод DraftRole.
3) Go POST /api/v1/roles/draft (miniapp_roles_handlers.go + маршрут server.go): {text} ИЛИ {audio_base64, mime_type};
   audio → ExtractMedia → DraftRole; возвращает roleJSON-черновик; НИЧЕГО не сохраняет.
4) Mini App: в role-edit.js (создание) кнопки «🎙 Описать голосом» (MediaRecorder→base64→/api/v1/roles/draft) и «✍️ Из
   текста»; черновик пред-заполняет форму, пользователь правит и жмёт существующий Save. Метод в miniapp/api/client.js.

Правила: контракт синхронно + контракт-тест; privacy — не логируй сырой звук/секреты (data_base64 уже эленится в
_request_summary — сохрани); Go — slog; Python — type-аннотации, frozen dataclass, без голого except; техдолг — docs/backlog.md.

Тесты: /ai/draft-role отдаёт валидный структурный черновик при MOCK_LLM (test_http_server.py/test_agents.py); Go-эндпоинт
покрыт на обоих путях; черновик не персистится. `make test` зелёный.

Коммить ОДНИМ коммитом: feat(roles): voice/text-driven AI role drafting. Только это в коммите.
```

---

## 11. Чек-лист ревью каждого коммита

- [ ] Контракт синхронно Go+Python (+контракт-тест); enum-ы через `make contracts`.
- [ ] Миграция идемпотентна, с `.down`, номер не конфликтует.
- [ ] Privacy-флаги на всех новых путях. Идемпотентность отправок/колбэков/заданий/вебхуков.
- [ ] Недоверенный ввод обёрнут (`PromptBuilder`).
- [ ] Таксономия соблюдена: контакт-видимое идёт через approval→send; внутреннее автономно; schedule видим/отменяем.
- [ ] `slog` (Go) / type-аннотации + frozen dataclass (Python); без голого `except`.
- [ ] `make test` зелёный; детерминизм через `MOCK_LLM=true`. Техдолг — `docs/backlog.md`.
- [ ] Ровно один коммит на фичу, conventional-сообщение.
