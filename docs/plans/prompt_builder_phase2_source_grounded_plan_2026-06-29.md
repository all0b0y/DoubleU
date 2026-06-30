# Prompt Builder Phase 2: source-grounded plan

Дата: 2026-06-29

Статус: план доработки нового Prompt Contract. Legacy prompt builder не развиваем.

## Проверенные входные данные

Основные источники:

- OpenAI prompt engineering: https://platform.openai.com/docs/guides/text?api-mode=chat#prompt-engineering
- OpenAI help prompt guide: https://help.openai.com/en/articles/4936848-how-do-i-create-a-good-prompt-for-an-ai-model
- OpenAI reasoning best practices: https://developers.openai.com/api/docs/guides/reasoning-best-practices
- Claude prompting best practices: https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices
- OpenRouter API reference overview: https://openrouter.ai/docs/api/reference/overview

Дополнительные краткие конспекты, использованы после основных источников:

- `/Users/all0b0y/Library/Mobile Documents/iCloud~md~obsidian/Documents/STUDY/AI/Материалы/GPT AND Reasoning.md`
- `/Users/all0b0y/Library/Mobile Documents/iCloud~md~obsidian/Documents/STUDY/AI/Материалы/CLAUDE.md`
- `/Users/all0b0y/Downloads/openapi.yaml` - OpenRouter OpenAPI schema, использована для проверки
  `reasoning`, `reasoning_effort`, `tools`, `tool_choice`, `parallel_tool_calls`,
  `response_format`, `structured_outputs`, `supported_parameters`, `architecture`,
  `canonical_slug`.

Проектные входные данные:

- `docs/backlog.md` проверен перед правкой плана. В фазу входят только prompt-builder хвосты:
  critic через PromptBuilder, concrete `model_used`, eval/regression gate. `integration` tool,
  LLM Guard и удаление rerank endpoint остаются отдельными backlog-треками.
- `.claude/analyze.md` использован как Langfuse evidence: новый builder лучше по контракту, но
  orchestration шумнее, есть malformed args `{}""`, duplicate `final_result`, fallback после
  `400 invalid request params`, prompt/schema bloat.
- Кодовая база проверена через codebase-memory graph: `PromptSpec`, renderers, `PromptBuilder`,
  `critic_spec`, `_review_draft`, auxiliary tasks, `ToolCatalogEntry`, `resolve_family`,
  `resolve_model_slug`, `resolve_builtin_toolset`.

## Где сейчас задаются роль и ограничения

Основные параметры нового prompt builder задаются в `PromptSpec`:

- `role` - кто модель в этом вызове;
- `task` - единственная задача вызова;
- `context` - trusted context, не raw user text;
- `constraints` - правила и запреты;
- `output_format`, `examples`, `success_criteria`, `tool_policy` - формат, примеры, критерии и
  политика инструментов.

Кодовый путь для writer/orchestrator/critic:

1. `ai-python/ai_agent/prompts/library.py` собирает `PromptSpec`.
2. `ai-python/ai_agent/prompts/renderers.py` рендерит его по prompt-shape семейству модели:
   базовый envelope должен быть XML, а степень детализации внутри тегов меняется по семейству:
   подробнее для generic chat/instruction моделей, минимальнее для reasoning-shaped моделей,
   workflow/tool-policy подробнее для agentic/XML-friendly моделей.
3. `ai-python/ai_agent/security.py:PromptBuilder.build(...)` кладет rendered prompt в system
   prompt, а raw user data в `<untrusted_user_message>`/`<untrusted_context>`.

Целевая XML-форма system prompt для роли, ограничений и формата:

```xml
<prompt_contract>
  <role>...</role>
  <task>...</task>
  <trusted_context>
    <item>trusted role/context fact</item>
  </trusted_context>
  <constraints>
    <constraint>hard role constraint</constraint>
    <constraint>product safety constraint</constraint>
  </constraints>
  <tool_policy>...</tool_policy>
  <output_format>
    <text_format>plain_text | telegram_rich_markdown</text_format>
    <rules>...</rules>
  </output_format>
  <success_criteria>
    <criterion>...</criterion>
  </success_criteria>
</prompt_contract>
```

