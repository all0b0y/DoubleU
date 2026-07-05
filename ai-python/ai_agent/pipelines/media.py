"""Media extraction pipeline.

Turns an incoming Telegram attachment (voice, audio, photo, document) into plain
text so the agent stays text-only and the frozen ``/ai/process-message`` contract
is untouched (the "media -> text" design). Like :mod:`embedding`, this is a direct
provider call — not an LLM agent — so it lives in its own pipeline module and uses
the stdlib ``urllib`` transport with graceful degradation: any failure returns an
empty text with an "error"/"unsupported" provenance instead of raising, letting the
Go transport fall back to a placeholder rather than dropping the message.

Every networked call goes through **OpenRouter** (one key, one base URL):
  * voice/audio -> OpenRouter speech-to-text ``/audio/transcriptions`` with a JSON
    ``input_audio`` body (default model ``qwen/qwen3-asr-flash-2026-02-10``).
  * photo       -> OpenRouter ``/chat/completions`` with an image_url data URI
    (default model ``qwen/qwen3-vl-8b-instruct``).
  * document    -> local parsing (pdf/docx/text), no model, no network.

``MOCK_LLM=true`` short-circuits the two networked paths to deterministic text so
the endpoint is testable end-to-end without credentials; document parsing is
deterministic and always runs for real.
"""

from __future__ import annotations

import base64
import io
import json
import logging
import os
import urllib.request

from ..schemas import ExtractMediaRequest, ExtractMediaResponse


logger = logging.getLogger(__name__)

# Upper bound on extracted text so a long document/transcript cannot blow up the
# downstream LLM context. Truncation is marked so the model knows it is partial.
_MAX_TEXT_CHARS = 8000
_TRUNCATION_MARKER = "\n…[truncated]"

# Default OpenRouter models — overridable via STT_MODEL / VISION_MODEL env.
# Keep these pointing at slugs that currently have live OpenRouter endpoints:
# a retired slug returns 404 ("No endpoints found"), which degrades every photo/
# voice to an empty extraction and a "не удалось обработать" placeholder.
_DEFAULT_STT_MODEL = "qwen/qwen3-asr-flash-2026-02-10"
_DEFAULT_VISION_MODEL = "qwen/qwen3-vl-8b-instruct"

# Vision instruction: describe + verbatim OCR, language-agnostic so a Russian
# screenshot transcribes in Russian. Kept terse to minimise output tokens/cost.
_VISION_PROMPT = (
    "Describe this image concisely, then transcribe ALL visible text verbatim. "
    "If the image is mainly text (a screenshot, document or receipt), prioritise "
    "the exact text. Reply in the language of the text shown in the image."
)


def extract_media(request: ExtractMediaRequest) -> ExtractMediaResponse:
    """Convert one media attachment into plain text.

    Dispatches by ``request.kind``. Never raises on provider/parse failure: a
    degraded result carries empty ``text`` and a non-success ``provenance`` so the
    caller degrades gracefully.
    """
    try:
        data = base64.b64decode(request.data_base64, validate=True)
    except (ValueError, TypeError):
        logger.warning("extract_media: invalid base64 payload", extra={"kind": request.kind})
        return ExtractMediaResponse(text="", provenance="error", model_used="none")
    if not data:
        return ExtractMediaResponse(text="", provenance="error", model_used="none")

    if request.kind in ("voice", "audio"):
        text, model = _transcribe(data, request.mime_type, request.kind)
        return ExtractMediaResponse(
            text=_truncate(text), provenance="voice_transcript" if text else "error", model_used=model
        )
    if request.kind == "photo":
        text, model = _describe_image(data, request.mime_type)
        return ExtractMediaResponse(
            text=_truncate(text), provenance="image_caption" if text else "error", model_used=model
        )
    if request.kind == "document":
        text, provenance = _extract_document(data, request.mime_type)
        return ExtractMediaResponse(text=_truncate(text), provenance=provenance, model_used="none")

    return ExtractMediaResponse(text="", provenance="unsupported", model_used="none")


# --------------------------------------------------------------------------- #
# Voice / audio — speech to text (OpenRouter /audio/transcriptions)
# --------------------------------------------------------------------------- #

def _transcribe(data: bytes, mime_type: str, kind: str) -> tuple[str, str]:
    """Transcribe audio bytes to text via OpenRouter STT. Returns (text, model_used).

    OpenRouter's STT endpoint takes a JSON ``input_audio`` body (base64 + format),
    not a multipart upload, and returns ``{"text": ...}``. Note the provider caps
    audio at 10 MB / 5 min — oversized clips fail here and degrade to a placeholder
    upstream (a proper pre-check lands in Slice 3).
    """
    if _mock_enabled():
        return "mock voice transcript", "mock-stt"
    base_url, api_key, model, extra_headers = _resolve_openrouter("STT_MODEL", _DEFAULT_STT_MODEL)
    if not api_key:
        logger.warning("transcribe: no OPENROUTER_API_KEY configured")
        return "", "none"

    payload = {
        "model": model,
        "input_audio": {
            "data": base64.b64encode(data).decode("ascii"),
            "format": _audio_format(mime_type, kind),
        },
    }
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/audio/transcriptions",
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            **extra_headers,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            parsed = json.loads(resp.read())
        return str(parsed.get("text", "")).strip(), model
    except Exception as exc:  # noqa: BLE001 — degrade-safe, surfaced via log
        logger.warning("transcribe failed", extra={"model": model, "error": str(exc)})
        return "", "error"


