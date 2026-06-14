"""Deterministic agent-runtime builders shared by the service/HTTP/safety tests.

The pydantic-ai ``FunctionModel`` is the agent-path equivalent of the old
``MockLLMGateway``: it returns fixed structured output so the service's policy
and routing behaviour can be asserted without a real provider.  ``branching_draft_model``
inspects the user message and mimics the legacy mock's keyword routing so the
message-sensitive contract tests (intent / choice_question / confidence) stay
meaningful.
"""

from __future__ import annotations

from pydantic_ai.messages import ModelResponse, ToolCallPart, UserPromptPart
from pydantic_ai.models.function import AgentInfo, FunctionModel

from ai_agent.agents.runtime import AgentRuntime


def structured(tool_args: dict) -> FunctionModel:
    """A model that always answers with the given structured output."""

    def respond(messages, info: AgentInfo) -> ModelResponse:
        return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args=tool_args)])

    return FunctionModel(respond)


def _last_user_text(messages) -> str:
    """Concatenate the user-prompt text across the request messages."""
    texts: list[str] = []
    for message in messages:
        for part in getattr(message, "parts", []):
            if isinstance(part, UserPromptPart):
                content = part.content
                texts.append(content if isinstance(content, str) else " ".join(map(str, content)))
    return "\n".join(texts).lower()


def branching_draft_model() -> FunctionModel:
    """Draft model that routes on the message like the legacy mock gateway did."""

    def respond(messages, info: AgentInfo) -> ModelResponse:
        text = _last_user_text(messages)
        reply, intent, options, confidence, topic = (
            "Thanks, I'll get back to you shortly.", "general_message", [], 0.8, "general chat",
        )
        if " or " in text and "?" in text:
            reply, intent, options, confidence, topic = (
                "Let me think about the options.", "choice_question",
                ["Monday", "Tuesday"], 0.7, "choosing a day",
            )
        elif "?" in text:
            reply, intent, confidence, topic = (
                "Sure, let me check and reply shortly.", "question", 0.95, "a question",
            )
        elif any(k in text for k in ("meeting", "call", "schedule", "встрет", "созвон")):
            reply, intent, confidence, topic = (
                "Sounds good, that works for me.", "scheduling", 0.95, "scheduling",
            )
        return ModelResponse(parts=[ToolCallPart(tool_name="final_result", args={
            "reply": reply, "intent": intent, "options": options,
            "confidence": confidence, "topic": topic,
        })])

    return FunctionModel(respond)


def make_test_runtime(**overrides) -> AgentRuntime:
    """Build an AgentRuntime with deterministic models for service-level tests.

    ``overrides`` may replace any model (``draft_model`` …) or pass the
    ``input_safety`` / ``output_safety`` / ``sink`` dependencies.
    """
    defaults = {
        "draft_model": branching_draft_model(),
        "critic_model": structured({"verdict": "approve", "issues": [], "revised_reply": ""}),
        "extract_model": structured({"facts": [], "profile_suggestions": []}),
        "summary_model": structured(
            {"summary": "Обсудили договор и счёт.",
             "action_items": ["прислать счёт до пятницы"], "recommendations": []}
        ),
        "topic_model": structured({"topic": "general chat"}),
        "rerank_model": structured({"ranked_ids": []}),
    }
    defaults.update(overrides)
    return AgentRuntime(run_timeout_seconds=10, **defaults)
