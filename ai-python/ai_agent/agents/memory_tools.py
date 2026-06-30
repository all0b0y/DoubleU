"""PostgreSQL memory toolset for pydantic-ai agents.

Typed tools over the per-chat memory schema (MEMORY_ARCHITECTURE.md):
semantic facts, episode summaries, contact profile and recent messages.
Every tool is scoped to ``deps.tenant_id``/``deps.chat_id`` — an agent can
never be prompted into another chat's memory — and privacy flags are enforced
inside each tool (``no_memory`` blocks both reads and writes).

Writes go EXCLUSIVELY through the ``upsert_semantic_fact`` v3 procedure
(dedup by hash + cosine, advisory locks — MEM-11); profile changes are staged
as pending suggestions for owner review, never applied directly (MEM-4).
"""

from __future__ import annotations

import logging
from typing import Literal, Sequence

from pydantic_ai import RunContext
from pydantic_ai.toolsets import FunctionToolset

from .deps import AgentDeps

logger = logging.getLogger(__name__)

# Active (non-deleted, non-superseded) facts of the current chat.
_FACT_FILTER = "tenant_id = $1 AND chat_id = $2 AND deleted_at IS NULL AND superseded_by_id IS NULL"

# How many supersession/update hops back from a current fact we surface as
# history. 2 is enough to show "current ← previous ← the one before" without the
# walk fanning out across long-collapsed chains.
_GRAPH_MAX_DEPTH = 2

# Graph-aware semantic search (temporal knowledge graph read-path).
#
# Seeds with the top-K *current* facts ranked by embedding distance, then walks
# fact_relations backward (fact_id_to → fact_id_from) along supersedes/updates
# edges to recover what each current fact replaced. Older facts are soft-deleted
# by consolidate_facts, so the recursive arm deliberately omits the deleted_at
# filter — the edge itself (tenant-scoped) bounds what it reaches. Rows come back
# ordered (similarity, root, depth) so each current fact is followed by its
# progressively older versions.
_GRAPH_SEARCH_SQL = """
WITH RECURSIVE seed AS (
    SELECT sf.id, sf.fact_text, sf.fact_type, sf.confidence,
           fe.embedding <=> $3::vector AS dist
      FROM semantic_facts sf
      JOIN fact_embeddings fe ON fe.fact_id = sf.id
     WHERE sf.tenant_id = $1 AND sf.chat_id = $2
       AND sf.deleted_at IS NULL AND sf.superseded_by_id IS NULL
     ORDER BY fe.embedding <=> $3::vector
     LIMIT $4
),
chain AS (
    SELECT s.id, s.fact_text, s.fact_type, s.id AS root_id, s.dist, 0 AS depth
      FROM seed s
    UNION ALL
    SELECT old.id, old.fact_text, old.fact_type, c.root_id, c.dist, c.depth + 1
      FROM chain c
      JOIN fact_relations fr
        ON fr.fact_id_to = c.id
       AND fr.tenant_id = $1
       AND fr.relation_type IN ('supersedes', 'updates')
      JOIN semantic_facts old
        ON old.id = fr.fact_id_from
       AND old.tenant_id = $1 AND old.chat_id = $2
     WHERE c.depth < {max_depth}
)
SELECT id, fact_text, fact_type, root_id, depth
  FROM chain
 ORDER BY dist, root_id, depth
""".format(max_depth=_GRAPH_MAX_DEPTH)

memory_toolset: FunctionToolset[AgentDeps] = FunctionToolset()


def _vector_literal(embedding: Sequence[float]) -> str:
    """Render an embedding as a pgvector literal so no binary codec is needed."""
    return "[" + ",".join(f"{v:.8f}" for v in embedding) + "]"