def _audio_format(mime_type: str, kind: str) -> str:
    """Map a Telegram audio mime type to an OpenRouter ``input_audio.format`` token."""
    mime = (mime_type or "").lower()
    for needle, fmt in (("ogg", "ogg"), ("opus", "ogg"), ("mpeg", "mp3"),
                        ("mp3", "mp3"), ("mp4", "m4a"), ("m4a", "m4a"),
                        ("wav", "wav"), ("webm", "webm"), ("flac", "flac")):
        if needle in mime:
            return fmt
    # Telegram voice notes are OGG/OPUS; generic audio defaults to mp3.
    return "ogg" if kind == "voice" else "mp3"


# --------------------------------------------------------------------------- #
# Photo — vision describe + OCR (OpenRouter /chat/completions)
# --------------------------------------------------------------------------- #

def _describe_image(data: bytes, mime_type: str) -> tuple[str, str]:
    """Describe an image and transcribe its text via OpenRouter vision. Returns (text, model_used)."""
    if _mock_enabled():
        return "mock image description", "mock-vision"
    base_url, api_key, model, extra_headers = _resolve_openrouter("VISION_MODEL", _DEFAULT_VISION_MODEL)
    if not api_key:
        logger.warning("describe_image: no OPENROUTER_API_KEY configured")
        return "", "none"

    mime = mime_type or "image/jpeg"
    data_uri = f"data:{mime};base64,{base64.b64encode(data).decode('ascii')}"
    payload = {
        "model": model,
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": _VISION_PROMPT},
                    {"type": "image_url", "image_url": {"url": data_uri}},
                ],
            }
        ],
        "max_tokens": 1024,
    }
    req = urllib.request.Request(
        f"{base_url.rstrip('/')}/chat/completions",
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            **extra_headers,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            parsed = json.loads(resp.read())
        text = str(parsed["choices"][0]["message"]["content"]).strip()
        return text, model
    except Exception as exc:  # noqa: BLE001 — degrade-safe, surfaced via log
        logger.warning("describe_image failed", extra={"model": model, "error": str(exc)})
        return "", "error"


def _resolve_openrouter(model_env: str, default_model: str) -> tuple[str, str, str, dict[str, str]]:
    """Return (base_url, api_key, model, extra_headers) for an OpenRouter call.

    All media provider calls route through OpenRouter using the single
    ``OPENROUTER_API_KEY`` / ``OPENROUTER_BASE_URL`` config (mirrors the rest of
    the service); only the model differs per task and is env-overridable.
    """
    api_key = os.getenv("OPENROUTER_API_KEY", "").strip()
    base_url = os.getenv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1").strip()
    model = os.getenv(model_env, default_model).strip()
    extra: dict[str, str] = {}
    app_url = os.getenv("OPENROUTER_APP_URL", "").strip()
    app_name = os.getenv("OPENROUTER_APP_NAME", "").strip()
    if app_url:
        extra["HTTP-Referer"] = app_url
    if app_name:
        extra["X-Title"] = app_name
    return base_url, api_key, model, extra


# --------------------------------------------------------------------------- #
# Document — local text extraction (no model, no network)
# --------------------------------------------------------------------------- #

def _extract_document(data: bytes, mime_type: str) -> tuple[str, str]:
    """Extract text from a document. Returns (text, provenance).

    PDF and DOCX parsers are imported lazily and optional: when the library is
    absent the kind degrades to "unsupported" rather than crashing the service
    (mirrors the optional-dependency handling of the MCP toolset).
    """
    mime = (mime_type or "").lower()
    if "pdf" in mime:
        return _extract_pdf(data)
    if "wordprocessingml" in mime or mime == "application/msword":
        return _extract_docx(data)
    if mime.startswith("text/") or mime in ("application/json", "application/csv", ""):
        return data.decode("utf-8", errors="replace").strip(), "document_text"
    # Unknown binary type: last-resort UTF-8 sniff, else give up cleanly.
    try:
        decoded = data.decode("utf-8")
    except UnicodeDecodeError:
        return "", "unsupported"
    return decoded.strip(), "document_text"


def _extract_pdf(data: bytes) -> tuple[str, str]:
    try:
        from pdfminer.high_level import extract_text  # type: ignore[import-untyped]
    except ImportError:
        logger.warning("pdf extraction unavailable: pdfminer.six not installed")
        return "", "unsupported"
    try:
        text = extract_text(io.BytesIO(data)) or ""
    except Exception as exc:  # noqa: BLE001 — malformed PDF, degrade-safe
        logger.warning("pdf extraction failed", extra={"error": str(exc)})
        return "", "error"
    return text.strip(), "document_text"


def _extract_docx(data: bytes) -> tuple[str, str]:
    try:
        import docx  # type: ignore[import-untyped]
    except ImportError:
        logger.warning("docx extraction unavailable: python-docx not installed")
        return "", "unsupported"
    try:
        document = docx.Document(io.BytesIO(data))
        text = "\n".join(p.text for p in document.paragraphs)
    except Exception as exc:  # noqa: BLE001 — malformed DOCX, degrade-safe
        logger.warning("docx extraction failed", extra={"error": str(exc)})
        return "", "error"
    return text.strip(), "document_text"


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

def _mock_enabled() -> bool:
    """Mirror the agent layer's MOCK_LLM switch for the networked media paths."""
    return os.getenv("MOCK_LLM", "true").lower() in ("1", "true", "yes", "on")


def _truncate(text: str) -> str:
    """Cap extracted text length, marking truncation so the model knows it is partial."""
    text = text.strip()
    if len(text) <= _MAX_TEXT_CHARS:
        return text
    return text[:_MAX_TEXT_CHARS] + _TRUNCATION_MARKER
