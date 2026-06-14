"""Output safety scanner pipeline (Phase 3).

Invoked by :class:`ai_agent.agents.runtime.AgentRuntime` after an agent run
returns.  Per purpose (see :mod:`ai_agent.safety.policies`) it:

* ``Deanonymize`` — substitutes placeholders back to their original values
  using :attr:`LLMCallContext.redactions`.  Hallucinated placeholders trigger
  a block.
* ``Toxicity`` / ``Sensitive`` — keyword + regex sanity checks (LLM Guard
  models swap in here in production).  For ``draft`` purpose, toxicity
  becomes ``flagged=True`` (draft is still returned, owner UI annotates it
  with ⚠).
* ``JSON`` — for ``extract`` / ``classify`` outputs, ensures the response
  parses as JSON.  One repair attempt is delegated to a cheap gateway; the
  ``repair_gateway`` callable receives the malformed text and returns a
  hopefully valid one.
"""

from __future__ import annotations

import json
import re
import time
from typing import Callable

from ai_agent.config.flags import FeatureFlags
from ai_agent.contract_enums import LLMPurpose
from ai_agent.observability.context import LLMCallContext
from ai_agent.schemas import ModelTask

from .deanonymize import HallucinatedPlaceholderError, deanonymize
from .policies import OUTPUT_SCANNERS_BY_PURPOSE, purpose_for_task
from .results import ScanResult


# Conservative keyword sets — LLM Guard's transformer models replace these in
# production when ``safety_output_enabled=True``.
_TOXIC_OUTPUT_KEYWORDS: tuple[str, ...] = (
    "kill yourself", "you are stupid", "shut up",
    "идиот", "придурок",
)

_SENSITIVE_OUTPUT_RE = re.compile(
    r"\b(?:[\w.+-]+@[\w-]+\.[\w.-]+|sk-[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,})\b"
)


RepairGateway = Callable[[str], str]


class OutputSafetyScanner:
    """Phase 3 output gate.

    ``repair_gateway`` is called at most once per response when JSON validation
    fails; if the repair still produces invalid JSON, the scanner gives up and
    flags the response.
    """

    def __init__(
        self,
        flags: FeatureFlags | None = None,
        repair_gateway: RepairGateway | None = None,
    ) -> None:
        """Create the output scanner."""
        self._flags = flags or FeatureFlags()
        self._repair_gateway = repair_gateway

    @property
    def enabled(self) -> bool:
        """Return whether the scanner is enabled via FeatureFlags."""
        return self._flags.safety_output_enabled

    def scan(
        self,
        text: str,
        task: ModelTask,
        ctx: LLMCallContext,
    ) -> tuple[str, bool]:
        """Run scanners for ``task`` against ``text``.

        Returns ``(possibly_rewritten_text, flagged)``.  ``flagged=True`` is a
        non-blocking signal the caller surfaces in the bot UI.  Blocking
        cases (hallucinated placeholder, malformed JSON after repair) yield an
        empty string + ``flagged=True``.
        """
        if not self.enabled:
            return text, False

        purpose = ctx.purpose or purpose_for_task(task)
        scanners = OUTPUT_SCANNERS_BY_PURPOSE.get(purpose, ())
        flagged = False
        start = time.perf_counter()

        if "Toxicity" in scanners and self._toxic(text):
            ctx.safety_results.append(ScanResult(
                scanner="OutputToxicity",
                passed=False,
                reason="toxic_output",
                latency_ms=(time.perf_counter() - start) * 1000,
            ))
            flagged = True

        if "Sensitive" in scanners and self._sensitive(text):
            ctx.safety_results.append(ScanResult(
                scanner="OutputSensitive",
                passed=False,
                reason="sensitive_leak",
                latency_ms=(time.perf_counter() - start) * 1000,
            ))
            flagged = True

        if "JSON" in scanners:
            text, json_flagged = self._validate_json(text, ctx, start)
            flagged = flagged or json_flagged
            if json_flagged and not text:
                # Repair failed.  Return an empty payload but keep flagged.
                return "", True

        if "Deanonymize" in scanners and ctx.redactions:
            try:
                text = deanonymize(text, ctx.redactions)
                ctx.safety_results.append(ScanResult(
                    scanner="Deanonymize",
                    passed=True,
                    latency_ms=(time.perf_counter() - start) * 1000,
                ))
            except HallucinatedPlaceholderError as exc:
                ctx.safety_results.append(ScanResult(
                    scanner="Deanonymize",
                    passed=False,
                    reason="hallucinated_placeholder",
                    latency_ms=(time.perf_counter() - start) * 1000,
                    details={"placeholder": exc.placeholder},
                ))
                ctx.output_flagged = True
                return "", True

        if flagged:
            ctx.output_flagged = True
        return text, flagged

    def _toxic(self, text: str) -> bool:
        lowered = text.lower()
        return any(kw in lowered for kw in _TOXIC_OUTPUT_KEYWORDS)

    def _sensitive(self, text: str) -> bool:
        return bool(_SENSITIVE_OUTPUT_RE.search(text))

    def _validate_json(
        self,
        text: str,
        ctx: LLMCallContext,
        start: float,
    ) -> tuple[str, bool]:
        """Try to parse text as JSON; on failure run one repair attempt."""
        try:
            json.loads(text)
            ctx.safety_results.append(ScanResult(
                scanner="JSON",
                passed=True,
                latency_ms=(time.perf_counter() - start) * 1000,
            ))
            return text, False
        except (ValueError, TypeError):
            pass

        if self._repair_gateway is None:
            ctx.safety_results.append(ScanResult(
                scanner="JSON",
                passed=False,
                reason="invalid_json_no_repair",
                latency_ms=(time.perf_counter() - start) * 1000,
            ))
            return "", True

        try:
            repaired = self._repair_gateway(text)
            json.loads(repaired)
            ctx.safety_results.append(ScanResult(
                scanner="JSON",
                passed=True,
                reason="repaired",
                latency_ms=(time.perf_counter() - start) * 1000,
            ))
            return repaired, False
        except (ValueError, TypeError, RuntimeError):
            ctx.safety_results.append(ScanResult(
                scanner="JSON",
                passed=False,
                reason="invalid_json_after_repair",
                latency_ms=(time.perf_counter() - start) * 1000,
            ))
            return "", True
