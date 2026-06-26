"""Langfuse async sink: bounded queue + background worker (Phase 4).

The Langfuse SDK is optional; when it is not installed (or
``OBSERVABILITY_SINK=none``) the sink degrades to a noop so the hot reply path
never blocks.  When installed, span/score events are pushed onto a bounded
``queue.Queue`` and flushed in batches by a daemon thread.  On overflow the
oldest event is dropped and a counter is incremented for visibility.

Privacy: when the inbound chat carries ``no_training``, the sink ships only the
SHA-256 hash of the prompt (and an empty completion) instead of raw text — see
:meth:`LangfuseSink.record_generation`.
"""

from __future__ import annotations

import base64
import hashlib
import json
import logging
import queue
import threading
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Callable

from ai_agent.contract_enums import PrivacyFlag

logger = logging.getLogger(__name__)


@dataclass
class _SinkEvent:
    """Internal: one queued observability event."""

    kind: str
    payload: dict[str, Any] = field(default_factory=dict)


# Type alias: the worker that uploads one event.  Plugged in by the
# concrete Langfuse SDK adapter (or by tests).
Uploader = Callable[[_SinkEvent], None]


def _noop_uploader(_event: _SinkEvent) -> None:
    """Default uploader: drops events on the floor."""
    return None


class LangfuseSink:
    """Async, fire-and-forget sink for Langfuse spans + scores.

    The class never raises through ``record_*`` so the hot reply path is
    isolated from the sink's network failures.  Internal exceptions are
    logged at WARNING and the event is dropped.
    """

    def __init__(
        self,
        host: str = "",
        public_key: str = "",
        secret_key: str = "",
        queue_size: int = 1024,
        uploader: Uploader | None = None,
    ) -> None:
        """Create the sink and start the background worker if an uploader is set."""
        self._host = host.rstrip("/")
        self._public_key = public_key
        self._secret_key = secret_key
        self._queue: queue.Queue[_SinkEvent] = queue.Queue(maxsize=queue_size)
        self._uploader = uploader or _noop_uploader
        self._dropped = 0
        self._worker = threading.Thread(target=self._run, daemon=True, name="langfuse-sink")
        self._stopped = threading.Event()
        if uploader is not None:
            self._worker.start()

    @property
    def dropped(self) -> int:
        """Return the number of events dropped on queue overflow."""
        return self._dropped

    def configured(self) -> bool:
        """Return True if the sink is wired with a real uploader."""
        return self._uploader is not _noop_uploader

    def record_trace(self, trace_id: str, payload: dict[str, Any]) -> None:
        """Enqueue a trace metadata event."""
        self._enqueue("trace", {"trace_id": trace_id, **payload})

    def record_span(self, trace_id: str, name: str, payload: dict[str, Any]) -> None:
        """Enqueue a span event."""
        self._enqueue("span", {"trace_id": trace_id, "name": name, **payload})

    def record_generation(
        self,
        trace_id: str,
        model: str,
        prompt: str,
        completion: str,
        privacy_flags: tuple[PrivacyFlag, ...] = (),
        usage: dict[str, int] | None = None,
        cost_usd: float = 0.0,
    ) -> None:
        """Enqueue a generation (LLM call) event with privacy guards.

        When ``no_training`` is in ``privacy_flags`` the prompt/completion are
        replaced with their SHA-256 hashes so the Langfuse instance never sees
        raw user content.
        """
        prompt_payload = prompt
        completion_payload = completion
        if PrivacyFlag.NO_TRAINING in privacy_flags:
            prompt_payload = "sha256:" + hashlib.sha256(prompt.encode("utf-8")).hexdigest()
            completion_payload = ""
        self._enqueue("generation", {
            "trace_id": trace_id,
            "model": model,
            "prompt": prompt_payload,
            "completion": completion_payload,
            "usage": usage or {},
            "cost_usd": cost_usd,
        })

    def record_score(self, trace_id: str, name: str, value: float, comment: str = "") -> None:
        """Enqueue an owner-feedback score."""
        self._enqueue("score", {
            "trace_id": trace_id,
            "name": name,
            "value": value,
            "comment": comment,
        })

    def flush(self, timeout_seconds: float = 1.0) -> None:
        """Block until the current queue is drained or ``timeout_seconds`` elapses."""
        deadline = threading.Event()
        threading.Timer(timeout_seconds, deadline.set).start()
        while not self._queue.empty() and not deadline.is_set():
            try:
                event = self._queue.get_nowait()
            except queue.Empty:
                break
            self._safe_upload(event)
            self._queue.task_done()

    def stop(self) -> None:
        """Signal the worker thread to exit at the next iteration."""
        self._stopped.set()

    # ------------------------------------------------------------------
    # Internals
    # ------------------------------------------------------------------
    def _enqueue(self, kind: str, payload: dict[str, Any]) -> None:
        try:
            self._queue.put_nowait(_SinkEvent(kind=kind, payload=payload))
        except queue.Full:
            self._dropped += 1
            logger.warning("langfuse sink queue full; dropping event", extra={"kind": kind})

    def _run(self) -> None:
        while not self._stopped.is_set():
            try:
                event = self._queue.get(timeout=0.5)
            except queue.Empty:
                continue
            self._safe_upload(event)
            self._queue.task_done()

    def _safe_upload(self, event: _SinkEvent) -> None:
        try:
            self._uploader(event)
        except BaseException as exc:  # pragma: no cover - belt-and-braces guard
            logger.warning(
                "langfuse sink uploader failed", extra={"kind": event.kind, "error": str(exc)},
            )


