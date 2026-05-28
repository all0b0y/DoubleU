"""Intent classification pipeline (thin wrapper over :mod:`classifiers`)."""

from __future__ import annotations

from ..classifiers import IntentClassifier
from ..schemas import IntentRequest, IntentResponse


def classify_intent(classifier: IntentClassifier, request: IntentRequest) -> IntentResponse:
    """Run :class:`IntentClassifier` against ``request.message_text``."""
    return classifier.classify(request.message_text)
