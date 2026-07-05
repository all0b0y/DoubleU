"""HTTP endpoint routing tests for the stdlib AI service server."""

from __future__ import annotations

import unittest
from unittest import mock

from ai_agent.http_server import _build_model_config, make_handler
from ai_agent.observability.trace import derive_trace_id
from ai_agent.service import AIService

from agent_helpers import make_test_runtime


class HTTPServerTests(unittest.TestCase):
    """Verify HTTP route behavior without opening sandbox-blocked sockets."""

    def setUp(self) -> None:
        """Create a bound handler instance without invoking socket setup."""
        handler_class = make_handler(AIService(make_test_runtime()), True)
        self.handler = object.__new__(handler_class)

    def test_health_endpoint(self) -> None:
        """The bound handler should expose a liveness service."""
        response = self.handler.service.health()

        self.assertEqual(response, {"status": "ok"})

    def test_mcp_check_rejects_private_url(self) -> None:
        """Э1.3: a private/internal URL is refused before any network call."""
        response = self.handler._mcp_check({"url": "https://10.0.0.5/mcp", "transport": "http"})

        self.assertEqual(response, {"ok": False, "error": "url_not_allowed"})

    def test_mcp_check_lists_tools_on_success(self) -> None:
        """Э1.3: a reachable server yields ok:true with its tool names."""
        async def fake_list(spec: object) -> list[dict[str, str]]:
            return [{"name": "search", "description": "find things"}]

        with mock.patch("ai_agent.agents.runtime._list_mcp_tools", new=fake_list):
            response = self.handler._mcp_check(
                {"url": "https://93.184.216.34/mcp", "transport": "http"}
            )

        self.assertTrue(response["ok"])
        self.assertEqual(response["tools"], [{"name": "search", "description": "find things"}])

    def test_mcp_check_failure_hides_secrets_and_traces(self) -> None:
        """Э1.3: a connection failure becomes ok:false with only the error class —
        never a stack trace and never the auth header."""
        import json

        async def boom(spec: object) -> list[dict[str, str]]:
            raise ConnectionError("connect using Bearer sekret failed at line 42")

        with mock.patch("ai_agent.agents.runtime._list_mcp_tools", new=boom):
            response = self.handler._mcp_check(
                {"url": "https://93.184.216.34/mcp", "transport": "http", "auth_header": "Bearer sekret"}
            )

        self.assertFalse(response["ok"])
        self.assertEqual(response["error"], "ConnectionError")
        blob = json.dumps(response)
        self.assertNotIn("sekret", blob)
        self.assertNotIn("Traceback", blob)

    def test_classify_risk_endpoint(self) -> None:
        """The classify-risk route should return a risk response."""
        response = self.handler._classify_risk({"message_text": "Please send the invoice today."})

        self.assertEqual(response["risk_level"], "high")
        self.assertIn("financial_commitment", response["reasons"])

    def test_process_message_endpoint_matches_contract_shape(self) -> None:
        """The process-message route should return the frozen response fields."""
        response = self.handler._process_message(
            {
                "request_id": "req-1",
                "event": {
                    "event_id": "evt-1",
                    "tenant_id": "tenant-1",
                    "chat_id": "chat-1",
                    "telegram_update_id": 1,
                    "business_connection_id": "bc-1",
                    "source_message_id": "msg-1",
                    "sender_id": "sender-1",
                    "message_text": "Can we schedule a call today?",
                    "message_type": "text",
                    "created_at": "2026-05-13T10:00:00Z",
                },
                "chat_settings": {
                    "tenant_id": "tenant-1",
                    "chat_id": "chat-1",
                    "mode": "semi_auto",
                    "privacy_flags": [],
                },
                "memory_context": [],
            },
        )

        # Interaction tools add response_plan; it stays null for ordinary replies.
        self.assertEqual(
            set(response),
            {
                "decision", "response_text", "confidence", "risk_level", "intent",
                "text_format", "model_used", "usage", "memory_used", "flagged",
                "owner_note", "options", "response_plan", "scheduled_jobs",
                "cancelled_job_ids", "topic", "messages",
            },
        )
        self.assertEqual(response["decision"], "auto_reply")
        self.assertEqual(response["text_format"], "plain")
        self.assertFalse(response["flagged"])
        self.assertEqual(response["owner_note"], "")
        self.assertIsNone(response["response_plan"])
        self.assertEqual(response["scheduled_jobs"], [])


    def test_extract_media_endpoint_matches_shape(self) -> None:
        """The extract-media route parses the request and returns the frozen shape."""
        import base64

        response = self.handler._extract_media(
            {
                "kind": "voice",
                "data_base64": base64.b64encode(b"oggbytes").decode("ascii"),
                "mime_type": "audio/ogg",
            }
        )
        self.assertEqual(set(response), {"text", "provenance", "model_used"})
        self.assertEqual(response["provenance"], "voice_transcript")
        self.assertTrue(response["text"])

    def test_draft_role_endpoint_returns_structured_mock_draft(self) -> None:
        """The draft-role route returns the complete frozen wire shape in mock mode."""
        response = self.handler._draft_role(
            {"text": "Нужен менеджер поддержки: спокойно отвечает клиентам на русском."}
        )

        self.assertEqual(
            set(response),
            {
                "name",
                "description",
                "speaking_style",
                "goals",
                "constraints",
                "custom_fields",
            },
        )
        self.assertTrue(response["name"])
        self.assertTrue(response["description"])
        self.assertIsInstance(response["custom_fields"], list)

    def test_synthesize_pattern_endpoint_matches_contract_shape(self) -> None:
        """The synthesize-pattern route parses the request and returns the frozen shape."""
        response = self.handler._synthesize_pattern(
            {
                "cluster_facts": ["опять завал на работе", "устал", "не успеваю"],
                "tenant_id": "tenant-1",
                "chat_id": "chat-1",
                "privacy_flags": [],
            }
        )
        self.assertEqual(
            set(response),
            {"pattern", "subject", "confidence", "model_used", "usage", "blocked_by_privacy"},
        )
        self.assertFalse(response["blocked_by_privacy"])

    def test_draft_skill_endpoint_matches_contract_shape(self) -> None:
        """The draft-skill route parses the request and returns the frozen shape."""
        response = self.handler._draft_skill({"text": "Как отвечать на возражения по цене"})
        self.assertEqual(set(response), {"name", "description", "body"})
        self.assertTrue(response["name"])
        self.assertTrue(response["body"])

    def test_synthesize_pattern_respects_no_memory(self) -> None:
        """no_memory chats must never be reflected on."""
        response = self.handler._synthesize_pattern(
            {"cluster_facts": ["a", "b", "c"], "privacy_flags": ["no_memory"]}
        )
        self.assertTrue(response["blocked_by_privacy"])
        self.assertEqual(response["pattern"], "")