def _format_fact_chains(rows: Sequence) -> list[str]:
    """Render graph rows as one line per current fact, with its prior versions.

    Rows are ordered (similarity, root, depth): a current fact (depth 0) is
    followed by the facts it superseded. History is shown inline ("← ранее: …")
    so the agent sees how a fact evolved rather than only its latest value. A
    fact with no history renders exactly as the flat path did, keeping the
    common case unchanged.
    """
    groups: dict[int, list] = {}
    order: list[int] = []
    for row in rows:
        root = row["root_id"]
        if root not in groups:
            groups[root] = []
            order.append(root)
        groups[root].append(row)

    lines: list[str] = []
    seen: set[int] = set()
    for root in order:
        members = sorted(groups[root], key=lambda r: r["depth"])
        current = members[0]
        if current["id"] in seen:
            continue
        seen.add(current["id"])
        history = [r["fact_text"] for r in members[1:] if r["id"] not in seen]
        for r in members[1:]:
            seen.add(r["id"])
        line = f"[{current['fact_type']}] {current['fact_text']}"
        if history:
            line += " ← ранее: " + "; ".join(history)
        lines.append(line)
    return lines


@memory_toolset.tool
async def search_facts(ctx: RunContext[AgentDeps], query: str, limit: int = 5) -> list[str]:
    """Найти в памяти ЭТОГО чата факты, релевантные запросу.

    Используй, когда для ответа нужны детали о собеседнике или договорённостях,
    которых нет в видимом контексте (предпочтения, биография, обязательства).
    НЕ используй для вопросов про общие знания, не связанные с этим собеседником
    — это память конкретного чата, а не база общих фактов. Пустой список значит
    «фактов нет или память недоступна», а не повод придумать факт.
    """
    deps = ctx.deps
    if deps.pool is None or deps.no_memory or not query.strip():
        return []
    limit = max(1, min(limit, 10))
    embedding = None
    if deps.embedder is not None:
        embedding = await deps.embedder(query)
    if embedding:
        rows = await deps.pool.fetch(
            _GRAPH_SEARCH_SQL,
            deps.tenant_id,
            deps.chat_id,
            _vector_literal(embedding),
            limit,
        )
        return _format_fact_chains(rows)
    rows = await deps.pool.fetch(
        f"""SELECT fact_text, fact_type, confidence
              FROM semantic_facts
             WHERE {_FACT_FILTER} AND fact_text ILIKE '%' || $3 || '%'
             ORDER BY confidence DESC, created_at DESC
             LIMIT $4""",
        deps.tenant_id,
        deps.chat_id,
        query.strip(),
        limit,
    )
    return [f"[{r['fact_type']}] {r['fact_text']}" for r in rows]


@memory_toolset.tool
async def get_episode_summaries(ctx: RunContext[AgentDeps], limit: int = 3) -> list[str]:
    """Получить сводки прошлых эпизодов общения в этом чате (свежие первыми).

    Используй, когда нужен контекст более ранних разговоров, которых нет в
    видимой переписке. НЕ используй, если видимого контекста уже достаточно.
    """
    deps = ctx.deps
    if deps.pool is None or deps.no_memory:
        return []
    rows = await deps.pool.fetch(
        """SELECT content, period_start::date AS day
             FROM episode_summaries
            WHERE tenant_id = $1 AND chat_id = $2 AND deleted_at IS NULL
            ORDER BY period_end DESC
            LIMIT $3""",
        deps.tenant_id,
        deps.chat_id,
        max(1, min(limit, 5)),
    )
    return [f"[{r['day']}] {r['content']}" for r in rows]


@memory_toolset.tool
async def get_recent_messages(ctx: RunContext[AgentDeps], limit: int = 10) -> list[str]:
    """Получить последние сообщения этого чата (старые первыми).

    Используй, если видимого контекста не хватает, чтобы понять ход разговора.
    НЕ используй для первого сообщения от собеседника, когда видимого контекста
    уже достаточно — истории ещё нет, вызов ничего не добавит.
    """
    deps = ctx.deps
    if deps.pool is None or deps.no_memory:
        return []
    rows = await deps.pool.fetch(
        """SELECT sender_id, message_text, created_at
             FROM messages
            WHERE tenant_id = $1 AND chat_id = $2 AND message_text <> ''
            ORDER BY created_at DESC
            LIMIT $3""",
        deps.tenant_id,
        deps.chat_id,
        max(1, min(limit, 30)),
    )
    return [f"{r['sender_id']}: {r['message_text']}" for r in reversed(rows)]