Raw сообщение пользователя не должно попадать в `PromptSpec.context`; оно идет отдельным user
payload через untrusted envelope.

Для rich mode `PromptSpec.output_format` должен явно требовать Telegram Rich Markdown:

```xml
<output_format>
  <text_format>telegram_rich_markdown</text_format>
  <rules>
    Используй Markdown только для читаемости: короткие заголовки, списки, акценты.
    Не используй разметку как украшение и не ломай Telegram markdown parser.
    Plain/copy/short intent должен оставаться plain_text без markdown.
  </rules>
</output_format>
```

## Подтвержденные системы и решение по ним

| Система | Подтверждение источниками | Состояние в коде | Решение для фазы 2 |
|---|---|---|---|
| Prompt contract и trust boundary | OpenAI и Claude требуют ясные инструкции, контекст, формат, примеры; user input должен быть отделен от правил приложения. | Частично реализовано: `PromptSpec` и `PromptBuilder` есть, но critic payload пока raw f-string, auxiliary tasks пишут пустой system prompt. | Закрыть gaps, не переписывать уже рабочий каркас. |
| Family-aware rendering | OpenAI reasoning best practices отличают reasoning prompting от обычных GPT; Claude best practices подтверждают XML-разметку и явные границы данных. | Реализованы `gpt`, `reasoning`, `agentic` renderers и `resolve_family`, но GPT/reasoning форма ещё не описана как единый XML envelope. | Сохранить семейные различия, но привести rendered system prompt к XML envelope для всех семейств. |
| Provider/model capability registry | OpenRouter/OpenAPI показывают, что у моделей есть `canonical_slug`, `architecture`, `reasoning`, `supported_parameters`; параметры `tools`, `tool_choice`, `parallel_tool_calls`, `structured_outputs`, `reasoning_effort` поддерживаются не всеми моделями одинаково. | Сейчас `resolve_family` выбирает prompt shape по env patterns, но не хранит capability profile и не решает reasoning/tools per request. | Добавить provider-agnostic capability registry и preflight decision перед основным LLM call. |
| Reasoning minimalism | Reasoning-моделям нужны цель, ограничения, критерии, формат; не надо тащить chain-of-thought и чрезмерные пошаговые инструкции. | `_render_reasoning` уже краткий и без examples/CoT. | Добавить тесты, что все новые specs не вводят CoT-инструкции для reasoning. |
| XML/agentic shape | Claude best practices подтверждают XML tags для инструкций, контекста, входа и формата; этот вывод используем как общий XML-friendly renderer, а не как hard-code под одного провайдера. | `_render_agentic` уже XML-shaped. | Не заводить отдельный provider-specific renderer без фактической необходимости. Настроить routing patterns для XML/tool-friendly моделей в `agentic`. |
| Rich output format | OpenAI/Claude guidance требует явно задавать формат результата; в DoubleU уже есть `text_format=rich`, `decide_text_format(...)` и `normalize_rich_markdown(...)`. | `draft_spec` уже выбирает `_RICH_OUTPUT_FORMAT` для role-mode, но план не требовал XML `<output_format>` и rich/plain acceptance snapshots. | Зафиксировать rich/plain как XML output-format контракт: rich => Telegram Rich Markdown, plain => no markdown. |
| Tool-use contract | Для tool calling недостаточно короткого label: модели нужны условия применения, параметры, side effects и смысл пустого результата. | `ToolCatalogEntry` содержит только `key/label/description/category/default_enabled`; docstrings короткие. | Добавить расширенный prompt-facing tool policy: `use_when`, `do_not_use_when`, parameter rules, side effects, privacy, idempotency. |
| Structured outputs | OpenAI/Claude источники подтверждают строгий формат результата для машинной обработки. | В агентах уже есть typed outputs, но часть prompts и `_record_run` не дают rendered system prompt. | Не изобретать новый parser. Усилить schema/trace tests и non-empty rendered system prompt. |
| Evaluation loop | Источники рекомендуют проверять prompts на примерах и критериях качества; Langfuse уже дал реальные regression cases. | Есть unit tests, но нет source-grounded Langfuse/prompt regression gate. | Добавить replay/eval gate по двум трассам из анализа и budgets для tool calls/tokens. |

