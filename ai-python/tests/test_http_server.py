"""HTTP endpoint routing tests for the stdlib AI service server."""

from __future__ import annotations

import unittest

from ai_agent.http_server import make_handler
from ai_agent.llm import MockLLMGateway, ModelRouter
from ai_agent.service import AIService


class HTTPServerTests(unittest.TestCase):
    """Verify HTTP route behavior without opening sandbox-blocked sockets."""

    def setUp(self) -> None:
        """Create a bound handler instance without invoking socket setup."""
        handler_class = make_handler(AIService(ModelRouter(MockLLMGateway())), True)
        self.handler = object.__new__(handler_class)

    def test_health_endpoint(self) -> None:
        """The bound handler should expose a liveness service."""
        response = self.handler.service.health()

        self.assertEqual(response, {"status": "ok"})

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

        self.assertEqual(
            set(response),
            {"decision", "response_text", "confidence", "risk_level", "intent", "model_used", "usage", "memory_used"},
        )
        self.assertEqual(response["decision"], "auto_reply")


if __name__ == "__main__":
    unittest.main()
