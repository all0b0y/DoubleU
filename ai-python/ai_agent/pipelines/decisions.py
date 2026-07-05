"""Pure policy decision helpers used by the orchestration service."""

from __future__ import annotations

from ..schemas import ChatMode, Decision, PrivacyFlag, RiskLevel


# Deterministic ceilings on confidence per risk level.  The LLM never raises
# them: a self-assessed confidence is always capped by the risk classifier so
# the policy in :func:`decide` cannot auto-reply on risky messages.
_RISK_CAPS = {
    RiskLevel.CRITICAL: 0.35,
    RiskLevel.HIGH: 0.55,
    RiskLevel.MEDIUM: 0.84,
    RiskLevel.LOW: 0.96,
}

def decide(
    mode: ChatMode,
    privacy_flags: tuple[PrivacyFlag, ...],
    risk_level: RiskLevel,
    confidence: float,
) -> Decision:
    """Map (mode, privacy, risk) to a deterministic :class:`Decision`.

    Risk-tolerance is owner-chosen per chat mode (owner policy, 2026-07-04):

    * MANUAL   — always drafts; the owner sends every message by hand.
    * SEMI_AUTO — auto-sends LOW/MEDIUM risk; holds HIGH for a draft.
    * AUTO     — auto-sends LOW/MEDIUM/HIGH risk.

    CRITICAL is not blocked but *handled*: sensitive personal data is redacted
    upstream (Go ingestion) before it ever reaches here, so the only critical
    signal that survives is sexual content, which auto-replies with a gentle
    neutral redirect (see :func:`ai_agent.pipelines.draft.safety_directive_for`).
    PRIVATE chats and MANUAL mode never auto-reply. ``confidence`` is no longer a
    gate — it is reported for observability only.
    """
    if mode == ChatMode.DISABLED:
        return Decision.IGNORE
    if mode == ChatMode.SILENT:
        return Decision.OBSERVE
    if mode == ChatMode.MANUAL or PrivacyFlag.PRIVATE in privacy_flags:
        return Decision.DRAFT
    # CRITICAL (post-redaction: sexual content) auto-replies with a neutral
    # deflection — safe to send unsupervised because the reply de-escalates
    # rather than engages. MANUAL/PRIVATE already returned a draft above.
    if risk_level == RiskLevel.CRITICAL:
        return Decision.AUTO_REPLY
    if mode == ChatMode.AUTO:
        return Decision.AUTO_REPLY
    if mode == ChatMode.SEMI_AUTO:
        if risk_level in (RiskLevel.LOW, RiskLevel.MEDIUM):
            return Decision.AUTO_REPLY
        return Decision.DRAFT
    return Decision.DRAFT


def confidence_for(risk_level: RiskLevel, intent_confidence: float) -> float:
    """Combine risk and intent confidence into a single 0–1 score.

    Synthetic fallback used when the LLM did not return a valid self-assessed
    confidence (see :func:`combined_confidence`).  Higher risk levels cap the
    achievable confidence so the policy in :func:`decide` can never auto-reply
    on uncertain risky messages.
    """
    if risk_level == RiskLevel.CRITICAL:
        return _RISK_CAPS[RiskLevel.CRITICAL]
    if risk_level == RiskLevel.HIGH:
        return _RISK_CAPS[RiskLevel.HIGH]
    if risk_level == RiskLevel.MEDIUM:
        return min(_RISK_CAPS[RiskLevel.MEDIUM], intent_confidence)
    return min(_RISK_CAPS[RiskLevel.LOW], intent_confidence + 0.1)


def combined_confidence(llm_confidence: float, risk_level: RiskLevel) -> float:
    """Combine the LLM self-assessed confidence with the deterministic risk cap.

    ``final = min(llm_confidence, risk_cap)`` — the model can lower confidence
    below the cap (e.g. when unsure how to answer) but never exceed it, keeping
    risk policy on the deterministic classifier (charter: policy is backend's).
    """
    return min(max(llm_confidence, 0.0), _RISK_CAPS.get(risk_level, _RISK_CAPS[RiskLevel.LOW]))
