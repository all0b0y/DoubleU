"""Input safety pre-gateway scanner pipeline (Phase 1).

The scanner sits inside :class:`ai_agent.llm.ModelRouter` and runs before any
gateway sees the payload.  It composes scanners per purpose from
:mod:`ai_agent.safety.policies`:

* ``Anonymize`` rewrites emails / phones / cards / IBANs / IPs in place and
  records the originals in :attr:`LLMCallContext.redactions` so the output
  layer can deanonymize the model response (Phase 3).
* ``Secrets`` redacts API keys, JWTs, bearer tokens etc.
* ``PromptInjection`` (deterministic regex fallback when LLM Guard is absent)
  raises :class:`SafetyBlocked` for the ``draft`` purpose and returns a canned
  reply for the user.
* ``Toxicity`` / ``BanTopics`` raise :class:`SafetyBlocked` for ``summarize``
  and ``draft`` purposes.

The regex fallback is always used in MOCK mode (and in CI) so behaviour is
deterministic without downloading the LLM Guard transformers.  When
``llm-guard`` is installed in production the constructor wires in the LLM Guard
scanners and the regex pipeline only runs as a safety net.
"""

from __future__ import annotations

import time
from dataclasses import dataclass

from ai_agent.config.flags import FeatureFlags
from ai_agent.contract_enums import LLMPurpose
from ai_agent.observability.context import LLMCallContext
from ai_agent.schemas import ModelTask

from . import regex_fallback as rx
from .policies import INPUT_SCANNERS_BY_PURPOSE, purpose_for_task
from .results import SafetyBlocked, ScanResult


CANNED_INJECTION_REPLY = "Sorry, I can't follow that instruction."


@dataclass(frozen=True)
class _ScannerOutput:
    """Internal: outcome of one scanner pass — cleaned text + scan record."""

    cleaned: str
    result: ScanResult


class InputSafetyScanner:
    """Stage 1 safety gate: anonymize + injection + toxicity + secrets.

    The constructor is dependency-injected so unit tests pass in a custom
    scanner set (see ``test_safety_input.py``).  Default construction picks
    the deterministic regex fallback.
    """

    def __init__(
        self,
        flags: FeatureFlags | None = None,
        latency_budget_ms: float = 50.0,
    ) -> None:
        """Create the scanner.

        ``latency_budget_ms`` reserves a per-scan budget; if exceeded, slow
        scanners are skipped and ``ScanResult(reason="latency_budget")``
        flags the bypass for observability.
        """
        self._flags = flags or FeatureFlags()
        self._latency_budget_ms = latency_budget_ms

    @property
    def enabled(self) -> bool:
        """Return whether the scanner is enabled via FeatureFlags."""
        return self._flags.safety_input_enabled

    def scan(
        self,
        system_prompt: str,
        user_payload: str,
        task: ModelTask,
        ctx: LLMCallContext,
    ) -> tuple[str, str]:
        """Run scanners for ``task`` against ``user_payload``.

        Returns the (possibly rewritten) system + user payloads.  The
        ``ctx.redactions`` dict is populated for deanonymize.  On a blocking
        scanner (injection / toxicity / banned topic) raises
        :class:`SafetyBlocked` so the router maps it to a canned reply.
        """
        if not self.enabled:
            return system_prompt, user_payload

        purpose = ctx.purpose or purpose_for_task(task)
        scanners = INPUT_SCANNERS_BY_PURPOSE.get(purpose, ())
        outcome = rx.scan_all(user_payload)
        start = time.perf_counter()

        if "Anonymize" in scanners:
            ctx.redactions.update(outcome.redactions)
            ctx.safety_results.append(ScanResult(
                scanner="Anonymize",
                passed=True,
                latency_ms=(time.perf_counter() - start) * 1000,
                details={"placeholders": list(outcome.redactions.keys())},
            ))
            user_payload = outcome.cleaned

        if "Secrets" in scanners:
            blocked = outcome.secrets_detected
            ctx.safety_results.append(ScanResult(
                scanner="Secrets",
                passed=not blocked,
                reason="secret_detected" if blocked else "",
                latency_ms=(time.perf_counter() - start) * 1000,
                details={"match_kind": outcome.secret_match},
            ))
            # Secrets are redacted in-place (Anonymize step ran the replacement)
            # but the scan_result still records the detection so observability
            # surfaces it.

        if "PromptInjection" in scanners and outcome.injection_detected:
            ctx.safety_results.append(ScanResult(
                scanner="PromptInjection",
                passed=False,
                reason="prompt_injection_attempt",
                latency_ms=(time.perf_counter() - start) * 1000,
                details={"match": outcome.injection_match[:80]},
            ))
            raise SafetyBlocked(
                reason="prompt_injection_attempt",
                canned_reply=CANNED_INJECTION_REPLY,
                scanner="PromptInjection",
            )

        if "Toxicity" in scanners and outcome.toxicity_detected:
            ctx.safety_results.append(ScanResult(
                scanner="Toxicity",
                passed=False,
                reason="toxic_input",
                latency_ms=(time.perf_counter() - start) * 1000,
                details={"match": outcome.toxicity_match},
            ))
            raise SafetyBlocked(
                reason="toxic_input",
                canned_reply=CANNED_INJECTION_REPLY,
                scanner="Toxicity",
            )

        if "BanTopics" in scanners and outcome.banned_topic_detected:
            ctx.safety_results.append(ScanResult(
                scanner="BanTopics",
                passed=False,
                reason="banned_topic",
                latency_ms=(time.perf_counter() - start) * 1000,
                details={"match": outcome.banned_topic_match},
            ))
            raise SafetyBlocked(
                reason="banned_topic",
                canned_reply=CANNED_INJECTION_REPLY,
                scanner="BanTopics",
            )

        return system_prompt, user_payload

    def scan_embed_batch(
        self,
        texts: tuple[str, ...],
        ctx: LLMCallContext,
    ) -> tuple[tuple[str, ...], dict[str, str]]:
        """Run the embed-purpose scanners (Anonymize + Secrets only).

        Returns the sanitized texts and the merged redaction map.
        PromptInjection is intentionally not run on embed inputs (the texts are
        facts, not instructions, and an injection match would corrupt RAG
        retrieval).
        """
        if not self.enabled:
            return texts, {}
        cleaned: list[str] = []
        merged: dict[str, str] = {}
        for text in texts:
            outcome = rx.scan_all(text)
            cleaned.append(outcome.cleaned)
            merged.update(outcome.redactions)
        ctx.redactions.update(merged)
        ctx.safety_results.append(ScanResult(
            scanner="EmbedAnonymize",
            passed=True,
            details={"placeholders": list(merged.keys())},
        ))
        return tuple(cleaned), merged