@memory_toolset.tool
async def get_contact_profile(ctx: RunContext[AgentDeps]) -> dict[str, str]:
    """Получить профиль собеседника, заполненный владельцем (имя, тон, отношения).

    Используй, если в инструкции нет данных о собеседнике, а они нужны для тона.
    НЕ вызывай, если профиль собеседника уже явно описан в переданном контексте —
    повторный вызов в одном ходе вернёт то же самое и не добавит новых данных.
    """
    deps = ctx.deps
    if deps.pool is None or deps.no_memory:
        return {}
    row = await deps.pool.fetchrow(
        """SELECT name, gender, role, relationship_type, tone, preferred_address,
                  language, chat_context
             FROM contact_profiles
            WHERE tenant_id = $1 AND chat_id = $2""",
        deps.tenant_id,
        deps.chat_id,
    )
    if row is None:
        return {}
    return {key: str(value) for key, value in dict(row).items() if value}


@memory_toolset.tool
async def save_fact(
    ctx: RunContext[AgentDeps],
    fact_text: str,
    fact_type: Literal[
        "preference", "biographical", "commitment", "event", "relationship", "health", "taboo"
    ],
    subject: Literal["contact", "owner", "relationship", "third_party"],
    confidence: float,
) -> str:
    """Сохранить долговременный факт в память этого чата.

    Сохраняй только устойчивые факты (профессия, место учёбы, семья, стойкие
    предпочтения, обязательства с дедлайном) — не сиюминутные состояния. НЕ
    сохраняй факт, который уже присутствует в переданном контексте памяти.
    """
    deps = ctx.deps
    if deps.no_memory:
        return "memory disabled for this chat (no_memory)"
    if deps.pool is None:
        return "memory storage unavailable"
    fact_text = fact_text.strip()
    if not 5 <= len(fact_text) <= 120:
        return "fact_text must be 5..120 characters"
    confidence = min(max(confidence, 0.0), 1.0)
    row = await deps.pool.fetchrow(
        "SELECT * FROM upsert_semantic_fact($1, 'chat', $2, $3, $4, $5, $6, 'llm_extracted', $7)",
        deps.tenant_id,
        deps.chat_id,
        fact_text,
        fact_type,
        subject,
        confidence,
        deps.source_message_id,
    )
    action = row["action"] if row is not None else "unknown"
    logger.info(
        "agent fact write",
        extra={"tenant_id": deps.tenant_id, "chat_id": deps.chat_id, "action": action},
    )
    return f"saved ({action})"


@memory_toolset.tool
async def suggest_profile_update(
    ctx: RunContext[AgentDeps],
    field_name: Literal["relationship_type", "preferred_address", "language", "tone", "gender"],
    suggested_value: str,
    rationale: str,
) -> str:
    """Предложить владельцу обновление профиля собеседника (MEM-4).

    Используй только когда сообщение явно раскрывает атрибут, отличающийся от
    текущего профиля. НЕ предлагай значение, совпадающее с уже сохранённым, и
    не предлагай по неявным/предположительным признакам. Предложение попадает
    в очередь на подтверждение — профиль никогда не меняется без решения
    владельца.
    """
    deps = ctx.deps
    if deps.no_memory:
        return "memory disabled for this chat (no_memory)"
    if deps.pool is None:
        return "memory storage unavailable"
    suggested_value = suggested_value.strip()[:64]
    if not suggested_value:
        return "suggested_value is empty"
    row = await deps.pool.fetchrow(
        """INSERT INTO contact_profile_suggestions
               (tenant_id, chat_id, field_name, suggested_value, rationale, status)
           VALUES ($1, $2, $3, to_jsonb($4::text), $5, 'pending')
           ON CONFLICT DO NOTHING
           RETURNING id""",
        deps.tenant_id,
        deps.chat_id,
        field_name,
        suggested_value,
        rationale.strip()[:120],
    )
    if row is None:
        return "an identical suggestion is already pending"
    return f"staged for owner review (id={row['id']})"
