"""Per-purpose divergence comparison between legacy + new gateway outputs."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

from ai_agent.contract_enums import LLMPurpose


Classification = Literal["match", "close", "divergent"]


@dataclass(frozen=True)
class ShadowDiff:
    """Outcome of one legacy-vs-new comparison."""

    purpose: LLMPurpose
    score: float
    classification: Classification
    details: dict


def _token_set(text: str) -> set[str]:
    return {t for t in text.lower().split() if t}


def _jaccard(a: str, b: str) -> float:
    sa, sb = _token_set(a), _token_set(b)
    if not sa and not sb:
        return 1.0
    if not sa or not sb:
        return 0.0
    inter = len(sa & sb)
    union = len(sa | sb)
    return inter / union if union else 1.0


def _edit_ratio(a: str, b: str) -> float:
    """Cheap O(n*m) Levenshtein-based similarity ratio in [0, 1]."""
    if a == b:
        return 1.0
    if not a or not b:
        return 0.0
    # Compute Levenshtein distance with the two-row DP.
    prev = list(range(len(b) + 1))
    for i, ca in enumerate(a, start=1):
        curr = [i] + [0] * len(b)
        for j, cb in enumerate(b, start=1):
            cost = 0 if ca == cb else 1
            curr[j] = min(curr[j - 1] + 1, prev[j] + 1, prev[j - 1] + cost)
        prev = curr
    distance = prev[-1]
    longest = max(len(a), len(b))
    return 1.0 - (distance / longest)


def gateway_diff(old: str, new: str, purpose: LLMPurpose) -> ShadowDiff:
    """Compare two gateway outputs and classify divergence per purpose.

    Classification thresholds match those documented in the backlog:

    * ``draft``:  edit-similarity ≥ 0.85 → match, ≥ 0.70 → close, else divergent.
    * ``extract``: Jaccard on text tokens; ≥ 0.85 / ≥ 0.7 thresholds.
    * ``summarize``: edit-similarity proxy for ROUGE-L; thresholds 0.85 / 0.7.
    * ``embed``: identity check on text (embedding cosine is owned by the
      embedding pipeline, not the chat router).
    """
    if purpose == LLMPurpose.EMBED:
        score = 1.0 if old == new else 0.0
        classification: Classification = "match" if score == 1.0 else "divergent"
        return ShadowDiff(purpose=purpose, score=score, classification=classification, details={})

    if purpose == LLMPurpose.EXTRACT:
        score = _jaccard(old, new)
    else:
        # draft / summarize / classify / moderate
        score = _edit_ratio(old, new)

    if score >= 0.85:
        classification = "match"
    elif score >= 0.70:
        classification = "close"
    else:
        classification = "divergent"
    return ShadowDiff(
        purpose=purpose,
        score=score,
        classification=classification,
        details={"jaccard" if purpose == LLMPurpose.EXTRACT else "edit_ratio": score},
    )
