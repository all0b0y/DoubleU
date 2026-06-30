# План: Корректность времени, таймзоны и планировщика напоминаний

Дата: 2026-06-30
Роль исполнителя: AI/Go backend
Контракт затрагивается: **да** (см. правила бампа в `CLAUDE.md` → раздел «Native Tools»).

---

## 0. Контекст и постановка

Расследование показало, что таймзонная математика в проекте **уже корректна** (UTC-канон,
`create_reminder` требует offset, `_parse_fire_at` отклоняет naive-время, Go хранит `fire_at` в UTC).
Реальные проблемы — на стыках:

1. **Таймзона bot-only пользователей неизвестна.** `user_settings.timezone` заполняется ТОЛЬКО
   из Mini App (фоновый sync браузерного IANA-пояса). Пользователь, общающийся только через бота,
   имеет пустой `timezone` → молчаливый фолбэк на `"UTC"` → «напомни в 18:00» у юзера в UTC+3
   срабатывает в 21:00 по местному. Тихий баг, без сигнала пользователю.
2. **Два источника часов.** `current_time` берётся из `datetime.now(utc)` AI-контейнера
   (`runtime.py:778,961`), а валидация «в будущем ли» в Go — из `s.now()`. Возможен скью.
3. **Дублирование/слабость даты в промпте.** Go кладёт `[CURRENT DATE: YYYY-MM-DD]` (дата-only, UTC),
   что дублирует и слабее питоновской строки `ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА`. Плюс таймстемпы транскрипта
   рендерятся в UTC, хотя модели говорят про локальный пояс владельца — модель путается.
4. **Будущие/повторяющиеся напоминания + DST.** Хранение только UTC `fire_at` теряет исходное
   wall-clock-время и пояс: при переходе DST между «сейчас» и срабатыванием напоминание сработает
   не в то местное время.

Это план реализации задач **A1 + C + B + D**.

---

## 1. Как ориентироваться в коде (обязательно)

Используй **Codebase Memory MCP** в первую очередь (правило `CLAUDE.md` → «Code Exploration»):

- `mcp__codebase-memory-mcp__search_code` — найти точки правок по паттерну.
- `mcp__codebase-memory-mcp__get_code_snippet` — прочитать точный исходник символа.
- `mcp__codebase-memory-mcp__trace_path` — проверить вызовы/импакт перед правкой
  (`direction="both"` для кросс-сервисных связей).
- `mcp__codebase-memory-mcp__search_graph` — найти символ/тест по имени.
- `mcp__codebase-memory-mcp__detect_changes` — после правок сопоставить git diff с затронутыми символами.

Если проект не проиндексирован — `index_repository`. `Grep`/`Read` — только для конфигов,
миграций, JSON-контрактов и неиндексируемых файлов.

### Якоря (проверены на 2026-06-30, перепроверь перед правкой)

| Что | Где |
|---|---|
| Go: заголовок `[CURRENT DATE]` + таймстемпы транскрипта | `backend-go/internal/core/ai_context.go` (~115, ~187) |
| Py: строка времени владельца | `ai-python/ai_agent/pipelines/draft.py` → `_current_time_instruction` (~181) |
| Py: вставка строки времени в спеки | `ai-python/ai_agent/prompts/library.py` (`draft_spec` ~98, `orchestrator_spec` ~169) |
| Py: инструмент напоминаний | `ai-python/ai_agent/agents/schedule_tools.py` → `create_reminder` / `_parse_fire_at` |
| Py: deps агента | `ai-python/ai_agent/agents/deps.py` (`current_time`, ~60) |
| Py: сборка deps / генерация current_time | `ai-python/ai_agent/agents/runtime.py` (`_deps_for` ~466, ~778, ~961) |
| Py: контракт запроса/настроек | `ai-python/ai_agent/schemas.py` (`UserSettingsData` ~244, `AIProcessMessageRequest` ~259, `parse_process_message_request` ~631) |
| Go: контракт типов | `backend-go/internal/contracts/types.go` (`UserSettings` ~523, `AIProcessMessageRequest` ~725) |
| Go: сохранение jobs | `backend-go/internal/core/service.go` → `saveAIScheduledJobs` (~452) |
| Go: стор jobs | `backend-go/internal/store/scheduled_jobs.go` (`InsertScheduledJob`, `DueJobs`, `ListJobs`) |
| Миграции | `infra/migrations/` (последняя — `118_*`; следующая — **119**) |
| Контракт-артефакт | `contracts/contracts.json` + `scripts/generate_contracts.py` + sync-тест `TestToolCatalogMatchesCanonicalPythonCatalog` |

