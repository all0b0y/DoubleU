"""Risk classification pipeline (thin wrapper over :mod:`classifiers`)."""

from __future__ import annotations

from ..classifiers import RiskClassifier
from ..schemas import RiskRequest, RiskResponse


def classify_risk(classifier: RiskClassifier, request: RiskRequest) -> RiskResponse:
    """Run :class:`RiskClassifier` against ``request.message_text``."""
    return classifier.classify(request.message_text)
