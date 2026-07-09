"""Skills toolset for pydantic-ai agents (Г4).

Progressive disclosure: the writer's prompt lists each enabled skill's name and
description (level 1); ``load_skill`` fetches the full body on demand (level 2).
A skill can only be loaded when its name is in ``deps.enabled_skill_names`` — the
role's own enabled set — so the tool can never reach an unenabled or another
tenant's skill. Skill bodies are owner-authored / first-party content, so the
result is trusted (unlike MCP output); it is NOT wrapped as untrusted.
"""

from __future__ import annotations

import logging

from pydantic_ai import RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps

logger = logging.getLogger(__name__)

skill_toolset: FunctionToolset[AgentDeps] = FunctionToolset()

# Global first-party skills (tenant_id NULL) are visible to every tenant; a
# tenant's own skills match on tenant_id. Only enabled rows are loadable.
_LOAD_SKILL_SQL = (
    "SELECT id, body FROM skills "
    "WHERE (tenant_id IS NULL OR tenant_id = $1) AND name = $2 AND enabled LIMIT 1"
)

# Usage telemetry: one row per load, feeding the skill self-improvement signal
# (migration 124). rating is stamped later from the owner's draft feedback.
_RECORD_SKILL_USAGE_SQL = (
    "INSERT INTO skill_usage (tenant_id, skill_id, chat_id, source_message_id) "
    "VALUES ($1, $2, $3, $4)"
)


@skill_toolset.tool
async def load_skill(ctx: RunContext[AgentDeps], name: str) -> str:
    """Загрузи полный текст навыка по имени из списка навыков роли.

    Вызови ПЕРЕД тем как отвечать по теме навыка: в промпте перечислены доступные
    навыки (имя + описание), а тело с процедурой подгружается этим инструментом.
    Можно загрузить только навык из списка роли; для всего остального —
    «навык не найден» (это не повод выдумывать содержимое).
    """
    deps = ctx.deps
    lookup = name.strip()
    # Deny-by-default: never load a skill the role has not enabled.
    if deps.pool is None or lookup not in deps.enabled_skill_names:
        return "навык не найден"
    row = await deps.pool.fetchrow(_LOAD_SKILL_SQL, deps.tenant_id, lookup)
    if row is None:
        return "навык не найден"
    # Best-effort usage telemetry: a logging-write failure must never break the tool.
    try:
        await deps.pool.execute(
            _RECORD_SKILL_USAGE_SQL,
            deps.tenant_id, row["id"], deps.chat_id, deps.source_message_id or "",
        )
    except Exception:  # noqa: BLE001 - telemetry is non-critical
        logger.warning("skill usage record failed", extra={"skill": lookup})
    return f'<skill name="{lookup}">{row["body"]}</skill>'