## Гипотезы, которые отбрасываются как уже реализованные

- "Нужно добавить секции role/task/context/constraints". Уже реализовано в `PromptSpec` и GPT
  renderer. В фазе 2 остается только проверка покрытия всех prompt families.
- "Нужно сделать отдельный prompt для reasoning". Уже реализовано `_render_reasoning`; улучшаем
  тесты и routing, не переписываем форму.
- "Нужно XML-представление только для XML-friendly/agentic моделей". Отбрасывается как слишком
  узкое: XML envelope должен стать общей формой system prompt, а семейство модели меняет только
  плотность и содержание секций.
- "Нужно перечислить бренды и вручную решить, кто к какому семейству относится". Это неверная
  формулировка для OpenRouter: GLM/Kimi/DeepSeek/Qwen/MiniMax и другие модели должны
  классифицироваться по `architecture`, `reasoning`, `supported_parameters` и runtime reliability,
  а не только по названию автора.
- "Нужно изолировать user text в untrusted envelope". Базовый `PromptBuilder` это делает. В фазе
  закрываем исключения: critic и auxiliary paths.
- "Нужно добавить typed output вообще". Typed outputs уже используются. В фазе 2 проверяем строгие
  схемы, trace visibility и отсутствие ручного строкового parsing там, где уже есть structured output.

## Гипотезы, которые остаются в фазе 2

1. Новый builder должен стать единственным runtime path, иначе флаг скрывает реальные дефекты и
   мешает regression evidence.
2. Все LLM tasks должны иметь rendered system prompt из `PromptSpec`; пустой system prompt допустим
   только для metadata spans, но не для реального `app.llm.complete`.
3. Tool selection надо сделать prompt-contract частью, а не UI-only description.
4. Orchestrator должен быть дешевле и чище: dedup, call budget, provider compatibility guard,
   единый `final_result`.
5. Critic должен соблюдать тот же trust boundary: rendered `critic_spec` в system, raw message/draft
   в untrusted user payload.
6. Observability должна показывать concrete model slug и rendered system prompt, иначе сравнение
   семейств и стоимости ненадежно.
7. Перед основным вызовом нужен короткий deterministic preflight: определить, нужен ли reasoning,
   какие tool groups реально нужны, какую модель/capabilities можно использовать, и с какими
   request parameters отправлять основной запрос.

## Phase 2 implementation plan

### 2.1. Зафиксировать новый Prompt Contract как единственный путь

Файлы:

- `ai-python/ai_agent/config/flags.py`
- `ai-python/ai_agent/agents/runtime.py`
- `ai-python/ai_agent/pipelines/draft.py`
- `ai-python/ai_agent/prompts/library.py`
- `.env.example`
- `docker-compose.yml`

Работы:

- Убрать `PROMPT_CONTRACT_ENABLED` из product decision path. На переходном шаге можно оставить
  no-op env только для совместимости запуска, но runtime не должен выбирать legacy.
- Удалить legacy prompt constants и ветки после того, как тесты покрывают writer/orchestrator/critic.
- Не улучшать `_compose_base_instruction`: это старый путь.

Acceptance:

- В runtime нет ветки "legacy vs prompt contract" для реальных LLM calls.
- Existing tests больше не включают новый builder флагом.
- Langfuse для writer/orchestrator/critic показывает rendered system prompt.

### 2.2. Перевести оставшиеся prompt families в `PromptSpec`

Добавить builders в `ai-python/ai_agent/prompts/library.py`:

- `extract_spec()`: извлекает только durable facts/profile suggestions; raw history и message идут
  в untrusted payload; privacy flags не обходятся prompt-ом.
