"""Export owner feedback into eval cases (the bot → evals feedback loop).

The bot collects owner verdicts on every draft (👍/👎/approve/reject/edit —
``draft_feedback`` table).  Rejected and edited drafts are the most valuable
quality signal we have: each becomes a regression case asserting the agent no
longer produces the phrasing the owner threw away.

Usage:
    DATABASE_URL=postgres://... .venv/bin/python evals/export_feedback.py
Writes ``evals/feedback_cases.json`` consumed by ``eval_drafts.py``.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

import asyncpg

OUTPUT = Path(__file__).parent / "feedback_cases.json"

# Negative verdicts whose drafts become anti-example regression cases.
_NEGATIVE_RATINGS = ("down", "reject", "edit", "regenerate")


async def export(database_url: str, limit: int = 200) -> int:
    conn = await asyncpg.connect(database_url)
    try:
        rows = await conn.fetch(
            """SELECT df.id, df.response_text AS rejected_reply,
                      COALESCE(d.source_message_text, '') AS source_text,
                      df.feedback_text
                 FROM draft_feedback df
                 LEFT JOIN drafts d ON d.draft_id = df.draft_id
                WHERE df.rating = ANY($1::text[])
                  AND df.response_text <> ''
                ORDER BY df.created_at DESC
                LIMIT $2""",
            list(_NEGATIVE_RATINGS),
            limit,
        )
    finally:
        await conn.close()
    cases = [
        {
            "id": str(r["id"]),
            "source_text": r["source_text"],
            "rejected_reply": r["rejected_reply"],
            "owner_comment": r["feedback_text"],
        }
        for r in rows
        if r["source_text"]
    ]
    OUTPUT.write_text(json.dumps(cases, ensure_ascii=False, indent=2))
    return len(cases)


def main() -> None:
    database_url = os.getenv("DATABASE_URL", "").strip()
    if not database_url:
        print("DATABASE_URL is required", file=sys.stderr)
        raise SystemExit(2)
    count = asyncio.run(export(database_url))
    print(f"exported {count} feedback cases -> {OUTPUT}")


if __name__ == "__main__":
    main()
