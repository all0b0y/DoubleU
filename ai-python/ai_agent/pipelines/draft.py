"""Draft prompt building blocks + policy preflight for ``/ai/process-message``.

Owns the no-IO building blocks the draft path shares: the no-LLM policy gates
(:func:`preflight_outcome`), the data-driven formatters
(:func:`_format_persona_section`, :func:`_format_owner_identity`,
:func:`_format_interlocutor_section`, :func:`_greeting_rule`, ...) that
``ai_agent.prompts.library`` assembles into each call's :class:`PromptSpec`,
and the :class:`DraftOutcome` contract.  The actual generation runs in the
pydantic-ai :class:`ai_agent.agents.runtime.AgentRuntime`; pure helpers stay
at module scope so they can be tested independently.

Prompting model: the AI plays the **role** the owner described for this chat
(``ai_role``).  A role may speak either on the owner's behalf or openly as a
bot persona; only the former may inherit the owner's identity and style.  When
no role is assigned the default persona is "ассистент владельца" — reply in the
owner's observed per-chat style (the legacy digital-copy behaviour, now just
the fallback).  Memory is strictly per-chat: the prompt only ever sees the
context of the current chat.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from datetime import date, datetime, timezone
from typing import Literal
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from ..schemas import (
    AIProcessMessageRequest,
    AIRoleData,
    ChatMode,
    ContactProfileData,
    OutboundMessageData,
    PrivacyFlag,
    ResponsePlanData,
    RiskLevel,
    ScheduledJobData,
    Usage,
    UserSettingsData,
)


# Risk reasons that signal the message must be handled manually by the owner.
# When matched, the draft path returns a canned owner-facing notification
# instead of generating a reply.
_MANUAL_REASONS = frozenset({"profanity_or_obscene_language", "sexual_content"})

# Owner-attached role reference material is truncated to this many characters
# before prompt injection so it cannot crowd out the conversation context.
_ROLE_CONTEXT_MAX_CHARS = 6000

# Russian greeting tokens used to detect whether the owner already greeted
# the contact today (and therefore should not greet again on this turn).
_GREETING_WORDS = ("привет", "здравствуй", "добрый", "добро", "hi", "hello", "hey", "хай")

# Matches a timestamped memory-context line like ``[2026-05-13 10:00] OWNER: ...``.
_DATE_PATTERN = re.compile(r"\[(\d{4}-\d{2}-\d{2})")

TextFormat = Literal["plain", "rich"]

# Only terse confirmations stay plain. choice_question is NOT here: those replies
# (e.g. quiz questions) legitimately carry Rich Markdown, and forcing plain made
# Telegram show raw ** / # / numbered lists. Rich send supports inline keyboards,
# so option buttons are unaffected; when rich is disabled the Go side degrades
# markdown to HTML, so plain is never needed to avoid raw markup.
_PLAIN_FORMAT_INTENTS = frozenset({"acknowledgement"})

_WRITER_TWO_STAGE_DIRECTIVE = (
    "\n\nКОНТЕКСТ УЖЕ СОБРАН на предыдущей стадии и передан ниже в контекстном блоке —"
    " опирайся в первую очередь на него. Инструменты у тебя есть, но это ЗАПАСНОЙ путь:"
    " вызывай их только если для точного ответа критично не хватает данных, которые"
    " предыдущая стадия упустила. НЕ дублируй уже собранный контекст и НЕ задавай"
    " вопросы повторно, если форма ответа уже задана. Если данных хватает — просто"
    " пиши ответ, ничего не выдумывая."
)


@dataclass(frozen=True)
class DraftOutcome:
    """Result of the draft stage of ``/ai/process-message``.

    ``intent``/``confidence`` are ``None`` when the LLM did not return valid
    structured output — the caller falls back to the regex classifier and the
    synthetic confidence (ARCH-4 graceful degradation).
    """

    text: str
    model_used: str
    usage: Usage
    # Owner-facing warning; never sendable to the contact (ARCH-1).
    owner_note: str = ""
    intent: str | None = None
    text_format: TextFormat = "plain"
    options: tuple[str, ...] = field(default_factory=tuple)
    response_plan: ResponsePlanData | None = None
    scheduled_jobs: tuple[ScheduledJobData, ...] = field(default_factory=tuple)
    cancelled_job_ids: tuple[int, ...] = field(default_factory=tuple)
    confidence: float | None = None
    topic: str = ""
    # Ordered prose messages from the send_message tool, delivered before the
    # terminal reply/questionnaire so one turn can become several messages.
    messages: tuple[OutboundMessageData, ...] = field(default_factory=tuple)


def preflight_outcome(
    request: AIProcessMessageRequest,
    risk_level: RiskLevel,
    risk_reasons: tuple[str, ...] = (),
) -> DraftOutcome | None:
    """Apply the no-LLM gates shared by every draft path.

    Returns a terminal :class:`DraftOutcome` for disabled/silent chats and
    critical risk (the manual-reason fast paths set ``owner_note`` so the
    warning can never be sent to the contact — ARCH-1), or ``None`` when the
    LLM call should proceed.  Shared by the legacy router path and the
    pydantic-ai agent runtime so the policy gates exist exactly once.
    """
    mode = request.chat_settings.mode
    if mode in (ChatMode.DISABLED, ChatMode.SILENT):
        return DraftOutcome(text="", model_used="none", usage=Usage())

    if risk_level == RiskLevel.CRITICAL and _MANUAL_REASONS & set(risk_reasons):
        if "sexual_content" in risk_reasons:
            note = "⚠️ Сообщение интимного характера. ИИ не формирует ответ — ответьте вручную."
        else:
            note = "⚠️ Сообщение содержит нецензурную лексику. ИИ не формирует ответ — ответьте вручную или проигнорируйте."
        return DraftOutcome(text="", model_used="none", usage=Usage(), owner_note=note)
    if risk_level == RiskLevel.CRITICAL:
        return DraftOutcome(text="", model_used="none", usage=Usage())
    return None


def _scheduler_available(request: AIProcessMessageRequest) -> bool:
    if request.ai_role is None:
        return False
    if PrivacyFlag.NO_MEMORY in request.chat_settings.privacy_flags:
        return False
    if PrivacyFlag.PRIVATE in request.chat_settings.privacy_flags:
        return False
    return request.ai_role.tool_settings.get("scheduler.create_reminder", False)


def _interaction_available(request: AIProcessMessageRequest) -> bool:
    if request.ai_role is None:
        return False
    return request.ai_role.tool_settings.get("interaction.ask_questions", False)


def _interaction_instruction() -> str:
    return (
        "ИНСТРУМЕНТ ВОПРОСОВ (ask_questions): по умолчанию НЕ задавай вопросов — отвечай сразу по "
        "сути, используя то, что собеседник уже написал, и разумные допущения. Вызывай "
        "ask_questions(questions[1..6]) ТОЛЬКО когда без конкретного факта ты объективно не можешь "
        "дать полезный ответ, и этого факта нет в переписке, и его нельзя разумно предположить. "
        "НЕ задавай вопрос, ответ на который собеседник уже дал; НЕ спрашивай, если он попросил "
        "решить самому («сам реши», «на твоё усмотрение», «распредели сам») — просто сделай работу; "
        "и НЕ пристёгивай уточняющий вопрос к уже полному, готовому ответу ради продолжения диалога. "
        "Если собеседник прямо попросил формат «сначала объясни/проведи лекцию, потом задай вопрос/"
        "вопросы/проверь меня», не обещай задать вопросы позже: дай содержательную часть в reply или "
        "send_message, а первый проверочный вопрос оформи в этом же ходе через ask_questions. "
        "Каждый вопрос = {prompt, options[0..8]}. prompt — это сам вопрос словами (для подтверждения — "
        "явный да/нет-вопрос, например «Сохранить этот план?»), НИКОГДА не многоточие, тире или "
        "заглушка. options — короткие, конкретные и взаимоисключающие варианты ИМЕННО для этого "
        "вопроса, выведенные из текущего разговора; никогда не подставляй общий шаблонный список "
        "(подарок/ужин/фильм и т.п.), не относящийся к теме. Если готовых вариантов нет — оставь "
        "options пустым: собеседник ответит своими словами через эту же форму. prompt и options "
        "пиши на языке собеседника (на том же языке, что и его последнее сообщение), не "
        "переключаясь на другой язык. Так бот покажет варианты КНОПКАМИ, и "
        "ответ однозначно привяжется к вопросу; НИКОГДА не дублируй варианты цифрами или списком в "
        "обычном тексте. Для викторины задавай ПО ОДНОМУ вопросу за ход; реакцию на прошлый ответ "
        "(например «Верно!») клади в начало prompt следующего вопроса. "
        "request_confirmation(prompt, confirm_label, cancel_label) — только обёртка для одного "
        "да/нет-подтверждения; ОБЯЗАТЕЛЬНО передавай обе подписи кнопок на языке prompt, без "
        "универсальных шаблонов. "
        "ВАЖНО: текст самого вопроса живёт ТОЛЬКО в ask_questions. НИКОГДА не повторяй этот вопрос "
        "в финальном ответе (reply), send_message или обычном сообщении. Если перед вопросом есть "
        "что сказать по сути — скажи в reply только этот текст ДО вопроса; если нет — оставь reply "
        "пустым, не выдумывай подводку ради подводки. Если gateway не выполнил tool call "
        "ask_questions, заполни поле structured output questions тем же пулом вопросов и оставь "
        "reply пустым."
    )


def _message_split_instruction() -> str:
    return (
        "РАЗБИЕНИЕ НА СООБЩЕНИЯ (send_message): send_message(text) ставит отдельный "
        "контактный текст в очередь этого хода; фактическую отправку делает backend после ответа "
        "модели, в порядке вызовов. Используй его только для самостоятельной прозы, которая должна "
        "идти ДО финального reply или формы. Если ход заканчивается ask_questions, сам вопрос и "
        "варианты НЕ должны попадать в send_message или reply — только в ask_questions. Дроби "
        "только когда части действительно разные; обычную короткую реплику отправляй как один "
        "финальный ответ."
    )


_OWNER_TZ_UNKNOWN_MARKER = "ПОЯС ВЛАДЕЛЬЦА НЕИЗВЕСТЕН"


def _current_time_instruction(
    request: AIProcessMessageRequest, current_time: datetime | None
) -> str:
    """Ground the prompt in "now" and, when known, the owner's local time.

    ``user_settings.timezone == ""`` means the owner's zone is UNKNOWN (e.g. a
    bot-only owner who never opened the Mini App, whose browser-IANA sync never
    ran) — it must never be silently treated as UTC, since that produces a
    reminder fired hours off the owner's real wall-clock time with no signal to
    anyone. An IANA name that fails to resolve is treated the same way: the
    stored value cannot be trusted to mean what the owner thinks it means.
    """
    now_utc = (current_time or datetime.now(timezone.utc)).astimezone(timezone.utc)
    raw_timezone = (
        request.user_settings.timezone.strip() if request.user_settings is not None else ""
    )
    if not raw_timezone:
        return (
            f"{_OWNER_TZ_UNKNOWN_MARKER}: текущее время UTC: {now_utc.isoformat()}. "
            "Часовой пояс владельца не определён (бот-онли пользователь, Mini App ещё не "
            "синхронизировал IANA-пояс). При просьбе о напоминании на голое локальное время "
            "(«в 9», «напомни в 18:30», без даты и пояса) сначала уточни пояс или город "
            "владельца через ask_questions — никогда не предполагай UTC и не подставляй пояс "
            "собеседника."
        )
    try:
        local_now = now_utc.astimezone(ZoneInfo(raw_timezone))
    except ZoneInfoNotFoundError:
        return (
            f"{_OWNER_TZ_UNKNOWN_MARKER}: текущее время UTC: {now_utc.isoformat()}. "
            f"Сохранённый часовой пояс владельца ({raw_timezone!r}) не распознан как "
            "IANA-имя. При просьбе о напоминании на голое локальное время сначала уточни "
            "пояс или город владельца через ask_questions — никогда не предполагай UTC."
        )
    timezone_name = raw_timezone
    return (
        "ТЕКУЩЕЕ ВРЕМЯ ВЛАДЕЛЬЦА: "
        f"{local_now.isoformat()} ({timezone_name}); UTC: {now_utc.isoformat()}. "
        "Голое время в сообщении («в 9», «напомни в 18:30», без даты и пояса) — это локальное "
        "время ВЛАДЕЛЬЦА в указанном поясе; никогда не интерпретируй его как UTC и не как пояс "
        "собеседника. Если названный момент сегодня по локальному времени владельца уже прошёл, "
        "переноси напоминание на ближайшее будущее наступление (как правило, на следующий день); "
        "если перенос неоднозначен, сначала задай уточняющий вопрос вместо вызова инструмента. "
        "Для scheduler.create_reminder вычисляй fire_at_iso как абсолютную метку времени со "
        "смещением (offset), а не голое локальное время."
    )


def _split_memory_context(
    raw_memory: tuple[str, ...],
) -> tuple[str | None, str | None, list[str], list[str]]:
    """Separate trusted owner instructions from untrusted conversation context.

    ``[OWNER INSTRUCTION]`` and ``[CURRENT DRAFT]`` markers injected by the Go
    backend during draft regeneration must be promoted into trusted system
    instructions; if they stay in the user-facing context block the LLM tends
    to ignore them.  Returns ``(owner_instruction, current_draft, style_facts,
    clean_memory)`` — the last list preserves order for prompt assembly.
    """
    owner_instruction: str | None = None
    current_draft: str | None = None
    owner_style_facts: list[str] = []
    clean_memory: list[str] = []

    for item in raw_memory:
        if item.startswith("[OWNER INSTRUCTION]"):
            owner_instruction = item[len("[OWNER INSTRUCTION]"):].strip()
        elif item.startswith("[CURRENT DRAFT]"):
            current_draft = item[len("[CURRENT DRAFT]"):].strip()
        elif item.startswith("[OWNER STYLE]"):
            owner_style_facts.append(item[len("[OWNER STYLE]"):].strip())
        else:
            clean_memory.append(item)
    return owner_instruction, current_draft, owner_style_facts, clean_memory


def decide_text_format(ai_role: AIRoleData | None, intent: str | None) -> TextFormat:
    """Choose whether the model response may use Telegram Rich Markdown."""
    if intent in _PLAIN_FORMAT_INTENTS:
        return "plain"
    if ai_role is None:
        return "plain"
    return "rich"


# _HEADING_INLINE_RE matches a heading marker (# through ######) that is
# preceded by at least one non-newline character (possibly with whitespace
# between).  Telegram's Rich parser — like standard GFM — requires an
# empty line before a heading; the pattern captures the last non-newline
# character before the heading so the replacement can insert \n\n.
_HEADING_INLINE_RE = re.compile(r"([^\n])\s*(#{1,6} )")


def normalize_rich_markdown(text: str) -> str:
    """Normalize LLM-produced Rich Markdown before delivery.

    Fixes two common model output patterns that break Telegram's rich parser
    or produce visual artefacts:

    1. Heading markers appearing inline or after a single newline:
       ``"😊 ### Заголовок"`` → ``"😊\\n\\n### Заголовок"``.
       GFM requires a blank line before a heading; without it Telegram
       renders the ``###`` as literal text.

    2. Three or more consecutive newlines collapsed to two (Telegram
       ignores extra blank lines, so they waste characters and make the
       rich parse less predictable).

    Only applied when ``decide_text_format`` resolves to ``"rich"`` — plain
    responses are never touched.

    This function is idempotent: a second pass produces identical output.
    """
    # Separate any heading that trails non-newline text (incl. same-line emoji)
    # onto its own paragraph.  The replacement preserves the preceding character
    # and inserts exactly one blank line.
    text = _HEADING_INLINE_RE.sub(r"\1\n\n\2", text)
    # Collapse three or more newlines to exactly two.
    text = re.sub(r"\n{3,}", "\n\n", text)
    return text


def _greeting_rule(memory_context: tuple[str, ...]) -> str:
    """Determine the greeting policy based on temporal conversation history.

    Prefers the backend-supplied ``[RUNTIME: paused_hours=N]`` flag (authoritative,
    from ``view_chat_paused_hours`` — MEM-9) for the conversation gap; falls back
    to parsing ``[CURRENT DATE]`` and ``[YYYY-MM-DD HH:MM]`` timestamps.
    """
    current_date: date | None = None
    last_message_date: date | None = None
    paused_hours: float | None = None
    greeting_sent_today = False
    # When the Go backend supplies the authoritative greeting flag we trust it and
    # skip the per-line greeting-word scan (single source of truth). The scan
    # remains as a fallback for callers that do not inject the flag (e.g. the
    # /generate-draft path).
    greeting_flag_provided = False

    for line in memory_context:
        if line.startswith("[CURRENT DATE:"):
            # B: the header now carries owner-local datetime+offset (or a UTC
            # "unknown timezone" marker), not just a bare date — match the
            # leading YYYY-MM-DD regardless of what follows it.
            m = re.search(r"\[CURRENT DATE: (\d{4}-\d{2}-\d{2})", line)
            if m:
                try:
                    current_date = date.fromisoformat(m.group(1))
                except ValueError:
                    pass
            continue

        if line.startswith("[RUNTIME: greeting_sent_today="):
            greeting_flag_provided = True
            greeting_sent_today = "true" in line.lower()
            continue

        if line.startswith("[RUNTIME: paused_hours="):
            m = re.search(r"paused_hours=([\d.]+)", line)
            if m:
                try:
                    paused_hours = float(m.group(1))
                except ValueError:
                    pass
            continue

        m = _DATE_PATTERN.search(line)
        if not m:
            continue
        try:
            msg_date = date.fromisoformat(m.group(1))
        except ValueError:
            continue

        last_message_date = msg_date

        if (
            not greeting_flag_provided
            and current_date
            and msg_date == current_date
            and "OWNER:" in line
        ):
            text_part = line[line.find("] OWNER:") + 8:].lower()
            if any(g in text_part for g in _GREETING_WORDS):
                greeting_sent_today = True

    if current_date is not None and last_message_date is not None:
        days_gap = (current_date - last_message_date).days
    elif paused_hours is not None:
        # Backend-supplied pause (view_chat_paused_hours, MEM-9). Fallback only:
        # on the draft path the incoming message is already persisted, so the
        # view reports ~0 — the timestamped history above is the better signal
        # because it excludes the current message.
        days_gap = int(paused_hours // 24)
    else:
        return (
            "This appears to be the first ever message from this contact. "
            "A brief greeting is appropriate if the contact greeted you."
        )

    if days_gap == 0:
        if greeting_sent_today:
            return (
                "You already greeted this contact today — do NOT open with a greeting. "
                "Continue the conversation naturally without 'Привет', 'Hello', etc."
            )
        return (
            "This is an ongoing same-day conversation. "
            "Do NOT open with a greeting unless the contact greeted you in THIS message. "
            "Match the established tone."
        )

    if days_gap == 1:
        return (
            "The last message was YESTERDAY. A brief greeting like 'Привет!' or 'Добрый день!' "
            "is appropriate to open a new day of conversation."
        )

    if days_gap <= 7:
        return (
            f"The last message was {days_gap} days ago. "
            "A light greeting is appropriate to re-open the conversation."
        )

    return (
        f"The last message was {days_gap} days ago — a significant gap. "
        "Open with a warm greeting and acknowledge the gap if natural."
    )


def _format_persona_section(
    ai_role: AIRoleData | None,
    owner_style_facts: list[str],
    contact_profile: ContactProfileData | None,
) -> str:
    """Describe WHO is answering in this chat.

    With an assigned ``ai_role`` the AI plays that persona.  ``speaking_style``
    wins when provided; only owner-speaking roles may fall back to observed
    owner style.  Without a role the fallback persona is "ассистент владельца"
    — reproduce the owner's own per-chat style (owner-entered ``owner_style``
    plus accumulated ``[OWNER STYLE]`` facts), the legacy behaviour.
    """
    if ai_role is not None:
        lines = [f"Ты выступаешь в роли «{ai_role.name}» — эту роль владелец описал сам:"]
        if ai_role.description:
            lines.append(ai_role.description)
        if ai_role.speaking_style:
            lines.append(f"Стиль речи роли: {ai_role.speaking_style}.")
        elif ai_role.speak_as_owner:
            style_hint = _owner_style_hint(owner_style_facts, contact_profile)
            if style_hint:
                lines.append(f"Стиль речи не задан — ориентируйся на стиль владельца: {style_hint}.")
            else:
                lines.append(
                    "Стиль речи не задан — выведи его из описания роли и тона переписки."
                )
        else:
            lines.append(
                "Стиль речи не задан — выведи его только из описания роли и текущей темы."
            )
        if ai_role.goals:
            lines.append(f"Твои цели в переписке: {ai_role.goals}.")
        if ai_role.constraints:
            lines.append(f"Жёсткие ограничения роли (нарушать нельзя): {ai_role.constraints}.")
        if ai_role.context:
            # Owner-attached reference material (FAQ, pricing, docs). Wrapped in
            # XML tags so the model treats it as data, not instructions, and
            # truncated to keep the prompt inside the token budget.
            context = ai_role.context[:_ROLE_CONTEXT_MAX_CHARS]
            lines.append(
                "Справочные материалы роли — используй их для точных ответов"
                " (цены, условия, факты), но не цитируй целиком:\n"
                f"<role_reference>\n{context}\n</role_reference>"
            )
        for custom in ai_role.custom_fields:
            # Owner-defined extra role attributes: title = what it is,
            # description = why it matters, optional context = reference text.
            if not custom.title:
                continue
            line = f"{custom.title}: {custom.description}".rstrip(": ").strip()
            if custom.context:
                line += f" ({custom.context[:_ROLE_CONTEXT_MAX_CHARS]})"
            if line:
                lines.append(line)
        lines.append(
            "Держись этой роли в каждом ответе: её манера и границы важнее стиля"
            " сообщений OWNER в переписке."
        )
        return "\n".join(lines)

    style_hint = _owner_style_hint(owner_style_facts, contact_profile)
    if not style_hint:
        return (
            "Роль для чата не задана. Ты — ассистент владельца: изучи его сообщения"
            " (OWNER:) в контексте и отвечай в его стиле."
        )
    return (
        "Роль для чата не задана. Ты — ассистент владельца: пиши в его стиле: "
        + style_hint
        + ". Дополнительно копируй тон и длину сообщений OWNER из переписки."
    )


def _owner_style_hint(
    owner_style_facts: list[str], contact_profile: ContactProfileData | None
) -> str:
    """Join the owner-entered per-chat style with accumulated style facts.

    The owner-entered style (``owner_style`` — the "i"-button ``me_style`` /
    ``style_notes``) goes first; observed ``[OWNER STYLE]`` facts complement it.
    Returns "" when nothing is known.
    """
    style_parts: list[str] = []
    if contact_profile is not None and contact_profile.owner_style:
        style_parts.append(contact_profile.owner_style)
    style_parts.extend(owner_style_facts[:5])
    return "; ".join(style_parts)


# Maps the structured address_form enum to a natural-language instruction.
_ADDRESS_FORM_LABELS = {"ty": "на «ты»", "vy": "на «вы»"}


def _format_interlocutor_section(contact_profile: ContactProfileData | None) -> str:
    """Describe the contact from their profile, or fall back to inference from history.

    Renders the owner-supplied structured fields cleanly; the legacy ``notes`` JSON
    is never echoed raw (its ``me_style`` is surfaced in the OWNER section instead).
    """
    if contact_profile is None:
        return "Собеседник (CONTACT). Определи отношения и тон из истории переписки."
    parts: list[str] = []
    if contact_profile.name:
        parts.append(f"Имя: {contact_profile.name}")
    if contact_profile.gender:
        parts.append(f"Пол: {contact_profile.gender}")
    if contact_profile.role:
        parts.append(f"Роль: {contact_profile.role}")
    if contact_profile.relationship_type:
        parts.append(f"Тип отношений: {contact_profile.relationship_type}")
    if contact_profile.address_form:
        label = _ADDRESS_FORM_LABELS.get(contact_profile.address_form, contact_profile.address_form)
        parts.append(f"Обращение: {label}")
    if contact_profile.tone:
        parts.append(f"Тон: {contact_profile.tone}")
    if contact_profile.emoji_usage:
        parts.append(f"Эмодзи: {contact_profile.emoji_usage}")
    if contact_profile.chat_context:
        parts.append(f"О чём общаемся: {contact_profile.chat_context}")
    if contact_profile.preferred_address:
        parts.append(f"Предпочитаемое обращение: {contact_profile.preferred_address}")
    if contact_profile.language:
        parts.append(f"Язык собеседника: {contact_profile.language}")
    if contact_profile.contact_timezone:
        parts.append(f"Часовой пояс: {contact_profile.contact_timezone}")
    if contact_profile.topics:
        parts.append("Темы общения: " + ", ".join(contact_profile.topics))
    if contact_profile.do_not_discuss:
        parts.append("Не обсуждать: " + ", ".join(contact_profile.do_not_discuss))
    if not parts:
        return "Собеседник (CONTACT). Определи отношения и тон из истории переписки."
    return "\n".join(parts)


def _format_owner_identity(
    user_settings: UserSettingsData | None, *, speaks_as_owner: bool = True
) -> str:
    """Render owner identity/preferences as a compact reference line.

    Surfaces who is writing (name), in what timezone, and which language to
    prefer — signals the Go backend already sends but the prompt never used.
    """
    if user_settings is None:
        return ""
    bits: list[str] = []
    if user_settings.display_name:
        bits.append(f"имя: {user_settings.display_name}")
    if user_settings.timezone:
        bits.append(f"часовой пояс: {user_settings.timezone}")
    if user_settings.primary_language:
        bits.append(f"основной язык: {user_settings.primary_language}")
    if user_settings.language_for_drafts and user_settings.language_for_drafts not in ("", "auto"):
        bits.append(f"язык черновиков: {user_settings.language_for_drafts}")
    if user_settings.bio:
        bits.append(f"о себе: {user_settings.bio}")
    if not bits:
        return ""
    prefix = (
        "О владельце, от чьего имени идёт переписка (справочно): "
        if speaks_as_owner
        else "Профиль владельца (справочно): "
    )
    return prefix + "; ".join(bits)


def build_feedback_hint(feedback_context: tuple) -> str:
    """Build a natural-language hint from past owner feedback for the prompt.

    Shared by the draft path (baked into the system instruction) and the critic
    path (so the reviewer also knows which phrasings the owner has rejected).
    """
    if not feedback_context:
        return ""
    approved = [f.response_text for f in feedback_context if f.rating == "up" and f.response_text]
    rejected = [f.response_text for f in feedback_context if f.rating == "down" and f.response_text]
    wishes = [f.feedback_text for f in feedback_context if f.feedback_text]
    parts: list[str] = []
    if approved:
        parts.append(
            "Примеры ответов, которые понравились владельцу — пиши в таком же духе:\n"
            + "\n".join(f"- {r[:200]}" for r in approved[:3])
        )
    if rejected:
        # 'down' now also includes the ORIGINAL AI text the owner rewrote before
        # sending (the edit-delta), so these are concrete anti-examples.
        parts.append(
            "Примеры ответов, которые не понравились владельцу (он их отклонил"
            " или переписал) — не повторяй такой стиль и формулировки:\n"
            + "\n".join(f"- {r[:200]}" for r in rejected[:3])
        )
    if wishes:
        parts.append("Пожелания владельца:\n" + "\n".join(f"- {w[:200]}" for w in wishes[:3]))
    return "\n\n".join(parts)