- `summary_spec()`: summary/action_items/recommendations, без raw dump сообщений и без сохранения
  приватных деталей.
- `topic_spec()`: короткая именованная тема 3-7 слов; recent messages только контекст.
- `rerank_spec()`: возвращает только ids из переданного набора; не выдумывает ids; держать
  минимальным, так как endpoint кандидат на отдельный removal после shadow-measure.
- `pattern_spec()`: синтезирует только устойчивые повторяемые паттерны, не превращает единичный факт
  в глобальное правило.
- `role_draft_spec()`: role draft из текста/аудио; `name/description/speaking_style/goals/constraints`
  и custom fields; raw text/audio не логировать.

Acceptance:

- У каждого builder есть тест, что raw user text не попадает в `PromptSpec.context`.
- Все builders рендерятся в `gpt`, `reasoning`, `agentic` как XML envelope:
  `<role>`, `<task>`, `<trusted_context>`, `<constraints>`, `<output_format>`,
  `<success_criteria>`; family-specific различия допускаются только внутри этих границ.
- Reasoning render остаётся кратким: XML tags сохраняют границы данных, но не добавляют CoT,
  examples-bloat или инструкции "думай пошагово".
- Rich/plain render snapshot проверяет `<output_format>`:
  `telegram_rich_markdown` для rich mode и `plain_text` без markdown для plain/copy/short intents.
- `_record_run` получает non-empty rendered system prompt для реального LLM call.

### 2.3. Закрыть critic trust boundary

Файл:

- `ai-python/ai_agent/agents/runtime.py`

Работы:

- В `_review_draft` оставить `critic_spec(...)` как trusted system instruction.
- `message_text` и `drafted.reply` передавать через `PromptBuilder.build(...)` как untrusted
  payload, а не raw f-string.
- `feedback_hint` классифицировать отдельно: если это sanitized owner-approved style example, он
  может остаться в `critic_spec.examples`; если там есть raw chat/user text, он должен идти через
  untrusted context/payload.
- Сохранить behavior critic failures: review advisory, оригинальный draft остается fallback.

Acceptance:

- Тест фиксирует, что `message_text` и `drafted.reply` обернуты в untrusted tags.
- Тест фиксирует, что raw message/draft отсутствует в `PromptSpec.context`.
- Тест фиксирует выбранную классификацию `feedback_hint`.
- Langfuse critic span показывает rendered critic system prompt и separated user payload.

### 2.4. Расширить prompt-facing tool policy

Файлы:

- `ai-python/ai_agent/agents/tool_catalog.py`
- `ai-python/ai_agent/agents/interaction_tools.py`
- `ai-python/ai_agent/agents/schedule_tools.py`
- delivery/read-memory tool modules, если присутствуют в текущей ветке
- `docs/reference/tools.md`
- Go/contracts только если поле становится UI/API-visible

Добавить Python-side metadata, например `ToolPromptPolicy`:

- `tool_name`
- `purpose`
- `use_when`
- `do_not_use_when`
- `parameters`
- `side_effect`: `none`, `staged_memory_write`, `staged_schedule`, `staged_interaction`,
  `staged_delivery`
- `privacy`
- `idempotency`
- `result_semantics`

Правила:

- UI `TOOL_CATALOG.description` может остаться коротким.
- Orchestrator prompt/tool descriptions должны получать расширенную policy.
- Tool docstrings должны быть синхронизированы с policy тестом, чтобы pydantic schema не
  расходилась с prompt contract.
- Для read tools явно описать:
  - `get_contact_profile` не вызывать, если профиль уже есть в trusted context;
  - `get_recent_messages` не вызывать для первого сообщения с достаточным контекстом;
  - `search_facts` использовать только для памяти этого чата, не для общих знаний.

Acceptance:

- Каждому native tool соответствует policy.
- Тест проверяет coverage `TOOL_NAME_TO_KEY -> ToolPromptPolicy`.
- Prompt snapshot для orchestrator содержит применимость инструментов, а не только короткие labels.

