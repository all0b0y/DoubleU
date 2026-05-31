"""In-process divergence counters exposed via the AI service ``/metrics``."""

from __future__ import annotations

import threading
from collections import defaultdict
from typing import Mapping

from ai_agent.contract_enums import LLMPurpose


class DivergenceCounter:
    """Thread-safe counter: per purpose, count of (match|close|divergent)."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._counts: dict[LLMPurpose, dict[str, int]] = defaultdict(lambda: defaultdict(int))

    def record(self, purpose: LLMPurpose, classification: str) -> None:
        """Increment the counter bucket for one (purpose, classification) pair."""
        with self._lock:
            self._counts[purpose][classification] += 1

    def snapshot(self) -> Mapping[str, Mapping[str, int]]:
        """Return a frozen view of every counter for ``/metrics`` rendering."""
        with self._lock:
            return {
                purpose.value: dict(buckets) for purpose, buckets in self._counts.items()
            }


_global = DivergenceCounter()


def record(purpose: LLMPurpose, classification: str) -> None:
    """Record one divergence outcome against the process-wide counter."""
    _global.record(purpose, classification)


def snapshot() -> Mapping[str, Mapping[str, int]]:
    """Return a snapshot of every divergence counter."""
    return _global.snapshot()


def reset() -> None:
    """Reset every divergence counter — intended for tests only."""
    global _global
    _global = DivergenceCounter()
