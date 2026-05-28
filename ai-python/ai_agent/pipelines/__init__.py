"""AI service pipelines.

Each module owns a single stage of the AI orchestration flow:
``draft``, ``extraction``, ``memory``, ``summarization``, ``embedding``,
``risk``, ``intent`` and ``decisions``.  The :class:`ai_agent.service.AIService`
holds the shared dependencies (router, classifiers, prompt builder) and
delegates per-endpoint logic to these modules.
"""
