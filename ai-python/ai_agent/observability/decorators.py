"""``@observe_pipeline`` wraps one pipeline call in a Langfuse span.

The decorator reads the current :class:`LLMCallContext` so it never needs to
plumb arguments through the pipeline modules.  When no sink is configured the
decorator is a no-op pass-through.
"""

from __future__ import annotations

import functools
import time
from typing import Any, Callable, TypeVar

from .context import current_context

F = TypeVar("F", bound=Callable[..., Any])

# Module-level holder for the optional global Langfuse sink.  ``set_sink``
# binds the sink at startup; the decorator looks it up at call time so a sink
# created after import time is still picked up.
_sink_holder: dict[str, Any] = {"sink": None}


def set_sink(sink: Any) -> None:
    """Bind a global LangfuseSink (or any object exposing ``record_span``)."""
    _sink_holder["sink"] = sink


def get_sink() -> Any:
    """Return the currently bound sink, or ``None`` if not configured."""
    return _sink_holder.get("sink")


def observe_pipeline(name: str) -> Callable[[F], F]:
    """Decorate a pipeline function so each call emits a Langfuse span."""

    def _decorator(func: F) -> F:
        @functools.wraps(func)
        def _wrapper(*args: Any, **kwargs: Any) -> Any:
            sink = get_sink()
            if sink is None:
                return func(*args, **kwargs)
            ctx = current_context()
            start = time.perf_counter()
            error = ""
            try:
                return func(*args, **kwargs)
            except BaseException as exc:
                error = str(exc)
                raise
            finally:
                sink.record_span(
                    trace_id=ctx.trace_id,
                    name=name,
                    payload={
                        "tenant_id": ctx.tenant_id,
                        "chat_id": ctx.chat_id,
                        "purpose": ctx.purpose.value if ctx.purpose else "",
                        "latency_ms": (time.perf_counter() - start) * 1000,
                        "error": error,
                    },
                )

        return _wrapper  # type: ignore[return-value]

    return _decorator