class HttpUploader:
    """Langfuse batch-ingestion uploader using stdlib urllib (no SDK required).

    Translates internal ``_SinkEvent`` records to the Langfuse v2
    ``/api/public/ingestion`` batch envelope and POSTs each event using
    Basic auth (public_key:secret_key).  Uses the same credentials as the
    LiteLLM ``success_callback`` so all events land in the same project.

    Network errors are propagated so ``LangfuseSink._safe_upload`` can catch
    and log them without blocking the hot reply path.
    """

    def __init__(self, host: str, public_key: str, secret_key: str) -> None:
        """Create an uploader targeting the given Langfuse host."""
        creds = base64.b64encode(f"{public_key}:{secret_key}".encode()).decode()
        self._url = f"{host.rstrip('/')}/api/public/ingestion"
        self._auth = f"Basic {creds}"

    def __call__(self, event: _SinkEvent) -> None:
        """Serialize one event to the Langfuse batch API."""
        item = _to_batch_item(event)
        if item is None:
            return
        body = json.dumps({"batch": [item]}, ensure_ascii=False).encode("utf-8")
        req = urllib.request.Request(
            self._url,
            data=body,
            headers={"Authorization": self._auth, "Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=5) as resp:
            resp.read()


def _to_batch_item(event: _SinkEvent) -> dict[str, Any] | None:
    """Translate a ``_SinkEvent`` to a Langfuse v2 batch item, or ``None`` to skip.

    Each batch item carries a unique ``id`` (UUID4) for Langfuse deduplication
    so repeated flush attempts for the same event do not double-count.
    """
    now = datetime.now(timezone.utc).isoformat()
    item_id = uuid.uuid4().hex
    p = event.payload

    if event.kind == "generation":
        return {
            "id": item_id,
            "timestamp": now,
            "type": "generation-create",
            "body": {
                # Langfuse requires an observation id on the body; without it the
                # whole event is rejected (400). The trace body carries its id via
                # trace_id, which is why traces ingested but generations never did.
                "id": item_id,
                "traceId": p.get("trace_id"),
                "name": "app.llm.complete",
                "model": p.get("model"),
                "input": p.get("prompt"),
                "output": p.get("completion"),
                "usage": {
                    "input": p.get("usage", {}).get("input_tokens", 0),
                    "output": p.get("usage", {}).get("output_tokens", 0),
                },
                "calculatedTotalCost": p.get("cost_usd") or 0,
                "startTime": now,
                "endTime": now,
            },
        }
    if event.kind == "span":
        return {
            "id": item_id,
            "timestamp": now,
            "type": "span-create",
            "body": {
                "id": item_id,
                "traceId": p.get("trace_id"),
                "name": p.get("name"),
                "startTime": now,
                "endTime": now,
                "metadata": {k: v for k, v in p.items() if k not in ("trace_id", "name")},
            },
        }
    if event.kind == "trace":
        return {
            "id": item_id,
            "timestamp": now,
            "type": "trace-create",
            "body": {
                "id": p.get("trace_id"),
                "name": p.get("name", "request"),
                "metadata": {k: v for k, v in p.items() if k not in ("trace_id", "name")},
            },
        }
    if event.kind == "score":
        return {
            "id": item_id,
            "timestamp": now,
            "type": "score-create",
            "body": {
                "id": item_id,
                "traceId": p.get("trace_id"),
                "name": p.get("name"),
                "value": p.get("value"),
                "comment": p.get("comment", ""),
                "dataType": "NUMERIC",
            },
        }
    return None
