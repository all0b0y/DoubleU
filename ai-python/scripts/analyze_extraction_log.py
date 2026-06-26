"""Analyse the ``extraction_log`` to ground fact-extraction tuning in data.

The extraction pre-filter threshold (``confidence >= 0.8``) and the transience
filter are currently set blind — nobody knows whether we over-filter (too few
facts) or under-filter (noise). The Go backend already records one row per
extraction attempt (``store.LogExtraction``): facts created / skipped / merged,
duration, model. This script aggregates those rows by ISO week so the threshold
can be retuned against real yield/skip/merge curves (backlog: MEM-13).

Read-only. Usage:
    DATABASE_URL=postgres://... .venv/bin/python scripts/analyze_extraction_log.py
    DATABASE_URL=... .venv/bin/python scripts/analyze_extraction_log.py --weeks 8 --tenant tenant:user:77
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys

import asyncpg

# Weekly rollup. Rates are derived in Python (below) to keep the SQL readable;
# percentiles stay in SQL because they need the full per-row distribution.
_WEEKLY_QUERY = """
    SELECT
        date_trunc('week', created_at)::date            AS week,
        count(*)                                         AS attempts,
        sum(coalesce(cardinality(input_message_ids), 0)) AS messages,
        sum(coalesce(cardinality(facts_created), 0))     AS facts_created,
        sum(facts_skipped)                               AS facts_skipped,
        sum(facts_merged)                                AS facts_merged,
        count(*) FILTER (WHERE error IS NOT NULL AND error <> '') AS errors,
        percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)  AS p50_ms,
        percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95_ms
      FROM extraction_log
     WHERE created_at >= now() - ($1::int * interval '1 week')
       AND ($2 = '' OR tenant_id = $2)
     GROUP BY 1
     ORDER BY 1
"""


def _safe_ratio(numerator: float, denominator: float) -> float:
    """Ratio that returns 0.0 instead of raising when the denominator is zero."""
    return numerator / denominator if denominator else 0.0


async def analyze(database_url: str, weeks: int, tenant: str) -> list[dict]:
    """Return one aggregated metrics dict per ISO week, newest last."""
    conn = await asyncpg.connect(database_url)
    try:
        rows = await conn.fetch(_WEEKLY_QUERY, weeks, tenant)
    finally:
        await conn.close()

    report: list[dict] = []
    for r in rows:
        created = r["facts_created"] or 0
        skipped = r["facts_skipped"] or 0
        merged = r["facts_merged"] or 0
        candidates = created + skipped + merged
        report.append(
            {
                "week": str(r["week"]),
                "attempts": r["attempts"],
                "messages": r["messages"] or 0,
                "facts_created": created,
                # facts per processed message — the headline yield signal.
                "yield": round(_safe_ratio(created, r["messages"] or 0), 3),
                # share of candidate facts dropped by the pre-filter / transience filter.
                "skip_rate": round(_safe_ratio(skipped, candidates), 3),
                # share merged into an existing fact (dedup pressure, not new knowledge).
                "merge_rate": round(_safe_ratio(merged, created + merged), 3),
                "errors": r["errors"],
                "p50_ms": int(r["p50_ms"] or 0),
                "p95_ms": int(r["p95_ms"] or 0),
            }
        )
    return report


def _print_table(report: list[dict]) -> None:
    header = ("week", "attempts", "messages", "facts", "yield", "skip_rate", "merge_rate", "errors", "p50_ms", "p95_ms")
    widths = (12, 9, 9, 7, 7, 10, 11, 7, 8, 8)
    line = "  ".join(h.ljust(w) for h, w in zip(header, widths))
    print(line)
    print("-" * len(line))
    for row in report:
        cells = (
            row["week"], row["attempts"], row["messages"], row["facts_created"],
            row["yield"], row["skip_rate"], row["merge_rate"], row["errors"],
            row["p50_ms"], row["p95_ms"],
        )
        print("  ".join(str(c).ljust(w) for c, w in zip(cells, widths)))


def main() -> None:
    parser = argparse.ArgumentParser(description="Weekly extraction_log metrics.")
    parser.add_argument("--weeks", type=int, default=12, help="lookback window in weeks (default 12)")
    parser.add_argument("--tenant", default="", help="restrict to one tenant_id (default: all)")
    args = parser.parse_args()

    database_url = os.getenv("DATABASE_URL", "").strip()
    if not database_url:
        print("DATABASE_URL is required", file=sys.stderr)
        raise SystemExit(2)

    report = asyncio.run(analyze(database_url, args.weeks, args.tenant))
    if not report:
        print("no extraction_log rows in the selected window")
        return
    _print_table(report)


if __name__ == "__main__":
    main()
