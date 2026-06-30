"""Prompt-facing applicability metadata for native tools.

Distinct from :class:`ai_agent.agents.tool_catalog.ToolCatalogEntry`, which is
the short, Go-mirrored, owner-facing label/description shown in the role UI.
``ToolPromptPolicy`` is the richer layer the orchestrator/writer prompts (and
each tool's own docstring — see the *_tools.py modules) draw from: a model
needs to be told not just THAT a tool exists but WHEN to call it and WHEN
not to, or it overtriggers on a short label alone (Claude 4.6 in particular —
see the project's prompting source principles).

Keyed by tool *function* name (matching
:data:`ai_agent.agents.tool_catalog.TOOL_NAME_TO_KEY`), not by catalog key,
because that is what a :class:`pydantic_ai.toolsets.FilteredToolset` actually
filters on and what ``enabled_tool_names`` returns. ``send_message`` is
included even though it has no catalog entry (it is the writer's always-on
delivery tool, never gated by ``tool_settings``).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from types import MappingProxyType
from typing import Literal, Mapping

SideEffect = Literal[
    "none", "staged_memory_write", "staged_schedule", "staged_interaction", "staged_delivery"
]


@dataclass(frozen=True)
class ToolPromptPolicy:
    """Applicability contract for one native tool function.

    ``use_when``/``do_not_use_when`` are the fields actually folded into the
    orchestrator/writer ``PromptSpec.tool_policy`` (see ``prompts/library.py``)
    — keep each entry one short clause, since every enabled tool's policy is
    rendered into the same turn's prompt. ``parameters``/``result_semantics``
    are documentation-of-record (``docs/reference/tools.md``) and the basis
    for each tool's own docstring; they are intentionally NOT re-rendered into
    the prompt text, since pydantic-ai already sends the docstring and the
    parameter schema to the model on every call regardless of prompt family.
    """

    tool_name: str
    purpose: str
    use_when: tuple[str, ...]
    do_not_use_when: tuple[str, ...]
    parameters: tuple[str, ...] = field(default_factory=tuple)
    side_effect: SideEffect = "none"
    privacy: str = ""
    idempotency: str = ""
    result_semantics: str = ""


TOOL_PROMPT_POLICIES: Mapping[str, ToolPromptPolicy] = MappingProxyType(
    {
        "search_facts": ToolPromptPolicy(
            tool_name="search_facts",
            purpose="Семантический/лексический поиск сохранённых фактов ЭТОГО чата по запросу.",
            use_when=(
                "нужны детали о собеседнике или договорённостях, которых нет в видимом контексте"
                " (предпочтения, биография, обязательства).",
            ),
            do_not_use_when=(
                "вопрос про общие знания, а не про этот чат — это память конкретного чата, не"
                " база общих фактов.",
                "нужный факт уже явно есть в переданном trusted-контексте.",
            ),
            parameters=("query — непустой запрос на естественном языке.", "limit — 1..10, по умолчанию 5."),
            side_effect="none",
            privacy="Пустой список при no_memory или отсутствии пула — не падает.",
            idempotency="Чистое чтение; повтор с тем же query безопасен, но избыточен — не дублируй.",
            result_semantics="Пустой список — фактов нет или память недоступна; не выдумывай факты вместо них.",
        ),
        "get_episode_summaries": ToolPromptPolicy(
            tool_name="get_episode_summaries",
            purpose="Сводки прошлых эпизодов общения в этом чате, свежие первыми.",
            use_when=(
                "нужен контекст более ранних разговоров, которых нет в видимой переписке.",
            ),
            do_not_use_when=("видимого контекста уже достаточно для ответа на текущее сообщение.",),
            parameters=("limit — 1..5, по умолчанию 3.",),
            side_effect="none",
            privacy="Пустой список при no_memory или отсутствии пула.",
            idempotency="Чистое чтение, повтор безопасен но избыточен.",
            result_semantics="Пустой список — эпизодов нет или память недоступна.",
        ),
        "get_recent_messages": ToolPromptPolicy(
            tool_name="get_recent_messages",
            purpose="Последние сообщения этого чата, старые первыми.",
            use_when=("видимого контекста не хватает, чтобы понять ход разговора.",),
            do_not_use_when=(
                "это первое сообщение от собеседника при достаточном видимом контексте — истории"
                " ещё нет, вызов бессмысленен.",
                "видимый контекст уже содержит нужную историю.",
            ),
            parameters=("limit — 1..30, по умолчанию 10.",),
            side_effect="none",
            privacy="Пустой список при no_memory или отсутствии пула.",
            idempotency="Чистое чтение.",
            result_semantics="Пустой список — сообщений нет или память недоступна.",
        ),
        "get_contact_profile": ToolPromptPolicy(
            tool_name="get_contact_profile",
            purpose="Сохранённый владельцем профиль собеседника (имя, тон, отношения, обращение).",
            use_when=(
                "в системной инструкции нет данных о собеседнике, а тон/обращение важны для ответа.",
            ),
            do_not_use_when=(
                "профиль собеседника уже явно описан в trusted-контексте этого хода — повтор не"
                " добавит новых данных.",
            ),
            parameters=(),
            side_effect="none",
            privacy="Пустой dict при no_memory или отсутствии пула.",
            idempotency="Чистое чтение без аргументов — повтор в одном ходе возвращает то же самое.",
            result_semantics="Пустой dict — профиль не заполнен владельцем; не выдумывай имя/тон вместо него.",
        ),
        "save_fact": ToolPromptPolicy(
            tool_name="save_fact",
            purpose="Сохранить долговременный факт в память этого чата (дедуп через upsert-процедуру).",
            use_when=(
                "в сообщении прозвучал устойчивый факт о собеседнике/договорённости, которого ещё"
                " нет в памяти (профессия, место учёбы/жительства, семья, стойкое предпочтение,"
                " обязательство с дедлайном).",
            ),
            do_not_use_when=(
                "факт сиюминутный (настроение, разовая реплика, временное состояние) — не"
                " долговременная память.",
                "факт уже присутствует в переданном контексте памяти этого чата.",
            ),
            parameters=(
                "fact_text — 5..120 символов.",
                "fact_type — preference/biographical/commitment/event/relationship/health/taboo.",
                "subject — contact/owner/relationship/third_party.",
                "confidence — 0..1, высокое значение только если факт назван явно и однозначно.",
            ),
            side_effect="staged_memory_write",
            privacy="No-op-сообщение при no_memory или отсутствии пула — не пишет.",
            idempotency="Дедуп по хэшу+косинусу на уровне процедуры; повтор не создаёт дубликат.",
            result_semantics="Текстовый статус ('saved (<action>)' либо ошибка) — не structured success/failure.",
        ),
        "suggest_profile_update": ToolPromptPolicy(
            tool_name="suggest_profile_update",
            purpose="Предложить владельцу обновление ОДНОГО поля профиля собеседника — без прямого изменения.",
            use_when=(
                "сообщение явно раскрывает атрибут идентичности собеседника (отношения, обращение,"
                " язык, тон, пол), отличающийся от текущего профиля.",
            ),
            do_not_use_when=(
                "атрибут уже совпадает с текущим профилем.",
                "информация неявная/предположительная, а не явно сказанная собеседником.",
            ),
            parameters=(
                "field_name — relationship_type/preferred_address/language/tone/gender.",
                "suggested_value — до 64 символов.",
                "rationale — короткая цитата-доказательство, до 120 символов.",
            ),
            side_effect="staged_memory_write",
            privacy="No-op-сообщение при no_memory или отсутствии пула.",
            idempotency="ON CONFLICT DO NOTHING — повторное идентичное предложение не дублирует pending-запись.",
            result_semantics=(
                "'staged for owner review' либо 'an identical suggestion is already pending' —"
                " профиль НЕ меняется этим вызовом, решает только владелец."
            ),
        ),
        "ask_questions": ToolPromptPolicy(
            tool_name="ask_questions",
            purpose="Задать собеседнику пул из 1–6 вопросов с кнопками-подсказками; бот покажет их по одному.",
            use_when=(
                "нужно задать ЛЮБОЙ вопрос с выбором перед финальным ответом — уточнение, опрос,"
                " тест/викторину.",
            ),
            do_not_use_when=(
                "вопрос не предполагает выбора — обычный открытый вопрос идёт в тексте финального"
                " ответа, не сюда.",
                "форма ответа для этого хода уже задана (не вызывай второй раз за один ход).",
            ),
            parameters=(
                "questions — 1..6 объектов {prompt, options[0..8]}; options опциональны (пустой"
                " список = свободный текстовый вопрос); для викторины — по одному вопросу за вызов.",
            ),
            side_effect="staged_interaction",
            privacy="Доступен только когда роль явно включает interaction.ask_questions.",
            idempotency="Перезаписывает форму этого хода — не вызывай больше одного раза за ход.",
            result_semantics=(
                "Текстовое подтверждение постановки формы — сама форма уходит собеседнику ПОСЛЕ"
                " хода; текст вопроса не должен повторяться в финальном reply."
            ),
        ),
        "request_confirmation": ToolPromptPolicy(
            tool_name="request_confirmation",
            purpose="Частный случай ask_questions: один вопрос да/нет с кнопками «Подтвердить»/«Отменить».",
            use_when=("нужно явное да/нет подтверждение перед действием, и хватает одного вопроса.",),
            do_not_use_when=(
                "нужно больше одного вопроса или нестандартные варианты ответа — используй"
                " ask_questions напрямую.",
            ),
            parameters=("prompt — текст вопроса.",),
            side_effect="staged_interaction",
            privacy="Та же гейтинг-логика, что у ask_questions (это его обёртка).",
            idempotency="Делегирует в ask_questions — те же правила перезаписи формы.",
            result_semantics="Как у ask_questions.",
        ),
        "create_reminder": ToolPromptPolicy(
            tool_name="create_reminder",
            purpose="Создать одно будущее напоминание для владельца или собеседника (запись, не отправка).",
            use_when=("из сообщения явно следует поставить напоминание на конкретное будущее время.",),
            do_not_use_when=(
                "названное время уже прошло и перенос неоднозначен — сначала задай уточняющий"
                " вопрос (ask_questions), не угадывай время.",
                "намерение поставить напоминание неясно или предположительно.",
            ),
            parameters=(
                "fire_at_iso — ISO-8601 со смещением (НЕ голое локальное время); вычисляй из"
                " текущего времени владельца в системной инструкции.",
                "text — непустой текст напоминания.",
                "target — owner или contact.",
            ),
            side_effect="staged_schedule",
            privacy="Error-строка при no_memory/private — гейтится и тулсетом, и внутри тула.",
            idempotency=(
                "SHA-256 ключ от (tenant, chat, source_message, fire_at, target, text) — повтор в"
                " рамках того же исходного сообщения не создаёт дубликат job."
            ),
            result_semantics="'scheduled reminder for <iso>' либо 'error: ...' — читай текст.",
        ),
        "list_reminders": ToolPromptPolicy(
            tool_name="list_reminders",
            purpose="Показать активные напоминания этого чата (id, время, текст).",
            use_when=("нужно сослаться на существующее напоминание или узнать id перед отменой.",),
            do_not_use_when=(
                "роль не включает list_reminders — existing_jobs тогда не передаются вовсе, вызов"
                " вернёт пусто независимо от реальных данных.",
            ),
            parameters=(),
            side_effect="none",
            privacy="Error-строка при no_memory/private.",
            idempotency="Чистое чтение из уже переданных данных (без обращения к БД) — повтор безопасен.",
            result_semantics="'Активных напоминаний нет.' — действительно нет pending-задач, не ошибка.",
        ),
        "cancel_reminder": ToolPromptPolicy(
            tool_name="cancel_reminder",
            purpose="Отменить одно pending-напоминание этого чата по числовому id (из list_reminders).",
            use_when=(
                "явно просят отменить уже поставленное напоминание, и его id известен (через"
                " list_reminders).",
            ),
            do_not_use_when=("id не известен — сначала вызови list_reminders, не угадывай номер.",),
            parameters=("reminder_id — числовой id из list_reminders для этого чата.",),
            side_effect="staged_schedule",
            privacy="Error-строка при no_memory/private.",
            idempotency="Повторная отмена того же id — no-op, безопасно.",
            result_semantics=(
                "'reminder #N cancelled' либо 'error: no pending reminder with id N' — второе"
                " означает неверный/чужой/неактивный id, не системную ошибку."
            ),
        ),
        "send_message": ToolPromptPolicy(
            tool_name="send_message",
            purpose="Доставить отдельное сообщение собеседнику сейчас и продолжить ход (writer-only).",
            use_when=(
                "в одном ходе нужно отправить РАЗНЫЕ по характеру части — например текст, а ПОТОМ"
                " вопрос — каждую отдельным сообщением.",
            ),
            do_not_use_when=(
                "ответ — одна короткая цельная реплика; не разбивай то, что естественно идёт одним"
                " сообщением.",
                "это вопрос с кнопками — для него используй ask_questions, не send_message.",
            ),
            parameters=("text — непустой текст, до 4000 символов; не более 4 вызовов за ход.",),
            side_effect="staged_delivery",
            privacy="Не гейтится privacy-флагами — это не память, доступен всегда писателю.",
            idempotency="Каждый вызов добавляет НОВОЕ сообщение в очередь (не дедуп) — не вызывай с тем же text дважды.",
            result_semantics=(
                "'queued message #N for delivery' — сообщение уйдёт ПОСЛЕ хода, до финального"
                " reply; ModelRetry при превышении лимита или пустом тексте, не тихий провал."
            ),
        ),
    }
)


def render_policy(policy: ToolPromptPolicy) -> str:
    """Render one tool's applicability as a compact prompt-facing sentence."""
    parts = [f"{policy.tool_name}: {policy.purpose}"]
    if policy.use_when:
        parts.append("Используй когда: " + " ".join(policy.use_when))
    if policy.do_not_use_when:
        parts.append("НЕ используй когда: " + " ".join(policy.do_not_use_when))
    return " ".join(parts)


def applicable_policy_text(tool_names: frozenset[str]) -> tuple[str, ...]:
    """Render policy lines for the tools actually enabled this turn, in stable order.

    Silently skips any name without a registered policy rather than raising —
    callers pass whatever a toolset filter computed, and a tool with no
    prompt-facing policy yet should degrade to "no extra guidance", not break
    the turn. ``test_tool_policy.py`` is what enforces full coverage.
    """
    return tuple(
        render_policy(TOOL_PROMPT_POLICIES[name])
        for name in sorted(tool_names)
        if name in TOOL_PROMPT_POLICIES
    )