### 2.5. Починить orchestration hygiene из Langfuse

Файлы:

- `ai-python/ai_agent/agents/runtime.py`
- tool wrappers/deps modules
- `ai-python/tests/test_agents.py`

Работы:

- Two-stage split: writer по умолчанию не получает read/schedule tools. Read/schedule/interaction
  остаются в orchestrator; writer получает bundled context и delivery/final output surface.
- Dedup tool calls по `(tool_name, normalized_args)` в рамках одного turn. Повтор возвращает
  cached result или deterministic "already called" response, не новый DB/tool hit.
- No-arg schema guard для `get_contact_profile`, `list_reminders` и похожих tools: malformed args
  вида `{}""` не должны приводить к повторным попыткам.
- Provider compatibility guard: если provider/model стабильно дает `400 invalid request params` или
  malformed JSON для tool-heavy orchestrator, не маршрутизировать его на tool-heavy path.
- `final_result` должен быть единственным terminal call. Повторный `final_result` в одном run -
  ошибка prompt/runtime policy.
- Call budget: simple role/capability question <= 3 tool calls; memory/history-heavy answer <= 5
  tool calls unless explicitly justified by interaction/scheduler flow.

Acceptance:

- Regression tests на два Langfuse сценария из `.claude/analyze.md`.
- Нет duplicate `final_result`.
- Нет повторного `get_contact_profile` с одинаковыми args.
- Нет malformed `{}""` в accepted tool calls.

### 2.6. Ввести provider-agnostic model capability registry

Файлы:

- `ai-python/ai_agent/prompts/families.py`
- model config / routing modules
- новая capability module, например `ai-python/ai_agent/agents/model_capabilities.py`
- `scripts/render_litellm_config.py`, если registry должен собираться из env/OpenRouter metadata
- Langfuse sink / `_record_run`
- tests around `model_used`

Работы:

- Разделить два понятия:
  - `prompt_shape_family`: `gpt`, `reasoning`, `agentic` - как рендерить инструкции;
  - `model_capabilities`: что реально можно отправить модели/provider через gateway.
- Не считать GPT/Claude полным списком семейств. OpenRouter model inventory должен позволять
  классифицировать OpenAI, Anthropic, Google, GLM, Kimi/Moonshot, DeepSeek, Qwen, MiniMax, Mistral,
  Llama, Cohere, Grok и любые новые модели без изменения кода под каждый бренд.
- Источник истины для capability:
  - текущие env/LiteLLM/OpenRouter slugs, которые реально доступны приложению;
  - OpenRouter `/models` metadata: `id`, `canonical_slug`, `architecture.instruct_type`,
    `architecture.tokenizer`, `reasoning`, `supported_parameters`, `context_length`,
    `top_provider`;
  - локальные overrides для известных провайдерных дефектов, например malformed tool JSON или
    `400 invalid request params` на tool-heavy path.
- Capability fields:
  - `supports_tools`;
  - `supports_tool_choice`;
  - `supports_parallel_tool_calls`;
  - `supports_structured_outputs` / `supports_response_format`;
  - `supports_reasoning`;
  - `reasoning_mandatory`;
  - `reasoning_default_enabled`;
  - `supported_reasoning_efforts`;
  - `supports_reasoning_max_tokens`;
  - `context_length`;
  - `json_tool_reliability`: `unknown`, `ok`, `degraded`, `blocked`;
  - `prompt_shape_family`.
- Для prompt shape использовать текущий `resolve_family` как fallback, но сначала пробовать
  registry metadata:
  - `reasoning` present или `architecture.instruct_type` вроде `deepseek-r1`, `qwq`, `qwen3` с
    reasoning profile -> `reasoning`;
  - XML/instruction-friendly agentic families, включая XML-friendly и tool-heavy reliable models ->
    `agentic`;
  - остальные chat/instruct models -> `gpt` shape как generic markdown contract.
