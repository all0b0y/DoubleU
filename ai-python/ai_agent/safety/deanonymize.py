"""Deanonymize LLM output by replacing placeholders with original PII.

Round-trips with :mod:`ai_agent.safety.regex_fallback.anonymize`: every
placeholder the input scanner emitted (``<EMAIL_1>``, ``<PHONE_2>`` …) is
substituted back when present in the LLM response.  The output also gets a
hallucination sanity check: a placeholder that was NOT in the redaction map
indicates the model fabricated an index, which is blocked.
"""

from __future__ import annotations

import re


# All scanner tags emitted by regex_fallback.anonymize / detect_secrets.
_PLACEHOLDER_RE = re.compile(
    r"<(?:EMAIL|PHONE|IBAN|IP|CARD|OPENAI_KEY|AWS_KEY|GITHUB_TOKEN|JWT|BEARER)_\d+>"
)


class HallucinatedPlaceholderError(ValueError):
    """Raised when LLM output references a placeholder not in the redaction map."""

    def __init__(self, placeholder: str) -> None:
        super().__init__(f"hallucinated placeholder: {placeholder}")
        self.placeholder = placeholder


def deanonymize(text: str, redactions: dict[str, str]) -> str:
    """Replace each placeholder in ``text`` with its original value.

    Raises :class:`HallucinatedPlaceholderError` if ``text`` contains a
    placeholder that does not appear in ``redactions`` — that is a sign the
    model fabricated a tag and the output should be blocked.
    """
    def _sub(match: re.Match[str]) -> str:
        placeholder = match.group(0)
        if placeholder not in redactions:
            raise HallucinatedPlaceholderError(placeholder)
        return redactions[placeholder]

    return _PLACEHOLDER_RE.sub(_sub, text)