---

## 2. A1 — Не угадывать пояс молча, а различать «неизвестно» и спрашивать

**Цель:** когда пояс владельца неизвестен, агент не должен молча считать его UTC. Он должен
либо подтвердить пояс через уже существующий `ask_questions`, либо отказаться ставить
напоминание на голое локальное время с понятной причиной.

**Семантика:** `user_settings.timezone == ""` ⇒ **пояс НЕИЗВЕСТЕН**; любое валидное IANA-имя
(включая явный `"UTC"`) ⇒ **известен**. Пустую строку НЕ маппить молча в `"UTC"`.

**Правки (Python):**
1. `AgentDeps` (`deps.py`): добавить `owner_timezone: str = ""` и `owner_timezone_known: bool = False`
   (или вычисляемое свойство). Прокинуть из `_deps_for` (`runtime.py`) — источник `request.user_settings.timezone`.
2. `_current_time_instruction` (`draft.py`): если пояс неизвестен — не печатать ложный `(UTC)`,
   а явно сообщить: «ПОЯС ВЛАДЕЛЬЦА НЕИЗВЕСТЕН: при просьбе о напоминании на голое локальное время
   сначала уточни пояс/город через ask_questions, не предполагай UTC». Если известен — текущее поведение.
3. `create_reminder` (`schedule_tools.py`): если `not ctx.deps.owner_timezone_known` — вернуть
   ошибку-подсказку (например `"error: owner timezone unknown — ask the owner for their timezone/city via ask_questions before scheduling a bare local time"`),
   чтобы агент перешёл к уточнению вместо догадки. Сохранить идемпотентность и текущие проверки.

**Тесты (Python, против контракта/поведения, не под реализацию):**
- неизвестный пояс + «напомни в 18» → `create_reminder` отказывает с подсказкой про ask_questions;
- известный пояс (`Europe/Moscow`) → работает как раньше, offset считается верно;
- инструкция времени содержит маркер «НЕИЗВЕСТЕН» только при пустом поясе.

---

## 3. C — Единый источник часов (Go штампует время запроса)

**Цель:** одно авторитетное «сейчас» на весь turn — убрать скью между AI-контейнером и Go.

**Контракт (обе стороны + бамп):**
1. Добавить в `AIProcessMessageRequest` поле `current_time` (ISO-8601 UTC с offset):
   - Go: `backend-go/internal/contracts/types.go` → `AIProcessMessageRequest` (`json:"current_time"`),
     заполнять из `s.now().UTC()` в точке формирования запроса к AI.
   - Py: `ai-python/ai_agent/schemas.py` → `AIProcessMessageRequest.current_time: str = ""`,
     распарсить в `parse_process_message_request` в aware `datetime` (UTC).
2. Бамп `contracts/contracts.json`, затем `python3 scripts/generate_contracts.py`. Проверить sync-тест.

**Правки (Python):**
3. `runtime.py`: вместо локального `datetime.now(timezone.utc)` (~778, ~961) использовать
   `request.current_time`, если оно пришло; иначе — текущий фолбэк (обратная совместимость).
   Прокинуть это значение в `_deps_for(..., current_time=...)`.

**Правки (Go):**
4. `saveAIScheduledJobs` (`service.go`): валидация `fireAt.After(now)` уже на `s.now()` — корректно;
   убедиться, что `current_time` запроса берётся из того же `s.now()` (один вызов на turn),
   чтобы генерация offset и валидация шли от одних часов.

**Тесты:**
- (Py) при переданном `current_time` инструкция времени и `create_reminder.now` используют его, а не `datetime.now`;
- (Go) контракт-тест/round-trip сериализации нового поля; существующие тесты планировщика зелёные.

---

## 4. B — Единый, локализованный блок времени в промпте

**Цель:** убрать дублирование и сделать таймстемпы консистентными с поясом владельца.

**Правки (Go, `ai_context.go`):**
1. `[CURRENT DATE: ...]`: либо **удалить** (единственный источник истины — питоновская строка
   `ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА`), либо проапгрейдить до owner-local datetime + offset, используя
   `user_settings.timezone` (Go уже имеет доступ через `Repository().UserSettings`). **Рекомендация:**
   апгрейд до локального datetime, т.к. этот заголовок в shared-context-блоке (нужен и оркестратору).
   Если пояс неизвестен — печатать дату в UTC с явной пометкой «(UTC, пояс владельца неизвестен)».
