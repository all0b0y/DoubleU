"""Pure policy decision helpers used by the orchestration service."""

from __future__ import annotations

from ..schemas import ChatMode, Decision, PrivacyFlag, RiskLevel


# Confidence threshold gating auto-reply in SEMI_AUTO.  Kept here so the policy
# is reviewable in one place rather than scattered through the service.  AUTO has
# no threshold by design: the owner opting into auto mode accepts unsupervised
# replies (owner decision, 2026-06-16), so only the deterministic risk gates and
# the MANUAL/PRIVATE blocks above guard it — see :func:`decide`.
SEMI_AUTO_MIN = 0.85

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
    """Map (mode, privacy, risk, confidence) to a deterministic :class:`Decision`.

    Critical risk always escalates to the owner; medium/high always drafts.
    PRIVATE chats and MANUAL mode never auto-reply even at high confidence.
    """
    if mode == ChatMode.DISABLED:
        return Decision.IGNORE
    if mode == ChatMode.SILENT:
        return Decision.OBSERVE
    if risk_level == RiskLevel.CRITICAL:
        return Decision.ASK_OWNER
    if risk_level in (RiskLevel.MEDIUM, RiskLevel.HIGH):
        return Decision.DRAFT
    if mode == ChatMode.MANUAL or PrivacyFlag.PRIVATE in privacy_flags:
        return Decision.DRAFT
    # AUTO mode: the owner explicitly opts into unsupervised replies and accepts
    # the risk, so there is no confidence gate — every low-risk message
    # auto-sends (owner decision, 2026-06-16). MEDIUM/HIGH/CRITICAL risk already
    # returned a draft/ask_owner above, so only low risk reaches here. SEMI_AUTO
    # keeps the confidence threshold: send only when the model is confident.
    if mode == ChatMode.AUTO:
        return Decision.AUTO_REPLY
    if mode == ChatMode.SEMI_AUTO and confidence >= SEMI_AUTO_MIN:
        return Decision.AUTO_REPLY
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
