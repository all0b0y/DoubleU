# DoubleU — Project Charter for AI Agents

This file defines the mandatory rules for all AI agents (Claude Code and similar) working on this codebase.
**All rules are binding. Deviating from them requires explicit user instruction.**

---

## Code Exploration

- For **understanding code and finding relationships** (call chains, callers, data flow, cross-service
  links, architecture, impact analysis), use the **Codebase Memory MCP** (`codebase-memory-mcp` tools:
  `search_graph`, `trace_path`, `get_code_snippet`, `query_graph`, `get_architecture`, `search_code`)
  **first**. Fall back to `Grep`/`Glob`/`Read` for plain text, configs, and non-code files. If the
  project is not indexed yet, run `index_repository` first.

---

## Code Quality

- Write **clean, readable code**. Prefer clarity over cleverness.
- Write **algorithmically efficient code**. Avoid unnecessary loops, allocations, and DB calls.
- **No boilerplate** that only works for one specific case. Code must handle the general case.
- **No template/stub implementations** that exist just to make a test pass. Code must actually work.
- Keep functions small and focused on a single responsibility.

## Documentation

- Write **inline documentation** (godoc / Python docstrings) on every exported function, method, and type.
- Comments explain **WHY**, not WHAT. The code already shows what it does.
- Document non-obvious invariants, constraints, and security assumptions.
- Do NOT write multi-paragraph comments for trivial code.

## Testing

- Write **functional tests** that verify behaviour, not implementation details.
- Tests must test the **feature**, not the specific code path.
- **Do not write tests that are fitted to the code** — write them against the contract/spec first.
- Use real dependencies where possible (real DB in integration tests, not mocks).
- Tests must remain green. Never commit broken tests.

## Architecture

- Follow the **frozen contract** between Go backend and Python AI service (`/contracts/`). Do not change the contract without updating both sides.
- **Privacy flags** (`no_memory`, `no_training`, `no_summary`, `private`) must be respected at every layer.
- **Idempotency** must be maintained for all webhook processing, send operations, and approval callbacks.
- Risk classification and policy decisions belong to the backend, not the AI service.

## Native Tools (карта связей)

Полный справочник — [`docs/reference/tools.md`](docs/reference/tools.md). Инструменты AI-агента
только **записывают намерение** в `AgentDeps`; всё видимое/персистентное исполняет Go-бэкенд.
Источник правды каталога — Python `TOOL_CATALOG`; Go зеркалит его (синк-тест
`TestToolCatalogMatchesCanonicalPythonCatalog`). Меняя контракт инструмента, правь **обе** стороны
и бампай `contracts/contracts.json` (затем `python3 scripts/generate_contracts.py`).

| Инструмент (ключ) | Python | Контракт-поле | Go-потребитель → стор |
|---|---|---|---|
| `memory.*` (6 шт.) | `agents/memory_tools.py` | прямой ответ / `memory_used` | `service.go` → `semantic_facts`, `contact_profiles*` |
| `interaction.ask_questions` | `agents/interaction_tools.py` | `response_plan{shape,questions}` | `service.go` (анкета+callback) → `interaction_forms` |
| `scheduler.create_reminder` | `agents/schedule_tools.py` | `scheduled_jobs[]` | `saveAIScheduledJobs` → `scheduled_jobs` |
| `scheduler.list_reminders` | `agents/schedule_tools.py` | req `existing_scheduled_jobs[]` | `existingScheduledJobsFor` (read) |
| `scheduler.cancel_reminder` | `agents/schedule_tools.py` | `cancelled_job_ids[]` | `cancelAIScheduledJobs` → `CancelJob` |
| `skills.load_skill` | `agents/skill_tools.py` | req `ai_role.skills[]` (level-1) | `RoleSkills` → `skills`/`role_skills` (тело читает сам Python) |

- **Анкета (`ask_questions`):** агент готовит пул из 1–6 вопросов; бот задаёт их **по одному**
  (кнопка-подсказка или ответ своими словами). Ответы копятся в `interaction_forms` (один активный
  на чат, TTL 24ч); после ВСЕХ вопросов Q&A уходит в ИИ как один ход. Callback — `qf:{form}:{q}:{slot}`.
- **Планировщик:** create/list/cancel; `existing_scheduled_jobs` шлётся в запрос только когда роль
  включает list/cancel. Категория `schedule` гейтится `no_memory`/`private`.
- **Навыки (`load_skill`, Г4):** progressive disclosure — уровень 1 (name+description) идёт в промпт
  writer'а из `ai_role.skills`; уровень 2 (тело) писатель тянет инструментом `load_skill` **только**
  для включённого навыка роли (`deps.enabled_skill_names`). Skill = запись в `skills` (tenant_id NULL =
  глобальный first-party); привязка к роли — `role_skills`. Writer-only (оркестратор навыки не видит),
  НЕ гейтится privacy (owner-контент, как `role.context`). Исполняемых скриптов нет.
- **MCP control plane (Г1):** owner CRUD для `mcp_servers` (`miniapp_mcp_handlers.go`), `auth_header`
  шифруется at rest (AES-256-GCM, `MCP_SECRETS_KEY`), SSRF-гард на URL (`mcp_url_guard.go`), результаты
  MCP оборачиваются как untrusted (`sanitize_toolset.py`). Проверка сервера — `POST /ai/mcp-check`.

## Refactoring & Backlog

- When you find **inefficient code** during work, add it to `/docs/backlog.md` under `## Inefficiency Backlog` instead of silently leaving it.
- When you find a **bug**, fix it immediately if small (< 10 lines). If large, add to `/docs/backlog.md` under `## Bug Backlog` and notify in your response.
- When you identify **technical debt**, add it to `/docs/backlog.md` under `## Tech Debt`.

## Security

- Never expose secrets, tokens, or credentials in logs or responses.
- All user-supplied text (Telegram messages) is **untrusted input** — wrap in XML tags before LLM calls.
- Validate at system boundaries (webhook input, external API responses). Trust internal code.
- Never bypass safety checks without explicit user instruction.

## Go-specific

- Use `slog` for structured logging. Never use `fmt.Println` in production paths.
- Errors must be handled or explicitly ignored with `_` and a comment explaining why.
- Avoid global mutable state. Pass dependencies via constructors.
- Use `sync.Map` or `sync.Mutex` for shared state. Never race on maps.

## Python-specific

- Use type annotations everywhere.
- Dataclasses with `frozen=True` for all request/response schemas.
- No bare `except:` — always catch specific exceptions.
- Keep AI service stateless — no in-memory caches between requests except via injected dependencies.

## Iteration 2 (deferred — do not implement without explicit instruction)

- Mini App UI (web dashboard)
- AI Roles Marketplace
- Telegram Client API / MTProto for historical message access