class ModelConfigTests(unittest.TestCase):
    """Tests for _build_model_config and the /ai/model-config GET endpoint."""

    def test_splits_comma_separated_models(self) -> None:
        """MODEL_TIER_* env vars are split on commas and whitespace-trimmed."""
        env = {
            "MODEL_TIER_FAST": "deepseek/fast-a, deepseek/fast-b",
            "MODEL_TIER_BALANCED": "deepseek/balanced",
            "MODEL_TIER_SMART": "deepseek/smart",
            "OPENROUTER_MODEL": "deepseek/default",
        }
        with mock.patch.dict("os.environ", env, clear=False):
            cfg = _build_model_config()

        self.assertEqual(cfg.fast, ["deepseek/fast-a", "deepseek/fast-b"])
        self.assertEqual(cfg.balanced, ["deepseek/balanced"])
        self.assertEqual(cfg.smart, ["deepseek/smart"])
        self.assertEqual(cfg.default, "deepseek/default")

    def test_empty_env_produces_empty_lists(self) -> None:
        """Unset tier env vars produce empty lists; unset OPENROUTER_MODEL produces empty string."""
        env = {
            "MODEL_TIER_FAST": "",
            "MODEL_TIER_BALANCED": "",
            "MODEL_TIER_SMART": "",
            "OPENROUTER_MODEL": "",
        }
        with mock.patch.dict("os.environ", env, clear=False):
            cfg = _build_model_config()

        self.assertEqual(cfg.fast, [])
        self.assertEqual(cfg.balanced, [])
        self.assertEqual(cfg.smart, [])
        self.assertEqual(cfg.default, "")

    def test_to_dict_matches_wire_shape(self) -> None:
        """to_dict must return exactly the four expected keys."""
        env = {
            "MODEL_TIER_FAST": "m/fast",
            "MODEL_TIER_BALANCED": "m/balanced",
            "MODEL_TIER_SMART": "m/smart",
            "OPENROUTER_MODEL": "m/default",
        }
        with mock.patch.dict("os.environ", env, clear=False):
            result = _build_model_config().to_dict()

        self.assertEqual(set(result.keys()), {"fast", "balanced", "smart", "default"})
        self.assertEqual(result["fast"], ["m/fast"])
        self.assertEqual(result["default"], "m/default")


class TraceIDPropagationTests(unittest.TestCase):
    """Phase 0: trace_id is derived from event_id and stamped on audit rows."""

    def setUp(self) -> None:
        handler_class = make_handler(AIService(make_test_runtime()), True)
        self.handler = object.__new__(handler_class)

    def test_audit_row_carries_derived_trace_id(self) -> None:
        """An /ai/process-message call must stamp the derived trace_id on the audit row."""
        event_id = "evt-phase0-1"
        self.handler._process_message(
            {
                "request_id": "req-phase0-1",
                "event": {
                    "event_id": event_id,
                    "tenant_id": "tenant-1",
                    "chat_id": "chat-1",
                    "telegram_update_id": 1,
                    "business_connection_id": "bc-1",
                    "source_message_id": "msg-1",
                    "sender_id": "sender-1",
                    "message_text": "Hello there",
                    "message_type": "text",
                    "created_at": "2026-05-13T10:00:00Z",
                },
                "chat_settings": {
                    "tenant_id": "tenant-1",
                    "chat_id": "chat-1",
                    "mode": "semi_auto",
                    "privacy_flags": [],
                },
                "memory_context": [],
            },
        )
        audits = self.handler.service.audit_events()
        self.assertTrue(audits, "expected at least one audit event")
        self.assertEqual(audits[-1].trace_id, derive_trace_id(event_id))


if __name__ == "__main__":
    unittest.main()
