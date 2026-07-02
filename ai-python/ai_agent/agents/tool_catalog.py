"""Canonical catalog for native tools exposed by the AI service."""

from __future__ import annotations

from dataclasses import dataclass
from types import MappingProxyType
from typing import Mapping

from ai_agent.contract_enums import PrivacyFlag, ToolCategory
from ai_agent.schemas import AIRoleData


@dataclass(frozen=True)
class ToolCatalogEntry:
    """Owner-visible metadata and default policy for one native tool."""

    key: str
    label: str
    description: str
    category: ToolCategory
    default_enabled: bool


TOOL_CATALOG: tuple[ToolCatalogEntry, ...] = (
    ToolCatalogEntry(
        key="memory.search_facts",
        label="Поиск фактов",
        description="Ищет сохранённые факты о собеседнике и договорённостях.",
        category=ToolCategory.READ,
        default_enabled=True,
    ),
    ToolCatalogEntry(
        key="memory.get_episode_summaries",
        label="Сводки прошлых разговоров",
        description="Читает краткие сводки предыдущих эпизодов общения.",
        category=ToolCategory.READ,
        default_enabled=True,
    ),
    ToolCatalogEntry(
        key="memory.get_recent_messages",
        label="Недавние сообщения",
        description="Получает последние сообщения чата, когда видимого контекста мало.",
        category=ToolCategory.READ,
        default_enabled=True,
    ),
    ToolCatalogEntry(
        key="memory.get_contact_profile",
        label="Профиль собеседника",
        description="Читает сохранённые имя, отношения, тон и обращение к собеседнику.",
        category=ToolCategory.READ,
        default_enabled=True,
    ),
    ToolCatalogEntry(
        key="memory.save_fact",
        label="Сохранение фактов",
        description="Сохраняет устойчивые факты и договорённости в память чата.",
        category=ToolCategory.READ,
        default_enabled=True,
    ),
    ToolCatalogEntry(
        key="memory.suggest_profile_update",
        label="Предложения к профилю",
        description="Предлагает владельцу обновить профиль собеседника.",
        category=ToolCategory.READ,
        default_enabled=True,
    ),
    ToolCatalogEntry(
        key="interaction.ask_questions",
        label="Вопросы собеседнику",
        description="Задаёт собеседнику пул вопросов по одному с кнопками-подсказками и ответом своими словами.",
        category=ToolCategory.INTERACTION,
        default_enabled=False,
    ),
    ToolCatalogEntry(
        key="scheduler.create_reminder",
        label="Создание напоминаний",
        description="Создаёт видимые владельцу напоминания для него или собеседника.",
        category=ToolCategory.SCHEDULE,
        default_enabled=False,
    ),
    ToolCatalogEntry(
        key="scheduler.list_reminders",
        label="Список напоминаний",
        description="Показывает активные напоминания этого чата, чтобы на них ссылаться.",
        category=ToolCategory.SCHEDULE,
        default_enabled=False,
    ),
    ToolCatalogEntry(
        key="scheduler.cancel_reminder",
        label="Отмена напоминаний",
        description="Отменяет ранее поставленное напоминание этого чата по его номеру.",
        category=ToolCategory.SCHEDULE,
        default_enabled=False,
    ),
    ToolCatalogEntry(
        key="skills.load_skill",
        label="Навыки роли",
        description="Загружает полный текст включённого навыка роли по имени.",
        category=ToolCategory.SKILLS,
        default_enabled=True,
    ),
)


TOOL_NAME_TO_KEY: Mapping[str, str] = MappingProxyType(
    {
        "search_facts": "memory.search_facts",
        "get_episode_summaries": "memory.get_episode_summaries",
        "get_recent_messages": "memory.get_recent_messages",
        "get_contact_profile": "memory.get_contact_profile",
        "save_fact": "memory.save_fact",
        "suggest_profile_update": "memory.suggest_profile_update",
        "ask_questions": "interaction.ask_questions",
        "request_confirmation": "interaction.ask_questions",
        "create_reminder": "scheduler.create_reminder",
        "list_reminders": "scheduler.list_reminders",
        "cancel_reminder": "scheduler.cancel_reminder",
        "load_skill": "skills.load_skill",
    }
)

TOOL_CATALOG_BY_KEY: Mapping[str, ToolCatalogEntry] = MappingProxyType(
    {entry.key: entry for entry in TOOL_CATALOG}
)


def enabled_tool_names(
    ai_role: AIRoleData | None, privacy_flags: tuple[PrivacyFlag, ...] = ()
) -> frozenset[str]:
    """Native tool function names enabled for this role/privacy combination.

    Mirrors the exact filter ``agents.runtime.resolve_builtin_toolset`` applies
    to the real ``FilteredToolset``, so prompt-facing tool policy
    (``agents.tool_policy``) never describes a tool that isn't actually wired
    in for this turn. A missing ``tool_settings`` entry inherits
    ``default_enabled``; ``schedule``-category tools are additionally blocked
    under ``no_memory``/``private``.
    """
    settings = ai_role.tool_settings if ai_role is not None else {}
    scheduler_blocked = any(
        flag in (PrivacyFlag.NO_MEMORY, PrivacyFlag.PRIVATE) for flag in privacy_flags
    )
    enabled_keys = {
        entry.key
        for entry in TOOL_CATALOG
        if settings.get(entry.key, entry.default_enabled)
        and not (scheduler_blocked and entry.category.value == "schedule")
    }
    return frozenset(name for name, key in TOOL_NAME_TO_KEY.items() if key in enabled_keys)
