"""Pydantic AI agent layer.

Typed agents (draft / critic / extraction / summarizer / topic / rerank) with
native structured output are the single LLM execution path for the service:
they replace the hand-rolled JSON prompting, the ModelRouter and the provider
gateways.  The PostgreSQL memory toolset gives agents direct, privacy-gated
access to the per-chat memory (semantic facts, episode summaries, contact
profile).  Phase 1/3 safety and the Langfuse sink are injected into the runtime
by :func:`ai_agent.agents.runtime.build_runtime`.
"""

from .runtime import AgentRuntime, build_runtime

__all__ = ["AgentRuntime", "build_runtime"]
