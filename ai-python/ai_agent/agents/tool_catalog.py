"""Canonical catalog for native tools exposed by the AI service."""

from __future__ import annotations

from dataclasses import dataclass
from types import MappingProxyType
from typing import Mapping

from ai_agent.contract_enums import ToolCategory


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
        key="interaction.present_buttons",
        label="Кнопки для собеседника",
        description="Задаёт собеседнику вопрос с вариантами ответа или подтверждением.",
        category=ToolCategory.INTERACTION,
        default_enabled=False,
    ),
    ToolCatalogEntry(
        key="scheduler.create_reminder",
        label="Напоминания",
        description="Создаёт видимые владельцу напоминания для него или собеседника.",
        category=ToolCategory.SCHEDULE,
        default_enabled=False,
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
        "present_buttons": "interaction.present_buttons",
        "request_confirmation": "interaction.present_buttons",
        "create_reminder": "scheduler.create_reminder",
    }
)

TOOL_CATALOG_BY_KEY: Mapping[str, ToolCatalogEntry] = MappingProxyType(
    {entry.key: entry for entry in TOOL_CATALOG}
)
