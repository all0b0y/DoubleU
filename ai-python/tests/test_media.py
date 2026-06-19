"""Media extraction pipeline tests.

Cover the deterministic paths only — voice/photo run under MOCK_LLM (no network)
and documents parse locally (no model). The optional pdf/docx libraries are NOT
exercised so the suite is green without them installed.
"""

from __future__ import annotations

import base64
import unittest

from ai_agent.pipelines import media
from ai_agent.schemas import ExtractMediaRequest, parse_extract_media_request


def _b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


class ExtractMediaTests(unittest.TestCase):
    """Behavioural tests for ai_agent.pipelines.media.extract_media."""

    def test_voice_returns_transcript(self) -> None:
        """Voice under mock yields a transcript provenance and non-empty text."""
        resp = media.extract_media(
            ExtractMediaRequest(kind="voice", data_base64=_b64(b"oggbytes"), mime_type="audio/ogg")
        )
        self.assertEqual(resp.provenance, "voice_transcript")
        self.assertTrue(resp.text)

    def test_photo_returns_caption(self) -> None:
        """Photo under mock yields an image-caption provenance and non-empty text."""
        resp = media.extract_media(
            ExtractMediaRequest(kind="photo", data_base64=_b64(b"jpegbytes"), mime_type="image/jpeg")
        )
        self.assertEqual(resp.provenance, "image_caption")
        self.assertTrue(resp.text)

    def test_document_plain_text_preserves_unicode(self) -> None:
        """A UTF-8 text document is decoded verbatim, including non-ASCII content."""
        resp = media.extract_media(
            ExtractMediaRequest(
                kind="document",
                data_base64=_b64("счёт на оплату".encode("utf-8")),
                mime_type="text/plain",
            )
        )
        self.assertEqual(resp.provenance, "document_text")
        self.assertIn("счёт", resp.text)

    def test_document_csv(self) -> None:
        """A CSV document is treated as text."""
        resp = media.extract_media(
            ExtractMediaRequest(kind="document", data_base64=_b64(b"a,b\n1,2\n"), mime_type="text/csv")
        )
        self.assertEqual(resp.provenance, "document_text")
        self.assertIn("a,b", resp.text)

    def test_unknown_kind_is_unsupported(self) -> None:
        """An out-of-scope kind (e.g. video) extracts nothing, never crashes."""
        resp = media.extract_media(ExtractMediaRequest(kind="video", data_base64=_b64(b"x")))
        self.assertEqual(resp.provenance, "unsupported")
        self.assertEqual(resp.text, "")

    def test_invalid_base64_degrades_to_error(self) -> None:
        """Malformed base64 degrades to an error result instead of raising."""
        resp = media.extract_media(ExtractMediaRequest(kind="voice", data_base64="!!!not base64!!!"))
        self.assertEqual(resp.provenance, "error")
        self.assertEqual(resp.text, "")

    def test_unknown_binary_document_unsupported(self) -> None:
        """A non-text binary with an unknown mime is reported unsupported."""
        resp = media.extract_media(
            ExtractMediaRequest(
                kind="document",
                data_base64=_b64(b"\xff\xfe\x00\x01"),
                mime_type="application/octet-stream",
            )
        )
        self.assertEqual(resp.provenance, "unsupported")

    def test_long_text_is_truncated(self) -> None:
        """Oversized extracted text is capped with a truncation marker."""
        big = "x" * (media._MAX_TEXT_CHARS + 100)
        resp = media.extract_media(
            ExtractMediaRequest(kind="document", data_base64=_b64(big.encode()), mime_type="text/plain")
        )
        self.assertTrue(resp.text.endswith(media._TRUNCATION_MARKER))
        self.assertLessEqual(len(resp.text), media._MAX_TEXT_CHARS + len(media._TRUNCATION_MARKER))


class ParseExtractMediaTests(unittest.TestCase):
    """Boundary validation for parse_extract_media_request."""

    def test_kind_required(self) -> None:
        with self.assertRaises(ValueError):
            parse_extract_media_request({"data_base64": "QQ=="})

    def test_data_required(self) -> None:
        with self.assertRaises(ValueError):
            parse_extract_media_request({"kind": "voice"})

    def test_parses_optional_fields(self) -> None:
        req = parse_extract_media_request(
            {"kind": "voice", "data_base64": "QQ==", "mime_type": "audio/ogg", "duration": 5}
        )
        self.assertEqual(req.kind, "voice")
        self.assertEqual(req.duration, 5)


if __name__ == "__main__":
    unittest.main()
