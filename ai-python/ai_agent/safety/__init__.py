"""Input and output safety layers around the LLM gateway."""

from .results import ScanResult, SafetyBlocked

__all__ = ["ScanResult", "SafetyBlocked"]
