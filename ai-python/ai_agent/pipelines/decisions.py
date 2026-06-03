"""Pure policy decision helpers used by the orchestration service."""

from __future__ import annotations

from ..schemas import ChatMode, Decision, PrivacyFlag, RiskLevel


# Confidence thresholds gating auto-reply per chat mode.  Kept here so the
# policy is reviewable in one place rather than scattered through the service.
SEMI_AUTO_MIN = 0.85
AUTO_MIN = 0.92
#TODO

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
    if mode == ChatMode.SEMI_AUTO and confidence >= SEMI_AUTO_MIN:
        return Decision.AUTO_REPLY
    if mode == ChatMode.AUTO and confidence >= AUTO_MIN:
        return Decision.AUTO_REPLY
    return Decision.DRAFT


def confidence_for(risk_level: RiskLevel, intent_confidence: float) -> float:
    """Combine risk and intent confidence into a single 0–1 score.

    Higher risk levels cap the achievable confidence so the policy in
    :func:`decide` can never auto-reply on uncertain risky messages.
    """
    if risk_level == RiskLevel.CRITICAL:
        return 0.35
    if risk_level == RiskLevel.HIGH:
        return 0.55
    if risk_level == RiskLevel.MEDIUM:
        return min(0.84, intent_confidence)
    return min(0.96, intent_confidence + 0.1)
