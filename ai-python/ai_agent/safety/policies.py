"""Single source of truth for per-purpose scanner sets.

Input vs output scanner sets are kept separate so the safety scanners only run
where the contract requires them.  ``embed`` deliberately skips
``PromptInjection`` because embed texts are facts, not instructions, and a
positive injection match would corrupt RAG retrieval.
"""

from __future__ import annotations

from ai_agent.contract_enums import LLMPurpose
from ai_agent.schemas import ModelTask


INPUT_SCANNERS_BY_PURPOSE: dict[LLMPurpose, tuple[str, ...]] = {
    LLMPurpose.DRAFT:     ("Anonymize", "Secrets", "PromptInjection", "Toxicity", "BanTopics"),
    LLMPurpose.SUMMARIZE: ("Anonymize", "Secrets", "PromptInjection", "Toxicity", "BanTopics"),
    LLMPurpose.EXTRACT:   ("Anonymize", "Secrets", "PromptInjection"),
    LLMPurpose.EMBED:     ("Anonymize", "Secrets"),
    LLMPurpose.MODERATE:  ("Anonymize", "Secrets", "PromptInjection"),
    LLMPurpose.CLASSIFY:  ("Anonymize", "Secrets", "PromptInjection"),
}


OUTPUT_SCANNERS_BY_PURPOSE: dict[LLMPurpose, tuple[str, ...]] = {
    LLMPurpose.DRAFT:     ("Toxicity", "Sensitive", "Deanonymize"),
    LLMPurpose.SUMMARIZE: ("Sensitive", "Deanonymize"),
    LLMPurpose.EXTRACT:   ("JSON",),
    LLMPurpose.EMBED:     (),
    LLMPurpose.MODERATE:  (),
    LLMPurpose.CLASSIFY:  ("JSON",),
}


def purpose_for_task(task: ModelTask) -> LLMPurpose:
    """Map a legacy ModelTask onto the purpose tag the safety layers use."""
    if task == ModelTask.SUMMARIZE_CHAT:
        return LLMPurpose.SUMMARIZE
    if task == ModelTask.EXTRACT_FACTS:
        return LLMPurpose.EXTRACT
    return LLMPurpose.DRAFT