2. Таймстемпы транскрипта (`msgDate := ...UTC().Format(...)`, ~187): рендерить в поясе владельца,
   если он известен; иначе — UTC с суффиксом-пометкой. Формат оставить `2006-01-02 15:04` + краткий tz.

**Согласованность:** не печатать локальное и UTC-время в двух разных форматах, способных
противоречить друг другу. Один пояс-источник на весь блок.

**Тесты:**
- регрессия промпт-контракта (`tests/test_prompt_contract_regression.py` / Go-аналог): заголовок и
  таймстемпы в известном поясе — локальные; в неизвестном — UTC с пометкой;
- нет двух конфликтующих строк «текущее время».

---

## 5. D — Будущие/повторяющиеся напоминания и DST (хранить wall-clock + tz)

**Цель:** при переходе DST (или смене пояса владельца) между созданием и срабатыванием
напоминание срабатывает в **исходное местное время**, а не сдвинутое.

**Принцип (из best-practice):** для будущих меток хранить «(local clock time, tz_name)» как истину,
а UTC `fire_at` — как производное, пересчитываемое у момента срабатывания.

**Миграция (`infra/migrations/119_scheduled_jobs_walltime.sql` + `.down.sql`):**
1. Добавить в `scheduled_jobs`: `local_time text NULL` (наивное «YYYY-MM-DDTHH:MM:SS»),
   `tz_name text NULL` (IANA). Обратная совместимость: NULL ⇒ старое поведение (UTC `fire_at`).

**Контракт `ScheduledJob` (обе стороны + бамп):**
2. `create_reminder` (Py) и `ScheduledJob` (Py/Go): дополнительно передавать `local_time` + `tz_name`
   (агент уже знает локальное время и пояс владельца). UTC `fire_at` остаётся для ближних one-shot.

**Стор/планировщик (Go, `scheduled_jobs.go`):**
3. `InsertScheduledJob`: сохранять `local_time`/`tz_name`.
4. `DueJobs`: если у строки есть `local_time`+`tz_name`, пересчитывать эффективный `fire_at`
   из них в момент выборки (учитывая актуальный DST), иначе — как раньше по UTC `fire_at`.
   Сохранить идемпотентность и индекс `(status, fire_at)`.

**Тесты:**
- напоминание, заданное до перехода DST, срабатывает в исходное wall-clock-время после перехода;
- one-shot без `local_time`/`tz_name` ведёт себя как раньше (регрессия);
- идемпотентность вставки не нарушена.

> Если объём D окажется великоват для одного захода — оформить как отдельный коммит/PR после A1+C+B
> и завести запись в `docs/backlog.md`, но логику не «застаблить» (CLAUDE.md запрещает stub-реализации).

---

## 6. Порядок исполнения и Definition of Done

1. **C** (контракт `current_time`) — фундамент: единые часы. Бамп контракта.
2. **A1** (семантика «пояс неизвестен» + отказ/уточнение).
3. **B** (единый локализованный блок времени).
4. **D** (wall-clock + tz, миграция 119, пересчёт в DueJobs). При необходимости — отдельный коммит.

**DoD (всё обязательно):**
- Контракт изменён на **обеих** сторонах; `contracts/contracts.json` бампнут;
  `python3 scripts/generate_contracts.py` выполнен; sync-тест каталога зелёный.
- Привилегии/приватность не ослаблены: `no_memory`/`private` по-прежнему гейтят планировщик.
- `slog` в Go, type hints + `frozen=True` dataclasses в Py (CLAUDE.md).
- Тесты функциональные (против поведения), все зелёные: `cd backend-go && go test ./...`
  и `cd ai-python && pytest`.
- Найденные по пути неэффективности/баги — в `docs/backlog.md` (CLAUDE.md).
- Privacy-флаги и идемпотентность планировщика сохранены на всех слоях.

---

## 7. Риски / на что смотреть

- **Обратная совместимость контракта:** новые поля — с дефолтами (`current_time=""`,
  `local_time/tz_name=NULL`), чтобы старые запросы/строки не падали.
- **IANA-валидация:** в Go использовать `time.LoadLocation`, как в `apiSaveUserSettings`; невалидный
  пояс ⇒ трактовать как «неизвестен», а не падать.
- **DST у DueJobs:** пересчёт должен быть детерминированным и идемпотентным (одно и то же `fire_at`
  при повторных выборках до срабатывания).
- **Регрессия промпта:** `test_prompt_contract_regression` — самый чувствительный тест к B/A1.