- Для tool-heavy orchestrator учитывать не только prompt shape, но и capability: supports tools,
  supports strict JSON/schema, provider JSON reliability.
- Закрыть backlog item: `model_used` должен быть concrete slug, а не `smart`/`balanced`, если role
  задана через tier без `model_name`.
- В trace писать: `model_slug`, `canonical_slug`, `provider`, `prompt_shape_family`,
  `capability_profile_version`, `task`, `rendered_system_prompt_hash`, token counts,
  reasoning tokens, tool call count, fallback reason.

Acceptance:

- Tests фиксируют concrete `model_used`.
- Tests фиксируют capability parsing из OpenRouter-like model metadata.
- Tests фиксируют fallback через env overrides/patterns/default, если metadata недоступна.
- Tests покрывают не только OpenAI/Anthropic examples, но и representative slugs для
  `deepseek/*`, `qwen/*`, `moonshotai/*` или `kimi/*`, `z-ai/*` или `glm/*`, `minimax/*`.
- Langfuse позволяет сравнить prompt family и фактически вызванную модель.

### 2.7. Добавить короткий TaskPreflight перед основным запросом

Цель: перед дорогим/основным LLM call быстро решить, какой режим нужен этому turn: reasoning on/off,
какой effort, какие native tools давать, нужен ли orchestrator вообще, и какие request parameters
безопасно поддерживает выбранная модель.

Файлы:

- новая модель результата, например `TaskPreflightDecision` в `ai-python/ai_agent/agents/results.py`
  или отдельном module;
- `ai-python/ai_agent/agents/runtime.py`;
- `ai-python/ai_agent/prompts/library.py`;
- `ai-python/ai_agent/agents/tool_catalog.py`;
- tests in `ai-python/tests/test_agents.py` / `test_model_capabilities.py`.

Decision schema:

- `needs_reasoning`: bool;
- `reasoning_effort`: `none|minimal|low|medium|high|max`, ограниченный capability profile;
- `needs_orchestrator`: bool;
- `tool_groups`: subset of `memory`, `schedule`, `interaction`, `delivery`, `mcp`;
- `allowed_tool_names`: concrete names after role/privacy/capability filtering;
- `structured_output_required`: bool;
- `selected_model_slug`;
- `prompt_shape_family`;
- `reason`: короткая внутренняя диагностическая строка для trace, не для ответа пользователю.

Preflight rules:

- Делать его deterministic и дешёвым. Сначала rule-based классификатор по message/request state,
  role settings, privacy flags и known tasks. LLM-preflight разрешать только если rule-based score
  неоднозначен.
- Не слать все tools "на всякий случай". Если запрос можно ответить из текущего trusted context,
  `needs_orchestrator=false`, tools empty.
- Включать reasoning:
  - для многошаговых задач, планирования, конфликтующих ограничений, сложной tool orchestration,
    критичного self-check;
  - для critic/review, когда риск/уверенность требует проверки.
- Выключать или ставить `none/minimal`:
  - для простых бытовых ответов, greeting, small talk, прямого ответа по уже собранному контексту;
  - для задач, где модель/provider не поддерживает отключаемый reasoning или это дороже без пользы.
- Выбирать tools по минимальному набору:
  - `memory` только если нужен профиль/история/факты этого чата;
  - `schedule` только при явном намерении создать/изменить/отменить напоминание;
  - `interaction` только если нужен structured clarification до ответа;
  - `delivery` только на финальной стадии, а не в context-gathering;
  - `mcp` только если роль явно разрешает внешний инструмент и privacy/capability gates проходят.
- Если выбранная модель не поддерживает `tools`/`tool_choice`/`structured_outputs`, preflight должен
  либо выбрать другую модель из tier/fallback, либо отключить tool-heavy path и записать причину.
- OpenRouter request parameters формировать из decision + capability:
  - `reasoning: { effort }` или `reasoning_effort` только если supported;
  - effort `none` использовать только если модель допускает отключение и `mandatory=false`;
  - `tools`, `tool_choice`, `parallel_tool_calls` только если supported;
  - `response_format`/`structured_outputs` только если supported.

