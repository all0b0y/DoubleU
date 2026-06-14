"""AI service pipelines.

The remaining modules own the non-agent stages of the flow: ``draft`` (prompt
assembly + policy preflight reused by the agent runtime), the deterministic
classifiers behind ``risk``/``intent``/``memory``, ``decisions`` (policy
thresholds) and ``embedding`` (direct provider transport).  All LLM generation
lives in :mod:`ai_agent.agents.runtime`.
"""