Acceptance:

- Простое "кто ты и что умеешь" -> no reasoning или minimal, no read tools если роль уже в trusted
  context, no duplicate `final_result`.
- "Поставь напоминание завтра" -> schedule tools only, reasoning minimal/low, strict time context.
- "Найди в памяти, что я говорил про X, и составь план" -> memory tools + reasoning medium.
- General knowledge question без памяти -> no memory tools.
- Provider без reliable tool JSON -> no tool-heavy orchestrator or different model.
- Langfuse trace показывает `TaskPreflightDecision` и фактические request params.

### 2.8. Eval и Langfuse regression gate

Добавить минимальный gate до дальнейшего расширения:

- replay fixtures для двух запросов из `.claude/analyze.md`;
- prompt snapshots для `gpt/reasoning/agentic`, все в XML envelope;
- rich/plain output-format snapshots: rich требует Telegram Rich Markdown, plain запрещает markdown;
- tool-call budgets;
- preflight decision snapshots: reasoning on/off, effort, selected tool groups;
- token budget trend относительно audited new path;
- assertion, что пустые `auto_reply`/`output_safety` observations не считаются пустыми LLM calls, а
  реальные prompts ищутся в `app.llm.complete`/`litellm-acompletion`.

Acceptance:

- `cd ai-python && MOCK_LLM=true python -m pytest tests/ -q`
- `cd backend-go && go test ./...` только если менялись Go-visible contracts/catalog.
- Langfuse smoke run: role/constraints в system, user data in untrusted tags, no duplicate terminal
  calls, no malformed tool args.

## Backlog boundary

Включить:

- Critic raw payload -> PromptBuilder.
- Concrete `model_used`.
- Provider-agnostic model capability registry.
- TaskPreflightDecision for reasoning on/off and minimal tool selection.
- Promptfoo/eval-style regression gate в минимальном виде, если это можно сделать без отдельной
  инфраструктуры.

Не включать:

- `integration` tool/outbound webhooks: это отдельный tools-platform security track с SSRF,
  allow-list и approval boundary.
- LLM Guard scanners: отдельный pre-prod security gate.
- Full rerank endpoint removal: только не ломать `rerank_spec`; removal после отдельного shadow
  measure.

## Self-review checklist

- Источники: каждая оставшаяся гипотеза подтверждена OpenAI/Claude, а не только локальным
  конспектом; OpenRouter-specific утверждения подтверждены OpenRouter docs/OpenAPI schema.
- Факты о моделях: не утверждать статично, что GLM/Kimi/DeepSeek/Qwen/MiniMax всегда относятся к
  одному prompt family или всегда поддерживают reasoning/tools. Проверять по metadata
  `reasoning`, `supported_parameters`, `architecture` и локальным overrides.
- Код: если гипотеза уже реализована (`PromptSpec`, renderers, untrusted envelope), она не стала
  задачей фазы 2.
- Trust boundary: raw user/message/draft/history не попадают в trusted `PromptSpec.context`.
- Reasoning: no "think step by step", no exposed chain-of-thought, no examples bloat для reasoning
  renderer; XML tags не считаются поводом раздувать reasoning prompt.
- XML contract: каждый renderer сохраняет явные границы role/task/context/constraints/tool_policy/
  output_format/success_criteria; отдельный provider-specific renderer не добавлен без доказанного
  divergence.
- Rich mode: если `decide_text_format(...)` возвращает `rich`, rendered `<output_format>` требует
  Telegram Rich Markdown; если `plain`, markdown явно запрещён.
- Tools: применимость реально видит orchestrator/tool schema, а не только `docs/reference/tools.md`.
- Preflight: reasoning/tool decision реально влияет на request params и allowed tools, а не только
  логируется.
- Langfuse: каждый дефект из `.claude/analyze.md` имеет test или runbook check.
- Worktree: документальный срез не смешан с текущими незавершенными контрактными изменениями.
